package console

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"rymesh/internal/agent"
	"rymesh/internal/config"
	"rymesh/internal/ingest"
	"rymesh/internal/store"
	"rymesh/internal/sys"
)

// An event that fails to reach the database holds the read offset, so the
// next pass stores it instead of skipping it for good.
func TestIngestHoldsOffsetOnStoreError(t *testing.T) {
	cfg := testConfig(t)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ing := ingest.NewIngestor(st, cfg.DataDir, log.New(io.Discard, "", 0))
	sess := &store.Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	_ = st.CreateSession(sess)
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: store.StatusRunning}
	_ = st.CreateTask(task)
	line := func(seq int, text string) string {
		return fmt.Sprintf(`{"seq":%d,"ts":"2026-01-01T00:00:00Z","stream":"notice","text":%q}`+"\n", seq, text)
	}
	one, two := line(1, "one"), line(2, "two")
	dir := ing.TaskDir(task.ID)
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(one+two), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`CREATE TRIGGER boom BEFORE INSERT ON events WHEN NEW.task_seq = 2
		BEGIN SELECT RAISE(FAIL, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	var off int64
	if adv, _ := ing.ReadOnce(task.ID, &off); !adv || off != int64(len(one)) {
		t.Fatalf("first pass: advanced=%v offset=%d, want %d", adv, off, len(one))
	}
	if adv, _ := ing.ReadOnce(task.ID, &off); adv || off != int64(len(one)) {
		t.Fatalf("while failing: advanced=%v offset=%d", adv, off)
	}
	if got, _ := st.GetTask(task.ID); got.IngestOffset != int64(len(one)) {
		t.Fatalf("saved offset = %d", got.IngestOffset)
	}
	if _, err := st.DB.Exec(`DROP TRIGGER boom`); err != nil {
		t.Fatal(err)
	}
	if adv, _ := ing.ReadOnce(task.ID, &off); !adv || off != int64(len(one+two)) {
		t.Fatalf("retry: advanced=%v offset=%d", adv, off)
	}
	evs, _ := st.RecentEvents(sess.ID, 10)
	var all string
	for _, e := range evs {
		all += string(e.Payload)
	}
	if strings.Count(all, `"one"`) != 1 || strings.Count(all, `"two"`) != 1 {
		t.Fatalf("events = %s", all)
	}
}

// spawn starts a process in its own group and reports when it exits.
func spawn(t *testing.T, args ...string) (pid int, exited <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	// ps may not see the new command line for a moment.
	for i := 0; i < 50; i++ {
		if cl, _ := sys.ProcessCmdline(cmd.Process.Pid); strings.Contains(cl, args[len(args)-1]) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd.Process.Pid, done
}

func waitExit(exited <-chan struct{}, d time.Duration) bool {
	select {
	case <-exited:
		return true
	case <-time.After(d):
		return false
	}
}

// Signals go only to a process that is still what was recorded: cancelling
// signals the task's runner, never a process that reused its PID, and the
// orphan sweep stops the agent, never anything else (macOS has no /proc).
func TestProcessIdentityBeforeSignals(t *testing.T) {
	other, otherExited := spawn(t, "sleep", "31337")
	if cl, err := sys.ProcessCmdline(other); err != nil || !strings.Contains(cl, "sleep 31337") {
		t.Fatalf("cmdline = %q %v", cl, err)
	}
	killTask(&store.Task{ID: "tid-abc", PID: other}, syscall.SIGTERM)
	if waitExit(otherExited, 300*time.Millisecond) {
		t.Fatal("killTask signalled a process that is not the runner")
	}
	runner, runnerExited := spawn(t, "sh", "-c", "sleep 30; :", "run-task", "tasks/tid-abc")
	killTask(&store.Task{ID: "tid-abc", PID: runner}, syscall.SIGTERM)
	if !waitExit(runnerExited, 3*time.Second) {
		t.Fatal("runner not signalled")
	}

	cfg := testConfig(t)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ing := ingest.NewIngestor(st, cfg.DataDir, log.New(io.Discard, "", 0))
	task := &store.Task{SessionID: "s", Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: store.StatusRunning}
	_ = st.CreateTask(task)
	_ = os.MkdirAll(ing.TaskDir(task.ID), 0o700)
	orphan := func(pid int) {
		_ = os.WriteFile(filepath.Join(ing.TaskDir(task.ID), "child.pid"), []byte(fmt.Sprintf("%d\n", pid)), 0o600)
		ing.StopOrphanChild(task.ID)
	}
	stranger, strangerExited := spawn(t, "sleep", "31338")
	orphan(stranger)
	if waitExit(strangerExited, 300*time.Millisecond) {
		t.Fatal("orphan sweep stopped an unrelated process")
	}
	agent, agentExited := spawn(t, "sh", "-c", "sleep 30; :", "claude")
	orphan(agent)
	if !waitExit(agentExited, 3*time.Second) {
		t.Fatal("orphan agent left running")
	}
}

// Config changes are serialized: none of many concurrent ones is lost, and
// the file on disk is always whole.
func TestConfigWritesSerialize(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	st, _ := store.OpenStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, cfgPath, st, log.New(io.Discard, "", 0), nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = srv.saveNodes(func(nodes []config.NodeConfig) ([]config.NodeConfig, error) {
				return append(nodes, config.NodeConfig{ID: fmt.Sprintf("d%d", i), Name: "n", URL: "http://x", Token: "t"}), nil
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			c := srv.updateConfig(func(c *config.Config) { c.MaxConcurrent = 1 + i%8 })
			if err := config.SaveConfigFile(cfgPath, c); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(srv.config().Nodes); n != 20 {
		t.Fatalf("devices after concurrent changes = %d, want 20", n)
	}
	if _, err := config.LoadConfig(cfgPath); err != nil {
		t.Fatalf("config file damaged: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// An export file names its own scrypt cost; only the one exports use is
// accepted, so a damaged file cannot make the import take gigabytes.
func TestBundleRejectsOtherScryptCost(t *testing.T) {
	code, _ := newBundleCode()
	b, err := sealBundle(bundlePayload{}, code, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openBundle(b, code); err != nil {
		t.Fatalf("valid bundle: %v", err)
	}
	var f map[string]any
	_ = json.Unmarshal(b, &f)
	f["scrypt_n"] = 1 << 20
	bad, _ := json.Marshal(f)
	start := time.Now()
	if _, err := openBundle(bad, code); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("costly N accepted: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("key derivation ran before the cost was checked")
	}
}

// Two devices answering one request at once: exactly one answer is taken,
// and the answer the agent reads is the one recorded.
func TestResolvePermissionOnce(t *testing.T) {
	cfg := testConfig(t)
	st, _ := store.OpenStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &store.Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	_ = st.CreateSession(sess)
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: store.StatusWaitingInput}
	_ = st.CreateTask(task)
	p := &store.Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "Bash", Status: "pending", Kind: store.KindPermission}
	_ = st.CreatePermission(p)
	tok := srv.auth.token()
	info, _ := srv.auth.verifyToken(tok)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, d := range []string{"allow", "deny"} {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/api/permissions/"+p.ID, strings.NewReader(`{"decision":"`+d+`"}`))
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
			req.Header.Set("X-CSRF-Token", info.CSRF)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			codes <- rec.Code
		}(d)
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	if !(got[0] == 200 && got[1] == 409 || got[0] == 409 && got[1] == 200) {
		t.Fatalf("codes = %v, want one 200 and one 409", got)
	}
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "tasks", task.ID, "control", "resp-"+p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp agent.CtlResponse
	_ = json.Unmarshal(b, &resp)
	rec, _ := st.GetPermission(p.ID)
	if rec.Status != agent.StatusFor(resp.Decision) {
		t.Fatalf("agent got %q, recorded %q", resp.Decision, rec.Status)
	}
}
