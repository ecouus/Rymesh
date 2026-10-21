package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ImportReport struct {
	CodexFiles   int      `json:"codex_files"`
	ClaudeFiles  int      `json:"claude_files"`
	Imported     int      `json:"imported"`
	Updated      int      `json:"updated"`
	Skipped      int      `json:"skipped"`
	Excluded     int      `json:"excluded"`
	Errors       int      `json:"errors"`
	ErrorSamples []string `json:"error_samples,omitempty"`
	FinishedAt   string   `json:"finished_at"`
}

type impEvent struct {
	TS      string
	Kind    string
	Payload map[string]any
}

type Importer struct {
	store        *Store
	home         string
	log          *log.Logger
	excludeRoots []string
	historyLimit int
	// syncMu serializes incremental syncs; the importer is shared by all HTTP
	// handlers and the underlying store has a single connection.
	syncMu sync.Mutex
	// stamps records each transcript as of its last successful import, so the
	// periodic rescan only re-parses files that changed since.
	stampMu  sync.Mutex
	stamps   map[string]fileKey
	titleKey fileKey
}

type fileKey struct {
	size  int64
	ino   uint64
	mtime int64
}

func statKey(path string) (fileKey, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileKey{}, false
	}
	ino, mt := fileStamp(fi)
	return fileKey{size: fi.Size(), ino: ino, mtime: mt}, true
}

// unchanged reports whether path still matches its last imported stamp. The
// returned key is what to remember once the file has been imported.
func (im *Importer) unchanged(path string) (fileKey, bool) {
	k, ok := statKey(path)
	if !ok {
		return k, false
	}
	im.stampMu.Lock()
	defer im.stampMu.Unlock()
	old, seen := im.stamps[path]
	return k, seen && old == k
}

func (im *Importer) remember(path string, k fileKey) {
	im.stampMu.Lock()
	if im.stamps == nil {
		im.stamps = map[string]fileKey{}
	}
	im.stamps[path] = k
	im.stampMu.Unlock()
}

func newImporter(st *Store, logger *log.Logger, excludeRoots []string, historyLimit int) *Importer {
	h, _ := os.UserHomeDir()
	return &Importer{store: st, home: h, log: logger,
		excludeRoots: append([]string(nil), excludeRoots...), historyLimit: historyLimit}
}

func (im *Importer) excluded(cwd string) bool {
	return isExcludedCWD(cwd, im.excludeRoots)
}

func (im *Importer) ImportAll() ImportReport {
	rep := ImportReport{}
	im.importCodex(&rep)
	im.importClaude(&rep)
	rep.FinishedAt = nowRFC3339()
	return rep
}

func (im *Importer) codexTitles() map[string]string {
	titles := map[string]string{}
	if f, err := os.Open(filepath.Join(im.home, ".codex", "session_index.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var x struct {
				ID         string `json:"id"`
				ThreadName string `json:"thread_name"`
			}
			if json.Unmarshal(sc.Bytes(), &x) == nil && x.ID != "" && x.ThreadName != "" {
				titles[x.ID] = x.ThreadName
			}
		}
		f.Close()
	}
	return titles
}

