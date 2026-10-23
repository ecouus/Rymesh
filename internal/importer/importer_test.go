package importer

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rymesh/internal/store"
	"rymesh/internal/util"
)

func TestImporterRescanSkipsUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		line, _ := json.Marshal(map[string]any{
			"timestamp": "2026-01-01T00:00:00Z", "sessionId": strings.TrimSuffix(name, ".jsonl"),
			"cwd": home, "type": "user", "message": map[string]any{"content": text},
		})
		if err := os.WriteFile(filepath.Join(dir, name), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("11111111-2222-4333-8444-555555555555.jsonl", "hello")
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("first scan = %+v", rep)
	}
	// Nothing changed: the rescan must not re-import anything.
	if rep := im.ImportAll(); rep.Imported != 0 || rep.Updated != 0 || rep.Skipped != 1 {
		t.Fatalf("idle rescan = %+v", rep)
	}
	// A conversation started in a terminal after the first scan is picked up.
	write("66666666-7777-4888-9999-000000000000.jsonl", "new one")
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("rescan after new file = %+v", rep)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 2 {
		t.Fatalf("sessions = %d", len(got))
	}
}

func TestImporterSkipsExcludedCWD(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "test-work")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"timestamp": "2026-01-01T00:00:00Z",
		"sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd":       work,
		"type":      "user",
		"message":   map[string]any{"content": "hello"},
	})
	file := filepath.Join(home, ".claude", "projects", "proj", "s.jsonl")
	if err := os.WriteFile(file, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := log.New(io.Discard, "", 0)

	im := &Importer{store: st, home: home, log: logger}
	rep := im.ImportAll()
	if rep.Excluded != 0 || rep.Imported != 1 {
		t.Fatalf("unexcluded import = %+v", rep)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 1 {
		t.Fatalf("sessions after import = %d", len(got))
	}

	st2, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	im2 := &Importer{store: st2, home: home, log: logger, excludeRoots: []string{work}}
	rep = im2.ImportAll()
	if rep.Excluded != 1 || rep.Imported != 0 {
		t.Fatalf("excluded import = %+v", rep)
	}
	if got, _ := st2.ListSessions("", "", true); len(got) != 0 {
		t.Fatalf("excluded session was imported: %d", len(got))
	}
}

// writeClaudeLine appends one Claude transcript record.
func appendClaudeRecords(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range records {
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func claudeRecord(ts, typ, uuid string, content any) map[string]any {
	if s, ok := content.(string); ok && typ != "user" {
		// Assistant records carry typed content blocks, never a bare string.
		content = []any{map[string]any{"type": "text", "text": s}}
	}
	return map[string]any{
		"timestamp": ts, "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": os.Getenv("TMPDIR"), "type": typ, "uuid": uuid,
		"message": map[string]any{"content": content},
	}
}

// A session continued in the CLI after the startup snapshot must pick up the
// new tail when it is opened, otherwise the last answer never appears.
func TestSyncSessionAppendsNewNativeEvents(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}

	rec := func(ts, typ, uuid, text string) map[string]any {
		r := claudeRecord(ts, typ, uuid, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:00Z", "user", "u1", "first question"),
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "first answer"),
	)
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("import = %+v", rep)
	}
	sess, err := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := st.SessionStats(sess.ID)
	if before != 2 {
		t.Fatalf("events before = %d", before)
	}

	// Continue the conversation natively, as a CLI session would.
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:01:00Z", "user", "u2", "second question"),
		rec("2026-01-01T00:01:10Z", "assistant", "a2", "second answer"),
	)
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	after, _, _ := st.SessionStats(sess.ID)
	if after != 4 {
		t.Fatalf("events after sync = %d, want 4", after)
	}
	got, err := st.RecentEvents(sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	last := string(got[len(got)-1].Payload)
	if !strings.Contains(last, "second answer") {
		t.Fatalf("last event = %s", last)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.LastMessageAt != "2026-01-01T00:01:10Z" {
		t.Fatalf("last_message_at = %q", fresh.LastMessageAt)
	}

	// A second sync with no new data must not duplicate anything.
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := st.SessionStats(sess.ID); again != 4 {
		t.Fatalf("events after idle sync = %d, want 4", again)
	}
}

// A console-created session continued with `claude --resume` in a terminal
// must show the terminal turns, but not repeat the turns the console ran.
func TestSyncSessionPicksUpTerminalTurnsOfWebSession(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "11111111-2222-4333-8444-555555555555.jsonl")
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}

	sess := &store.Session{Agent: "claude", NativeID: "11111111-2222-4333-8444-555555555555",
		Cwd: home, Title: "新会话 · Claude Code", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	// The console ran the first turn; its events came from the task log.
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: home, Prompt: "web question",
		Status: store.StatusCompleted, StartedAt: "2026-01-01T00:00:00Z", EndedAt: "2026-01-01T00:00:30Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"web question", "web answer"} {
		p, _ := json.Marshal(map[string]any{"text": txt})
		if _, err := st.InsertEvent(&store.Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: int64(i + 1),
			TS: "2026-01-01T00:00:1" + strconv.Itoa(i) + "Z", Kind: "text", Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	// The native transcript holds that console turn plus a terminal turn.
	appendClaudeRecords(t, path,
		claudeRecord("2026-01-01T00:00:10Z", "user", "u1", "web question"),
		claudeRecord("2026-01-01T00:00:20Z", "assistant", "a1", "web answer"),
		claudeRecord("2026-01-01T01:00:00Z", "user", "u2", "terminal question"),
		claudeRecord("2026-01-01T01:00:10Z", "assistant", "a2", "terminal answer"),
	)
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	var all []string
	for _, e := range evs {
		all = append(all, string(e.Payload))
	}
	joined := strings.Join(all, "|")
	if len(evs) != 4 || strings.Count(joined, "web answer") != 1 || !strings.Contains(joined, "terminal answer") {
		t.Fatalf("events after sync (%d): %s", len(evs), joined)
	}
	// Idle sync and the periodic rescan must not duplicate anything.
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	im.ImportAll()
	if n, _, _ := st.SessionStats(sess.ID); n != 4 {
		t.Fatalf("events after resync = %d, want 4", n)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 1 {
		t.Fatalf("rescan created a duplicate session: %d", len(got))
	}
}

func TestSyncSessionSkipsPartialLine(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "hello")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// A writer that has flushed only half a record must not corrupt the cursor.
	full, _ := json.Marshal(func() map[string]any {
		x := claudeRecord("2026-01-01T00:02:00Z", "assistant", "a2", "half written")
		x["cwd"] = work
		return x
	}())
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if _, err := f.Write(full[:len(full)/2]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 1 {
		t.Fatalf("partial line was imported: %d", n)
	}

	// Completing the record lets the next sync pick it up exactly once.
	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if _, err := f.Write(append(full[len(full)/2:], '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("completed line not imported: %d", n)
	}
}

// Console-run turns are already in the session from the task log; the same
// records in the native transcript must not be appended a second time.
func TestSyncSessionSkipsConsoleTaskWindows(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "start")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// A finished console task covering 00:01–00:02.
	task := &store.Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console turn", Status: store.StatusCompleted, CreatedAt: "2026-01-01T00:01:00Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE tasks SET started_at=?, ended_at=? WHERE id=?`,
		"2026-01-01T00:01:00Z", "2026-01-01T00:02:00Z", task.ID); err != nil {
		t.Fatal(err)
	}

	ins := map[string]any{"timestamp": "2026-01-01T00:01:30Z", "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": work, "type": "assistant", "uuid": "a-console",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "console answer"}}}}
	after := map[string]any{"timestamp": "2026-01-01T00:03:00Z", "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": work, "type": "assistant", "uuid": "a-cli",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "cli answer"}}}}
	appendClaudeRecords(t, path, ins, after)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	var texts []string
	for _, e := range evs {
		texts = append(texts, string(e.Payload))
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, "console answer") {
		t.Fatal("console turn was imported twice")
	}
	if !strings.Contains(joined, "cli answer") {
		t.Fatal("native-only turn was not imported")
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("events = %d, want 2 (start + cli answer)", n)
	}
}

// A user rename must survive later syncs; an auto title may be upgraded.
func TestSyncSessionPreservesUserTitle(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "auto title source")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	userTitle := "我的自定义标题"
	if err := st.UpdateSession(sess.ID, &userTitle, nil); err != nil {
		t.Fatal(err)
	}

	appendClaudeRecords(t, path, map[string]any{"timestamp": "2026-01-01T00:05:00Z",
		"sessionId": "11111111-2222-4333-8444-555555555555", "cwd": work,
		"type": "ai-title", "aiTitle": "Generated title"})
	appendClaudeRecords(t, path, func() map[string]any {
		x := claudeRecord("2026-01-01T00:06:00Z", "assistant", "a2", "later")
		x["cwd"] = work
		return x
	}())
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.Title != userTitle {
		t.Fatalf("title = %q, want %q", fresh.Title, userTitle)
	}
}

// Legacy snapshots were taken once at startup and carry no cursor. Opening such
// a session must backfill everything the native file gained since.
func TestSyncSessionBackfillsLegacySnapshot(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	// Claude names transcripts after the session id, which is how a legacy
	// snapshot without a stored path is located again.
	path := filepath.Join(proj, "11111111-2222-4333-8444-555555555555.jsonl")
	rec := func(ts, typ, uuid, text string) map[string]any {
		r := claudeRecord(ts, typ, uuid, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "question"))

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// Simulate the pre-fix snapshot: events imported, but no cursor recorded.
	if _, err := st.DB.Exec(`UPDATE sessions SET meta='{}' WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "answer"),
		rec("2026-01-01T00:00:09Z", "assistant", "a2", "latest answer"),
	)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if last := string(evs[len(evs)-1].Payload); !strings.Contains(last, "latest answer") {
		t.Fatalf("last = %s", last)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.LastMessageAt != "2026-01-01T00:00:09Z" {
		t.Fatalf("last_message_at = %q", fresh.LastMessageAt)
	}
	// The cursor is now recorded, so the next open is a no-op.
	if stt := store.ParseImportState(st.GetSessionMeta(sess.ID)); stt.Offset <= 0 {
		t.Fatalf("cursor not persisted: %+v", stt)
	}
}

// A native file that shrank (rewritten/compacted) invalidates the cursor and
// must be re-read in full instead of waiting for bytes past the old offset.
func TestSyncSessionReimportsAfterTruncation(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "keep me")
	r["cwd"] = work
	appendClaudeRecords(t, path, r,
		func() map[string]any {
			x := claudeRecord("2026-01-01T00:00:05Z", "assistant", "a1", strings.Repeat("old ", 200))
			x["cwd"] = work
			return x
		}())

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("events = %d", n)
	}

	// Rewrite the transcript shorter with different content.
	var b strings.Builder
	for _, rec := range []map[string]any{
		r,
		func() map[string]any {
			x := claudeRecord("2026-01-01T00:00:07Z", "assistant", "a2", "new")
			x["cwd"] = work
			return x
		}(),
	} {
		raw, _ := json.Marshal(rec)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	if len(evs) != 2 {
		t.Fatalf("events after rewrite = %d, want 2", len(evs))
	}
	if last := string(evs[len(evs)-1].Payload); !strings.Contains(last, "\"new\"") {
		t.Fatalf("last = %s", last)
	}
}

// Re-importing must never renumber a session that also holds console-run turns:
// their events would jump after the imported history.
func TestReimportFullKeepsMixedSession(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "native")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	task := &store.Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console", Status: store.StatusCompleted, CreatedAt: "2026-01-01T00:10:00Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO events (session_id, task_id, task_seq, ts, kind, payload)
		VALUES (?, ?, 1, ?, 'user', ?)`, sess.ID, task.ID, "2026-01-01T00:10:00Z",
		`{"text":"console turn"}`); err != nil {
		t.Fatal(err)
	}
	before, _, _ := st.SessionStats(sess.ID)

	// Force the full-reimport path.
	if _, err := st.DB.Exec(`UPDATE sessions SET meta='{}' WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	after, _, _ := st.SessionStats(sess.ID)
	if after != before {
		t.Fatalf("mixed session was rewritten: %d -> %d", before, after)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	found := false
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "console turn") {
			found = true
		}
	}
	if !found {
		t.Fatal("console turn lost")
	}
}

