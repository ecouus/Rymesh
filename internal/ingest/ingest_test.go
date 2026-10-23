package ingest

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rymesh/internal/store"
	"rymesh/internal/sys"
)

func TestReconcileClassifiesLostTasksAsInterrupted(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &store.Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	// A task from a previous boot whose runner cannot exist any more.
	lost := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p",
		Status: store.StatusRunning, BootID: "boot-id-from-a-previous-boot", PID: 999999}
	if err := st.CreateTask(lost); err != nil {
		t.Fatal(err)
	}
	// A pending task must stay pending; reconcile never starts or replays it.
	pending := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "q", Status: store.StatusPending}
	if err := st.CreateTask(pending); err != nil {
		t.Fatal(err)
	}
	ing := NewIngestor(st, dataDir, log.New(io.Discard, "", 0))
	ing.Reconcile()

	got, err := st.GetTask(lost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusInterrupted || !strings.Contains(got.Error, "服务器已重启") {
		t.Fatalf("lost task = status %q error %q", got.Status, got.Error)
	}
	gotP, _ := st.GetTask(pending.ID)
	if gotP.Status != store.StatusPending {
		t.Fatalf("pending task changed to %q", gotP.Status)
	}
	// No replacement task may be created automatically.
	tasks, _ := st.ListTasks(nil, 100)
	if len(tasks) != 2 {
		t.Fatalf("automatic retry created tasks: %d", len(tasks))
	}
}

func TestIngestorConsumesOversizedLines(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &store.Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: store.StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	ing := NewIngestor(st, dataDir, log.New(io.Discard, "", 0))
	dir := ing.TaskDir(task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(v map[string]any) string {
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	// A line above the per-pass batch budget must still be ingested, and one
	// above the hard line limit must be skipped, not wedge the reader.
	content := line(map[string]any{"seq": 1, "stream": "notice", "text": "before"}) +
		line(map[string]any{"seq": 2, "stream": "text", "text": strings.Repeat("a", 3<<20)}) +
		line(map[string]any{"seq": 3, "stream": "text", "text": strings.Repeat("b", ingestMaxLineBytes)}) +
		line(map[string]any{"seq": 4, "stream": "notice", "text": "after"}) +
		line(map[string]any{"seq": 5, "stream": "exit", "exit": map[string]any{
			"code": 0, "reason": "exited", "status": store.StatusCompleted}})
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var offset int64
	sawExit := false
	for i := 0; i < 20; i++ {
		advanced, exited := ing.ReadOnce(task.ID, &offset)
		sawExit = sawExit || exited
		if !advanced {
			break
		}
	}
	if offset != int64(len(content)) || !sawExit {
		t.Fatalf("offset %d of %d, sawExit %v", offset, len(content), sawExit)
	}
	evs, err := st.EventsAfter(sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range evs {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		texts = append(texts, e.Kind+":"+store.TruncateTitle(p.Text, 12))
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"notice:before", "text:aaa", "notice:一条事件过大", "notice:after"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	got, _ := st.GetTask(task.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("task status %q", got.Status)
	}
}

func TestReadEventLineWaitsForNewline(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("complete\npartial"), 16)
	line, n, ok := readEventLine(br, 1<<20)
	if !ok || string(line) != "complete\n" || n != 9 {
		t.Fatalf("first line = %q %d %v", line, n, ok)
	}
	if _, _, ok := readEventLine(br, 1<<20); ok {
		t.Fatal("partial line reported as complete")
	}
}

func TestNormalizeEvents(t *testing.T) {
	claude := map[string]any{
		"type": "assistant",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
	}
	evs := normalizeClaude(claude)
	if len(evs) != 2 || evs[0].Kind != "assistant" || evs[1].Kind != "tool" {
		t.Fatalf("claude normalize = %+v", evs)
	}

	codex := map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "command_execution", "id": "c1", "status": "completed",
			"command": "echo hi", "aggregated_output": "hi", "exit_code": 0},
	}
	evs = normalizeCodex(codex)
	if len(evs) != 1 || evs[0].Kind != "tool" {
		t.Fatalf("codex normalize = %+v", evs)
	}
	payload := evs[0].Payload.(map[string]any)
	if payload["name"] != "shell" {
		t.Fatalf("codex tool payload = %+v", payload)
	}
}

// Reconcile must not declare a task interrupted when launch() marked it running
// but has not recorded the runner PID yet: there is no evidence it is gone.
func TestReconcileKeepsTaskWithoutPID(t *testing.T) {
	data := t.TempDir()
	st, err := store.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &store.Session{Agent: "claude", Cwd: data, Title: "r"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: store.StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 0, 0, sys.CurrentBootID()); err != nil {
		t.Fatal(err)
	}
	ing := NewIngestor(st, data, log.New(io.Discard, "", 0))
	ing.Reconcile()
	got, _ := st.GetTask(task.ID)
	if got.Status == store.StatusInterrupted {
		t.Fatalf("task without a recorded runner PID was declared interrupted: %q", got.Error)
	}
}

// A task from a previous boot must be interrupted even if its PID was reused.
func TestReconcileInterruptsPreviousBoot(t *testing.T) {
	data := t.TempDir()
	st, err := store.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &store.Session{Agent: "claude", Cwd: data, Title: "r"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: store.StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, os.Getpid(), os.Getpid(), "boot-from-before"); err != nil {
		t.Fatal(err)
	}
	ing := NewIngestor(st, data, log.New(io.Discard, "", 0))
	ing.Reconcile()
	got, _ := st.GetTask(task.ID)
	if got.Status != store.StatusInterrupted {
		t.Fatalf("previous-boot task = %s, want interrupted", got.Status)
	}
}