func (im *Importer) importCodex(rep *ImportReport) {
	root := filepath.Join(im.home, ".codex", "sessions")
	titles := im.codexTitles()
	// Codex keeps thread titles in a separate index. When it changes, a rollout
	// may need a new title even though its own file did not change.
	if k, _ := statKey(filepath.Join(im.home, ".codex", "session_index.jsonl")); k != im.titleKey {
		im.stampMu.Lock()
		im.titleKey = k
		im.stamps = nil
		im.stampMu.Unlock()
	}
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	// Newest first so if a thread has several rollouts we take the freshest one.
	sort.Slice(files, func(i, j int) bool {
		si, _ := os.Stat(files[i])
		sj, _ := os.Stat(files[j])
		if si == nil || sj == nil {
			return files[i] > files[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	for _, path := range files {
		rep.CodexFiles++
		k, same := im.unchanged(path)
		if same {
			rep.Skipped++
			continue
		}
		if err := im.importCodexFile(path, titles, rep); err != nil {
			rep.Errors++
			if len(rep.ErrorSamples) < 5 {
				rep.ErrorSamples = append(rep.ErrorSamples, filepath.Base(path)+": "+err.Error())
			}
			continue
		}
		im.remember(path, k)
	}
}

// codexMeta is the session identity recorded in a Codex rollout.
type codexMeta struct {
	ID, Cwd, Timestamp string
	// Subagent marks threads Codex spawns for itself (e.g. the "guardian"
	// approval reviewer). Like Claude's subagent transcripts they are not
	// conversations of their own.
	Subagent bool
}

func (im *Importer) importCodexFile(path string, titles map[string]string, rep *ImportReport) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var meta codexMeta
	var events []impEvent
	lastTS := ""
	firstUser := ""
	seen := map[string]bool{}
	for sc.Scan() {
		var o struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(o.Payload, &p)
		if len(p) == 0 {
			continue
		}
		if o.Timestamp != "" {
			lastTS = o.Timestamp
		}
		if o.Type == "session_meta" {
			meta.ID = firstNonEmpty(str(p["id"]), str(p["session_id"]))
			meta.Cwd = str(p["cwd"])
			meta.Timestamp = str(p["timestamp"])
			if src, ok := p["source"].(map[string]any); ok && src["subagent"] != nil {
				meta.Subagent = true
			}
			continue
		}
		events = append(events, parseCodexRecord(o.Type, o.Timestamp, p, seen, &firstUser)...)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if meta.ID == "" {
		meta.ID = uuidFromFilename(filepath.Base(path))
	}
	if meta.ID == "" || meta.Cwd == "" {
		return fmt.Errorf("missing session id or cwd")
	}
	if im.excluded(meta.Cwd) {
		rep.Excluded++
		return nil
	}
	if meta.Subagent {
		// Drop one imported by an earlier version, too.
		if existing, err := im.store.FindNativeSession("codex", meta.ID); err == nil && existing.Source == "imported" {
			_ = im.store.DeleteSessionRow(existing.ID)
		}
		rep.Skipped++
		return nil
	}
	if st, err := os.Stat(meta.Cwd); err != nil || !st.IsDir() {
		return fmt.Errorf("recorded cwd no longer exists: %s", meta.Cwd)
	}
	title := titles[meta.ID]
	if title == "" {
		title = truncateTitle(firstUser, 72)
	}
	if title == "" {
		title = "Codex 会话 " + shortID(meta.ID)
	}
	created := meta.Timestamp
	if created == "" {
		created = firstTS(events)
	}
	updated := lastTS
	if updated == "" {
		updated = created
	}
	return im.storeImported("codex", meta.ID, meta.Cwd, title, created, updated, events, path, rep)
}

// parseCodexFile parses a whole Codex rollout for a full re-import.
func (im *Importer) parseCodexFile(path string) ([]impEvent, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var out []impEvent
	seen := map[string]bool{}
	lastTS := ""
	for sc.Scan() {
		var o struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(o.Payload, &p)
		if len(p) == 0 {
			continue
		}
		if o.Timestamp != "" {
			lastTS = o.Timestamp
		}
		out = append(out, parseCodexRecord(o.Type, o.Timestamp, p, seen, nil)...)
	}
	return out, lastTS, sc.Err()
}

// parseCodexRecord converts one rollout record into console events.
func parseCodexRecord(typ, ts string, p map[string]any, seen map[string]bool, firstUser *string) []impEvent {
	if typ != "response_item" {
		return nil
	}
	var out []impEvent
	switch str(p["type"]) {
	case "message":
		role := str(p["role"])
		txt := contentText(p["content"])
		if txt == "" {
			return nil
		}
		switch role {
		case "user":
			if internalUserText(txt) {
				return nil
			}
			if firstUser != nil && *firstUser == "" {
				*firstUser = txt
			}
			out = append(out, impEvent{TS: ts, Kind: "user", Payload: map[string]any{"text": txt}})
		case "assistant":
			out = append(out, impEvent{TS: ts, Kind: "assistant", Payload: map[string]any{"text": txt}})
		}
	case "reasoning":
		txt := summaryText(p["summary"])
		if txt == "" {
			txt = contentText(p["content"])
		}
		if txt != "" {
			out = append(out, impEvent{TS: ts, Kind: "thinking", Payload: map[string]any{"text": txt}})
		}
	case "custom_tool_call", "function_call", "local_shell_call":
		id := firstNonEmpty(str(p["call_id"]), str(p["id"]))
		// Only a real id can dedupe. Records without one all share the empty
		// key, so the first call would hide every later tool call.
		if seen != nil && id != "" {
			if seen["tool:"+id] {
				return nil
			}
			seen["tool:"+id] = true
		}
		input := p["input"]
		if input == nil {
			input = p["arguments"]
		}
		out = append(out, impEvent{TS: ts, Kind: "tool",
			Payload: map[string]any{"id": id, "name": firstNonEmpty(str(p["name"]), "tool"),
				"status": firstNonEmpty(str(p["status"]), "completed"), "input": input}})
	case "custom_tool_call_output", "function_call_output":
		out = append(out, impEvent{TS: ts, Kind: "tool_result",
			Payload: map[string]any{"id": str(p["call_id"]), "output": contentText(p["output"])}})
	case "web_search_call":
		out = append(out, impEvent{TS: ts, Kind: "tool",
			Payload: map[string]any{"id": str(p["id"]), "name": "web_search",
				"status": "completed", "input": p["action"]}})
	}
	return out
}

func (im *Importer) importClaude(rep *ImportReport) {
	root := filepath.Join(im.home, ".claude", "projects")
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		// Subagent transcripts carry the parent sessionId but contain only
		// sidechain events. They are not conversations of their own; importing
		// them used to create empty sessions that shadowed the real one.
		if strings.Contains(filepath.ToSlash(path), "/subagents/") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	for _, path := range files {
		rep.ClaudeFiles++
		k, same := im.unchanged(path)
		if same {
			rep.Skipped++
			continue
		}
		if err := im.importClaudeFile(path, rep); err != nil {
			rep.Errors++
			if len(rep.ErrorSamples) < 5 {
				rep.ErrorSamples = append(rep.ErrorSamples, filepath.Base(path)+": "+err.Error())
			}
			continue
		}
		im.remember(path, k)
	}
}

func (im *Importer) importClaudeFile(path string, rep *ImportReport) error {
	ps := &claudeParse{sid: strings.TrimSuffix(filepath.Base(path), ".jsonl"), seen: map[string]bool{}}
	evs, updated, err := im.parseClaudeFile(path, ps)
	if err != nil {
		return err
	}
	ps.events = evs
	ps.updated = updated
	cwd := ps.cwd
	if cwd == "" {
		cwd = decodeClaudeProjectDir(filepath.Base(filepath.Dir(path)))
	}
	if ps.sid == "" || cwd == "" {
		return fmt.Errorf("missing session id or cwd")
	}
	if im.excluded(cwd) {
		rep.Excluded++
		return nil
	}
	title := ps.title()
	created := ps.created
	if created == "" {
		if st, err := os.Stat(path); err == nil {
			created = st.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	if updated == "" {
		updated = created
	}
	return im.storeImported("claude", ps.sid, cwd, title, created, updated, ps.events, path, rep)
}

// claudeParse accumulates the parts of a transcript that describe a session.
type claudeParse struct {
	sid       string
	cwd       string
	firstUser string
	titleCand string
	aiTitle   string
	created   string
	updated   string
	events    []impEvent
	seen      map[string]bool
}

func (ps *claudeParse) title() string {
	if ps.titleCand != "" {
		return ps.titleCand
	}
	if t := truncateTitle(ps.aiTitle, 72); t != "" {
		return t
	}
	if t := truncateTitle(ps.firstUser, 72); t != "" {
		return t
	}
	return "Claude 会话 " + shortID(ps.sid)
}

// parseClaudeFile reads every record of a transcript. A nil ps returns just the
// events (used by the incremental tail reader).
func (im *Importer) parseClaudeFile(path string, ps *claudeParse) ([]impEvent, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	return im.scanClaudeRecords(bufio.NewScanner(f), ps)
}

func (im *Importer) scanClaudeRecords(sc *bufio.Scanner, ps *claudeParse) ([]impEvent, string, error) {
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var out []impEvent
	updated := ""
	for sc.Scan() {
		var o map[string]any
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		evs, ts, ok := parseClaudeRecord(o, ps)
		if ok && ts != "" {
			updated = ts
		}
		out = append(out, evs...)
	}
	if err := sc.Err(); err != nil {
		return out, updated, err
	}
	return out, updated, nil
}

// parseClaudeRecord converts one JSONL record. ps may be nil; the bool reports
// whether the record carried a usable timestamp.
func parseClaudeRecord(o map[string]any, ps *claudeParse) ([]impEvent, string, bool) {
	if ps != nil {
		if ts := str(o["timestamp"]); ts != "" {
			if ps.created == "" {
				ps.created = ts
			}
			ps.updated = ts
		}
		if s := str(o["sessionId"]); s != "" {
			ps.sid = s
		}
		if c := str(o["cwd"]); c != "" {
			ps.cwd = c
		}
	}
	ts := str(o["timestamp"])
	typ := str(o["type"])
	if typ == "summary" || typ == "custom-title" {
		if ps != nil {
			if s := firstNonEmpty(str(o["summary"]), str(o["customTitle"]), str(o["title"])); s != "" {
				ps.titleCand = s
			}
		}
		return nil, ts, false
	}
	// Claude Code stores its generated title separately; prefer it over a
	// truncated first message.
	if typ == "ai-title" {
		if ps != nil {
			if s := str(o["aiTitle"]); s != "" {
				ps.aiTitle = s
			}
		}
		return nil, ts, false
	}
	if b, _ := o["isSidechain"].(bool); b {
		return nil, ts, false
	}
	if b, _ := o["isMeta"].(bool); b {
		return nil, ts, false
	}
	uuid := str(o["uuid"])
	if ps != nil && uuid != "" {
		if ps.seen[uuid] {
			return nil, ts, false
		}
		ps.seen[uuid] = true
	}
	msg, _ := o["message"].(map[string]any)
	if msg == nil {
		return nil, ts, false
	}
	var out []impEvent
	content := msg["content"]
	switch typ {
	case "user":
		switch c := content.(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				if ps != nil && ps.firstUser == "" {
					ps.firstUser = c
				}
				out = append(out, impEvent{TS: ts, Kind: "user", Payload: map[string]any{"text": c}})
			}
		case []any:
			for _, it := range c {
				blk, _ := it.(map[string]any)
				switch str(blk["type"]) {
				case "text":
					if t := str(blk["text"]); t != "" {
						if ps != nil && ps.firstUser == "" {
							ps.firstUser = t
						}
						out = append(out, impEvent{TS: ts, Kind: "user", Payload: map[string]any{"text": t}})
					}
				case "tool_result":
					out = append(out, impEvent{TS: ts, Kind: "tool_result",
						Payload: map[string]any{"id": str(blk["tool_use_id"]),
							"output": textOf(blk["content"]), "is_error": blk["is_error"]}})
				}
			}
		}
	case "assistant":
		arr, _ := content.([]any)
		for _, it := range arr {
			blk, _ := it.(map[string]any)
			switch str(blk["type"]) {
			case "text":
				if t := str(blk["text"]); t != "" {
					out = append(out, impEvent{TS: ts, Kind: "assistant", Payload: map[string]any{"text": t}})
				}
			case "thinking":
				if t := str(blk["thinking"]); t != "" {
					out = append(out, impEvent{TS: ts, Kind: "thinking", Payload: map[string]any{"text": t}})
				}
			case "tool_use":
				out = append(out, impEvent{TS: ts, Kind: "tool",
					Payload: map[string]any{"id": str(blk["id"]), "name": str(blk["name"]),
						"input": blk["input"], "status": "completed"}})
			}
		}
	case "system":
		if str(o["subtype"]) == "compact_boundary" {
			out = append(out, impEvent{TS: ts, Kind: "notice",
				Payload: map[string]any{"text": "原生会话在此处发生过上下文压缩"}})
		}
	}
	return out, ts, true
}

// snapshotSize is how many event rows a snapshot of n native events writes.
func (im *Importer) snapshotSize(n int) int {
	if im.historyLimit > 0 && n > im.historyLimit {
		return im.historyLimit + 1 // plus the "older history" notice
	}
	return n
}

func (im *Importer) storeImported(agent, nativeID, cwd, title, created, updated string, events []impEvent, path string, rep *ImportReport) error {
	existing, err := im.store.FindNativeSession(agent, nativeID)
	if err != nil && !isNoRowsErr(err) {
		return err
	}
	if existing != nil && existing.Source == "web" {
		// Never snapshot a console session, but do pick up turns added to it
		// from a terminal so it moves up the list without being opened.
		if err := im.SyncSession(existing.ID); err != nil {
			return err
		}
		rep.Skipped++
		return nil
	}
	// A transcript with nothing user-visible (local commands, /resume, empty
	// rollouts) is not a conversation the user can open or continue.
	if len(events) == 0 {
		if existing != nil {
			_ = im.store.DeleteSessionRow(existing.ID)
		}
		rep.Skipped++
		return nil
	}
	if existing != nil {
		// Re-import when the earlier snapshot came from an incomplete transcript
		// (for example a subagent file) or when history was capped. A larger
		// native event count alone is not a reason: appended history is handled
		// incrementally by SyncSession, and replacing the snapshot would drop
		// console-produced events that cannot be reconstructed.
		//
		// A session with console-run turns is never replaced here either: the
		// same native records are already ingested from the task logs, and the
		// full re-import has no way to tell which ones to skip without the
		// per-task windows the incremental path uses.
		st := parseImportState(existing.Meta)
		hasConsole, _ := im.store.HasNonImportEvents(existing.ID, importTaskID(existing.ID))
		existingEvents, _, _ := im.store.SessionStats(existing.ID)
		wantEvents := im.snapshotSize(len(events))
		full := hasConsole || (existing.SnapshotImported && existingEvents >= int64(wantEvents) &&
			st.Path == path && st.Offset > 0)
		if full {
			// The transcript changed (the rescan only gets here for changed
			// files), so bring the session's tail up to date.
			if err := im.SyncSession(existing.ID); err != nil {
				return err
			}
			// Codex writes its thread name to session_index.jsonl only after the
			// turn ends, so a title that was a placeholder at first import can
			// still be upgraded later without touching the transcript.
			if looksAutoTitle(existing.Title) && !looksAutoTitle(title) && title != existing.Title {
				if err := im.store.UpdateSessionTitle(existing.ID, title); err != nil {
					return err
				}
				rep.Updated++
				return nil
			}
			rep.Skipped++
			return nil
		}
		if err := im.store.TrashImportedEvents(existing.ID, importTaskID(existing.ID)); err != nil {
			return err
		}
		if err := im.store.UpdateImportedSession(existing.ID, title, cwd, updated); err != nil {
			return err
		}
		rep.Updated++
	} else {
		sess := &Session{
			Agent: agent, NativeID: nativeID, Cwd: cwd, Title: title,
			Source: "imported", CreatedAt: created, UpdatedAt: updated,
			LastMessageAt: updated, SnapshotImported: true,
		}
		if err := im.store.CreateSession(sess); err != nil {
			return err
		}
		existing = sess
		rep.Imported++
	}
	omitted := 0
	if limit := im.historyLimit; limit > 0 && len(events) > limit {
		omitted = len(events) - limit
		events = events[len(events)-limit:]
	}
	seq := int64(0)
	if omitted > 0 {
		seq++
		b, _ := json.Marshal(map[string]any{"text": fmt.Sprintf(
			"更早的 %d 条原生历史未在控制台展开；原生恢复仍包含完整上下文。", omitted)})
		if _, err := im.store.InsertEvent(&Event{SessionID: existing.ID, TaskID: "import-" + existing.ID,
			TaskSeq: seq, TS: created, Kind: "notice", Payload: b}); err != nil {
			return err
		}
	}
	for _, e := range events {
		seq++
		p, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		if _, err := im.store.InsertEvent(&Event{SessionID: existing.ID, TaskID: "import-" + existing.ID,
			TaskSeq: seq, TS: e.TS, Kind: e.Kind, Payload: p}); err != nil {
			return err
		}
	}
	if err := im.store.MarkSnapshotImported(existing.ID); err != nil {
		return err
	}
	var size int64
	var ino uint64
	var mtime int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
		ino, mtime = fileStamp(fi)
	}
	sessionUpdated := laterTS(updated, existing.LastMessageAt)
	if err := im.store.SetImportState(existing.ID, ImportState{
		Path: path, Offset: size, Size: size, Seq: seq, Updated: sessionUpdated,
		Ino: ino, MTime: mtime,
	}); err != nil {
		return err
	}
	return nil
}

func internalUserText(s string) bool {
	t := strings.TrimSpace(s)
	for _, p := range []string{"<environment_context>", "<skills_instructions>", "<multi_agent",
		"<user_instructions>", "<permissions", "<collaboration", "<apps_instructions",
		"<plugins_instructions>", "<system", "# AGENTS.md instructions"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func contentText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, it := range x {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			if t := str(m["text"]); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "")
	default:
		if v == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func firstTS(events []impEvent) string {
	for _, e := range events {
		if e.TS != "" {
			return e.TS
		}
	}
	return nowRFC3339()
}

func uuidFromFilename(name string) string {
	// rollout-2026-10-19T16-56-00-<uuid>.jsonl
	base := strings.TrimSuffix(name, ".jsonl")
	if i := strings.LastIndex(base, "-"); i > 0 {
		// The UUID itself has dashes; search for a 36-char UUID suffix.
		for start := 0; start+36 <= len(base); start++ {
			cand := base[start : start+36]
			if isUUID(cand) {
				return cand
			}
		}
	}
	return ""
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func decodeClaudeProjectDir(name string) string {
	if !strings.HasPrefix(name, "-") {
		return ""
	}
	return strings.ReplaceAll(name, "-", "/")
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
