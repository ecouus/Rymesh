package console

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Imported sessions used to be a one-time snapshot: the native transcript was
// read when the service started, and anything written afterwards (for example a
// conversation continued in the CLI) never appeared in the console. The console
// then showed a session whose last answer was permanently missing, no matter
// how long the page waited. SyncSession closes that gap by reading only the new
// tail of the native file whenever a session is opened.

const importTaskPrefix = "import-"

func importTaskID(sessionID string) string { return importTaskPrefix + sessionID }

// SyncSession brings a session up to date with its native transcript. That
// covers sessions started in the console too: the same native session can be
// continued with `claude --resume` / `codex resume` in a terminal, and those
// turns must show up here as well. It is cheap when nothing changed (a single
// stat) and only reads the bytes written since the previous sync otherwise.
func (im *Importer) SyncSession(sessionID string) error {
	// The page fetches session detail and the first events page in parallel, so
	// two syncs for the same session race by design. Everything that decides
	// what to read must happen under syncMu: reading the cursor before locking
	// let the second caller reuse a stale offset and append the same tail again
	// (each append gets fresh task_seq values, so the unique index never dedupes).
	im.syncMu.Lock()
	defer im.syncMu.Unlock()

	sess, err := im.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if sess.NativeID == "" {
		return nil
	}
	state := parseImportState(sess.Meta)
	path := state.Path
	if path == "" || !fileExists(path) {
		path = im.findNativeFile(sess.Agent, sess.NativeID)
	}
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil
	}
	ino, mtime := fileStamp(fi)
	// Legacy snapshots (no cursor) and files replaced by a different one need a
	// full parse; otherwise only the tail is read. Identity is compared as well
	// as size: a rewrite in place can keep the size and would otherwise be read
	// from a stale offset, mixing two conversations.
	replaced := state.Ino != 0 && ino != 0 && state.Ino != ino
	if state.Path != path || state.Offset <= 0 || fi.Size() < state.Offset || replaced {
		// A console-created session has no imported snapshot: everything in
		// its transcript outside the console's own task windows was written by
		// the CLI directly, so the first sync reads it from the start rather
		// than jumping the cursor to the end as a snapshot session does.
		if sess.Source == "web" && state.Offset <= 0 {
			return im.appendTail(sess, path, ImportState{}, fi.Size(), ino, mtime)
		}
		return im.reimportFull(sess, path, fi.Size())
	}
	if fi.Size() == state.Offset {
		return nil
	}
	return im.appendTail(sess, path, state, fi.Size(), ino, mtime)
}

func (im *Importer) appendTail(sess *Session, path string, state ImportState, size int64, ino uint64, mtime int64) error {
	// The native file may have been appended mid-record; only complete lines are
	// consumed so the next sync starts on a record boundary.
	events, offset, ps, err := im.parseTail(sess.Agent, path, state.Offset)
	if err != nil {
		return err
	}
	taskID := importTaskID(sess.ID)
	// A console-run turn is ingested from the task log; the same records are in
	// the native transcript. Skip those windows so the turn is not shown twice.
	if len(events) > 0 {
		windows, _ := im.store.ConsoleTaskWindows(sess.ID)
		events = filterTaskWindows(events, windows)
	}
	// The session timestamp never goes backwards: a tail appended while a
	// console task is still running can end before that task's last event.
	updated := laterTS(state.Updated, sess.LastMessageAt)
	for _, e := range events {
		updated = laterTS(updated, e.TS)
	}
	if updated == "" {
		updated = nowRFC3339()
	}
	seq, err := im.store.LastImportedSeq(sess.ID, taskID)
	if err != nil {
		return err
	}
	title := ""
	if ps != nil && len(events) > 0 && (ps.titleCand != "" || ps.aiTitle != "" || ps.firstUser != "") {
		// Only upgrade a placeholder title, and only when the tail actually
		// contains a better one: a user rename must survive every sync.
		if looksAutoTitle(sess.Title) {
			title = ps.title()
		}
	}
	if title == "" && sess.Agent == "codex" && looksAutoTitle(sess.Title) {
		// Codex names a thread in session_index.jsonl only when the turn ends,
		// so a session opened mid-turn keeps its placeholder. Upgrade it once
		// the name exists; a user rename is never touched.
		if t := im.codexTitles()[sess.NativeID]; t != "" && t != sess.Title {
			title = truncateTitle(t, 120)
		}
	}
	next := ImportState{Path: path, Offset: offset, Size: size, Seq: state.Seq, Updated: updated,
		Ino: ino, MTime: mtime}
	nextSeq, err := im.store.AppendImportedEvents(sess.ID, taskID, seq, events, updated, title, next)
	if err != nil {
		return err
	}
	next.Seq = nextSeq
	return im.store.SetImportState(sess.ID, next)
}