// A console task that is still running has no ended_at; its native records are
// already arriving from the task log and must not be appended by a sync.
func TestSyncSessionSkipsRunningTaskWindow(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "start")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	task := &store.Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console", Status: store.StatusRunning, CreatedAt: util.NowRFC3339()}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	live := map[string]any{"timestamp": time.Now().UTC().Add(2 * time.Second).Format("2006-01-02T15:04:05.000Z"),
		"sessionId": "11111111-2222-4333-8444-555555555555", "cwd": work,
		"type": "assistant", "uuid": "a-live",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "live console turn"}}}}
	appendClaudeRecords(t, path, live)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "live console turn") {
			t.Fatal("running console task was imported a second time")
		}
	}
}

// Two concurrent opens of the same session (detail + events tail are fetched in
// parallel by the page) must not append the same native tail twice.
func TestConcurrentSyncDoesNotDuplicate(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	os.MkdirAll(work, 0o700)
	proj := filepath.Join(home, ".claude", "projects", "p")
	os.MkdirAll(proj, 0o700)
	uuid := "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(proj, uuid+".jsonl")
	rec := func(ts, typ, id, text string) map[string]any {
		r := claudeRecord(ts, typ, id, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "q"))

	st, _ := store.OpenStore(t.TempDir())
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", uuid)

	// The CLI appends while the page opens.
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "answer one"),
		rec("2026-01-01T00:00:09Z", "assistant", "a2", "answer two"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = im.SyncSession(sess.ID) }()
	}
	wg.Wait()
	evs, _ := st.RecentEvents(sess.ID, 50)
	t.Logf("events=%d", len(evs))
	seen := map[string]int{}
	for _, e := range evs {
		var p map[string]any
		json.Unmarshal(e.Payload, &p)
		if s, ok := p["text"].(string); ok {
			seen[s]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("%q imported %d times", k, n)
		}
	}
	if len(evs) != 3 {
		t.Fatalf("events=%d, want 3", len(evs))
	}
}

