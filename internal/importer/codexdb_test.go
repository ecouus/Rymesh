package importer

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rymesh/internal/agent"
	"rymesh/internal/store"
)

// TestMain lets the test binary stand in for `codex app-server`: with
// RYMESH_FAKE_CODEX set it serves the threads in that JSON file and exits.
func TestMain(m *testing.M) {
	if f := os.Getenv("RYMESH_FAKE_CODEX"); f != "" {
		fakeCodexAppServer(f)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeCodexData struct {
	Threads []map[string]any            `json:"threads"`
	Turns   map[string][]map[string]any `json:"turns"`
	Log     string                      `json:"log"`
}

func fakeCodexAppServer(path string) {
	b, _ := os.ReadFile(path)
	var d fakeCodexData
	_ = json.Unmarshal(b, &d)
	logf, _ := os.OpenFile(d.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		thread, _ := req.Params["threadId"].(string)
		_, _ = io.WriteString(logf, strings.TrimSpace(req.Method+" "+thread)+"\n")
		var result any = map[string]any{}
		switch req.Method {
		case "thread/list":
			// Like Codex: without an explicit (empty) provider list, only the
			// configured provider's threads, here none.
			threads := []map[string]any{}
			if _, all := req.Params["modelProviders"]; all {
				threads = d.Threads
			}
			result = map[string]any{"data": threads, "nextCursor": nil}
		case "thread/turns/list":
			id, _ := req.Params["threadId"].(string)
			result = map[string]any{"data": d.Turns[id], "nextCursor": nil}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "thread/started", "params": map[string]any{}})
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

// Threads Codex keeps only in its database come in through the app-server;
// one that still has a rollout file is left to the file importer, and an
// unchanged database is not asked again.
func TestImportCodexThreads(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, ".codex", "state_5.sqlite")
	if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	data := fakeCodexData{
		Log: logPath,
		Threads: []map[string]any{
			{"id": "db-thread", "cwd": work, "name": "修复检测", "preview": "fix it", "createdAt": 1792000000, "updatedAt": 1792000100},
			{"id": "file-thread", "cwd": work, "path": rollout, "preview": "has a file", "createdAt": 1792000000, "updatedAt": 1792000100},
			{"id": "gone-dir", "cwd": filepath.Join(home, "removed"), "preview": "old", "createdAt": 1, "updatedAt": 2},
		},
		Turns: map[string][]map[string]any{"db-thread": {
			{"startedAt": 1792000001, "completedAt": 1792000009, "items": []any{
				map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "please fix"}}},
				map[string]any{"type": "reasoning", "summary": []any{"look first"}},
				map[string]any{"type": "commandExecution", "id": "c1", "command": "ls", "cwd": work, "status": "completed",
					"aggregatedOutput": "a.txt", "exitCode": 0},
				map[string]any{"type": "agentMessage", "text": "fixed"},
			}},
			{"startedAt": 1792000050, "completedAt": 1792000060, "error": map[string]any{"message": "stream disconnected"},
				"items": []any{map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "again"}}}}},
		}},
	}
	b, _ := json.Marshal(data)
	fixture := filepath.Join(dir, "codex.json")
	if err := os.WriteFile(fixture, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RYMESH_FAKE_CODEX", fixture)
	old := agent.Agents
	agent.Agents = agent.FixedAgentSet(agent.AgentInfo{Name: "codex", Binary: os.Args[0], Available: true})
	t.Cleanup(func() { agent.Agents = old })

	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	if rep := im.ImportAll(); rep.CodexThreads != 3 || rep.Imported != 1 || rep.Errors != 0 {
		t.Fatalf("first scan = %+v", rep)
	}
	s, err := st.FindNativeSession("codex", "db-thread")
	if err != nil || s.Title != "修复检测" || s.Cwd != work || s.ReadOnly != "" {
		t.Fatalf("session = %+v %v", s, err)
	}
	evs, _ := st.RecentEvents(s.ID, 100)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user,thinking,tool,assistant,user,error" {
		t.Fatalf("events = %s", got)
	}
	if _, err := st.FindNativeSession("codex", "file-thread"); err == nil {
		t.Fatal("a thread with a rollout file was imported from the app-server")
	}

	calls, _ := os.ReadFile(logPath)
	if rep := im.ImportAll(); rep.CodexThreads != 0 || rep.Imported != 0 {
		t.Fatalf("idle rescan = %+v", rep)
	}
	if again, _ := os.ReadFile(logPath); string(again) != string(calls) {
		t.Fatalf("unchanged database queried again: %s", again)
	}
	// The database changed but this thread did not: listed, not re-read.
	if err := os.WriteFile(db, []byte("db2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep := im.ImportAll(); rep.CodexThreads != 3 || rep.Imported != 0 || rep.Updated != 0 {
		t.Fatalf("rescan after db change = %+v", rep)
	}
	if again, _ := os.ReadFile(logPath); strings.Count(string(again), "thread/turns/list db-thread") != 1 {
		t.Fatalf("calls = %s", again)
	}

	// The console then runs a turn in this thread (its events come from the
	// task log), and the user continues in the Codex app afterwards: only the
	// app's turn is added, after what is there.
	task := &store.Task{SessionID: s.ID, Agent: "codex", Cwd: work, Prompt: "from console", Status: store.StatusCompleted,
		StartedAt: "2026-10-15T00:00:00Z", EndedAt: "2026-10-15T00:01:00Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertEvent(&store.Event{SessionID: s.ID, TaskID: task.ID, TaskSeq: 1, TS: "2026-10-15T00:00:01Z",
		Kind: "user", Payload: []byte(`{"text":"from console"}`)}); err != nil {
		t.Fatal(err)
	}
	console := time.Date(2026, 10, 15, 0, 0, 1, 0, time.UTC).Unix()
	later := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC).Unix()
	data.Threads[0]["updatedAt"] = later
	data.Turns["db-thread"] = append(data.Turns["db-thread"],
		map[string]any{"startedAt": console, "completedAt": console + 30, "items": []any{
			map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "from console"}}}}},
		map[string]any{"startedAt": later, "completedAt": later + 5, "items": []any{
			map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "from the app"}}},
			map[string]any{"type": "agentMessage", "text": "ok"}}})
	// A deleted conversation stays deleted although Codex still has it.
	data.Threads = append(data.Threads, map[string]any{"id": "deleted", "cwd": work, "preview": "gone", "createdAt": 1792000000, "updatedAt": 1792000100})
	data.Turns["deleted"] = []map[string]any{{"startedAt": 1792000001, "items": []any{
		map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "x"}}}}}}
	if err := st.SetMeta(DismissedKey("codex", "", "deleted"), "x"); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(data)
	if err := os.WriteFile(fixture, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("db3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep := im.ImportAll(); rep.Imported != 0 || rep.Updated != 1 || rep.Errors != 0 {
		t.Fatalf("rescan after app turn = %+v", rep)
	}
	evs, _ = st.RecentEvents(s.ID, 100)
	var texts []string
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if txt, _ := p["text"].(string); txt != "" && (e.Kind == "user" || e.Kind == "assistant") {
			texts = append(texts, txt)
		}
	}
	if got := strings.Join(texts, "|"); got != "please fix|fixed|again|from console|from the app|ok" {
		t.Fatalf("after append = %s", got)
	}
	if _, err := st.FindNativeSession("codex", "deleted"); err == nil {
		t.Fatal("a conversation deleted in the console came back")
	}
}