func (im *Importer) reimportFull(sess *Session, path string, size int64) error {
	var events []impEvent
	var title, updated string
	// Console-produced events live in the same session and must keep their
	// chronological position. Replacing the snapshot would renumber it after
	// them, so only record the cursor and leave the existing history intact.
	var ino uint64
	var mtime int64
	if fi, err := os.Stat(path); err == nil {
		ino, mtime = fileStamp(fi)
	}
	if has, _ := im.store.HasNonImportEvents(sess.ID, importTaskID(sess.ID)); has {
		return im.store.SetImportState(sess.ID, ImportState{
			Path: path, Offset: size, Size: size, Updated: sess.LastMessageAt, Ino: ino, MTime: mtime,
		})
	}
	switch sess.Agent {
	case "claude":
		ps := &claudeParse{sid: sess.NativeID, seen: map[string]bool{}}
		evs, last, err := im.parseClaudeFile(path, ps)
		if err != nil {
			return err
		}
		events, updated = evs, last
		if looksAutoTitle(sess.Title) {
			title = truncateTitle(ps.title(), 120)
		}
	case "codex":
		evs, last, err := im.parseCodexFile(path)
		if err != nil {
			return err
		}
		events, updated = evs, last
	default:
		return nil
	}
	if len(events) == 0 {
		// Nothing user-visible in the file, but the cursor must still advance:
		// otherwise every open reparses the whole transcript forever.
		if updated == "" {
			updated = sess.LastMessageAt
		}
		return im.store.SetImportState(sess.ID, ImportState{
			Path: path, Offset: size, Size: size, Updated: updated, Ino: ino, MTime: mtime,
		})
	}
	windows, _ := im.store.ConsoleTaskWindows(sess.ID)
	events = filterTaskWindows(events, windows)
	limit := im.historyLimit
	omitted := 0
	if limit > 0 && len(events) > limit {
		omitted = len(events) - limit
		events = events[len(events)-limit:]
	}
	if updated == "" {
		updated = nowRFC3339()
	}
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
		ino, mtime = fileStamp(fi)
	}
	if omitted > 0 {
		// Keep the truncation explicit in the transcript rather than silently
		// dropping the beginning of the conversation.
		notice := impEvent{TS: events[0].TS, Kind: "notice", Payload: map[string]any{
			"text": fmt.Sprintf("更早的 %d 条原生历史未在控制台展开；原生恢复仍包含完整上下文。", omitted)}}
		events = append([]impEvent{notice}, events...)
	}
	taskID := importTaskID(sess.ID)
	// Replace and record the cursor in one transaction: a crash between the two
	// used to leave the session with no snapshot but a stale cursor, so it never
	// re-imported again.
	next := ImportState{Path: path, Offset: size, Size: size, Updated: updated, Ino: ino, MTime: mtime}
	nextSeq, err := im.store.ReplaceImportedEvents(sess.ID, taskID, events, updated, title, next)
	if err != nil {
		return err
	}
	next.Seq = nextSeq
	return im.store.SetImportState(sess.ID, next)
}

// parseTail reads complete lines starting at offset. A trailing partial line is
// left unconsumed so the next sync starts on a record boundary.
func (im *Importer) parseTail(agent, path string, offset int64) ([]impEvent, int64, *claudeParse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var consumed int64
	var out []impEvent
	var ps *claudeParse
	seen := map[string]bool{}
	if agent == "claude" {
		ps = &claudeParse{sid: strings.TrimSuffix(filepath.Base(path), ".jsonl"), seen: map[string]bool{}}
	}
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break // partial trailing line: keep it for the next sync
		}
		if err != nil {
			return out, offset + consumed, ps, err
		}
		consumed += int64(len(line))
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			continue
		}
		if ps != nil {
			var o map[string]any
			if json.Unmarshal(line, &o) != nil {
				continue
			}
			evs, _, _ := parseClaudeRecord(o, ps)
			out = append(out, evs...)
			continue
		}
		var o struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &o) != nil {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(o.Payload, &p)
		if len(p) == 0 {
			continue
		}
		out = append(out, parseCodexRecord(o.Type, o.Timestamp, p, seen, nil)...)
	}
	return out, offset + consumed, ps, nil
}

// findNativeFile locates the transcript for a session whose recorded path is
// gone or was never stored (legacy snapshots).
func (im *Importer) findNativeFile(agent, nativeID string) string {
	var candidates []string
	if agent == "claude" {
		root := filepath.Join(im.home, ".claude", "projects")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if strings.Contains(filepath.ToSlash(path), "/subagents/") {
				return nil
			}
			if strings.TrimSuffix(filepath.Base(path), ".jsonl") == nativeID {
				candidates = append(candidates, path)
			}
			return nil
		})
	} else {
		root := filepath.Join(im.home, ".codex", "sessions")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if uuidFromFilename(filepath.Base(path)) == nativeID {
				candidates = append(candidates, path)
			}
			return nil
		})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		si, _ := os.Stat(candidates[i])
		sj, _ := os.Stat(candidates[j])
		if si == nil || sj == nil {
			return candidates[i] > candidates[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	return candidates[0]
}

// filterTaskWindows drops native records that a console task already ingested.
func filterTaskWindows(events []impEvent, windows []TaskWindow) []impEvent {
	if len(windows) == 0 {
		return events
	}
	type span struct{ start, end time.Time }
	var spans []span
	for _, w := range windows {
		s, err := time.Parse(time.RFC3339Nano, w.Start)
		if err != nil {
			continue
		}
		e, err := time.Parse(time.RFC3339Nano, w.End)
		if err != nil {
			// A task that is still running: treat the window as open-ended.
			e = time.Now().Add(time.Minute)
		}
		spans = append(spans, span{s, e})
	}
	if len(spans) == 0 {
		return events
	}
	out := events[:0]
	for _, e := range events {
		ts, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			out = append(out, e)
			continue
		}
		inside := false
		for _, sp := range spans {
			if !ts.Before(sp.start) && !ts.After(sp.end) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, e)
		}
	}
	return out
}

// looksAutoTitle reports whether a title is safe to replace. A user rename is
// never overwritten by a later sync.
func looksAutoTitle(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	for _, prefix := range []string{"Claude 会话 ", "Codex 会话 ", "新会话 · "} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}