// A transcript rewritten in place (same path, same size) is a different file;
// reading from the old offset would splice two conversations together.
func TestSyncSessionDetectsInPlaceRewrite(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	uuid := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	path := filepath.Join(proj, uuid+".jsonl")
	rec := func(ts, typ, id, text string) map[string]any {
		r := claudeRecord(ts, typ, id, text)
		r["sessionId"] = uuid
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "original question"))

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, err := st.FindNativeSession("claude", uuid)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the file: a new inode with the same name. The content is longer
	// but the point is that the identity changed, not the size.
	tmp := path + ".new"
	appendClaudeRecords(t, tmp,
		rec("2026-01-02T00:00:00Z", "user", "u2", "rewritten question"),
		rec("2026-01-02T00:00:01Z", "assistant", "a2", "rewritten answer"))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := st.RecentEvents(sess.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if s, ok := p["text"].(string); ok {
			texts = append(texts, s)
		}
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "original question") {
		t.Fatalf("stale snapshot kept after replacement: %q", joined)
	}
	if !strings.Contains(joined, "rewritten answer") {
		t.Fatalf("replacement not imported: %q", joined)
	}
}

func TestImporterSkipsCodexSubagentThreads(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "21")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id string, source any) {
		meta, _ := json.Marshal(map[string]any{"timestamp": "2026-10-21T00:00:00Z", "type": "session_meta",
			"payload": map[string]any{"id": id, "cwd": home, "timestamp": "2026-10-21T00:00:00Z", "source": source}})
		msg, _ := json.Marshal(map[string]any{"timestamp": "2026-10-21T00:00:01Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}})
		_ = os.WriteFile(filepath.Join(dir, "rollout-2026-10-21T00-00-00-"+id+".jsonl"), append(append(meta, '\n'), append(msg, '\n')...), 0o600)
	}
	write("01a15ee8-0000-7000-8000-000000000001", "cli")
	write("01a15ee8-0000-7000-8000-000000000002", map[string]any{"subagent": map[string]any{"other": "guardian"}})
	st, _ := store.OpenStore(t.TempDir())
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	got, _ := st.ListSessions("", "", true)
	if len(got) != 1 || got[0].NativeID != "01a15ee8-0000-7000-8000-000000000001" {
		t.Fatalf("sessions = %d %+v", len(got), got)
	}
}
