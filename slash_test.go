package console

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSlashCommandParsing(t *testing.T) {
	for in, want := range map[string][2]string{
		"/model sonnet":   {"model", "sonnet"},
		"  /Effort  high": {"effort", "high"},
		"/compact":        {"compact", ""},
	} {
		n, a, ok := slashCommand(in)
		if !ok || n != want[0] || a != want[1] {
			t.Fatalf("%q -> %q %q %v", in, n, a, ok)
		}
	}
	for _, in := range []string{"hello /model", "//comment", "/usr/bin/ls is a path", ""} {
		if _, _, ok := slashCommand(in); ok && in != "/usr/bin/ls is a path" {
			t.Fatalf("%q parsed as a command", in)
		}
	}
	// Only a change the CLI confirmed is saved.
	if st, ok := claudeSettingFrom("/model haiku", "Set model to `Haiku 5.5` for this session only"); !ok || st["model"] != "haiku" {
		t.Fatalf("confirmed model = %v %v", st, ok)
	}
	if _, ok := claudeSettingFrom("/model bogus", "Model 'bogus' not found"); ok {
		t.Fatal("rejected model was saved")
	}
	if st, ok := claudeSettingFrom("/effort auto", "Set effort level to auto"); !ok || st["effort"] != "" {
		t.Fatalf("effort auto = %v %v", st, ok)
	}
}

func TestSessionSettingsKeepImportState(t *testing.T) {
	st, _ := openStore(t.TempDir())
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	_ = st.CreateSession(sess)
	_ = st.SetImportState(sess.ID, ImportState{Path: "/x.jsonl", Offset: 42})
	if _, err := st.UpdateSessionSettings(sess.ID, func(s *SessionSettings) { s.Model = "opus" }); err != nil {
		t.Fatal(err)
	}
	_ = st.SetImportState(sess.ID, ImportState{Path: "/x.jsonl", Offset: 99})
	if got := st.GetSessionSettings(sess.ID); got.Model != "opus" {
		t.Fatalf("settings lost after import update: %+v", got)
	}
	if s := parseImportState(st.GetSessionMeta(sess.ID)); s.Offset != 99 {
		t.Fatalf("import state lost: %+v", s)
	}
}

func TestLocalCommands(t *testing.T) {
	cfg := testConfig(t)
	st, _ := openStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &Session{Agent: "codex", Cwd: cfg.DataDir, Title: "old"}
	_ = st.CreateSession(sess)
	_, _ = st.UpdateSessionSettings(sess.ID, func(s *SessionSettings) { s.Model, s.Effort = "gpt-x", "high" })
	run := func(text string) map[string]any {
		rec := httptest.NewRecorder()
		if !srv.handleLocalCommand(rec, sess, text) {
			return nil
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	// /clear: a new conversation in the same place, with the same settings.
	r := run("/clear")
	next, _ := r["session"].(map[string]any)
	if next == nil || next["cwd"] != cfg.DataDir || next["id"] == sess.ID {
		t.Fatalf("/clear = %v", r)
	}
	if got := st.GetSessionSettings(next["id"].(string)); got.Model != "gpt-x" || got.Effort != "high" {
		t.Fatalf("settings not carried over: %+v", got)
	}
	// Codex commands the runner implements are not answered here.
	if run("/model") != nil || run("/compact") != nil {
		t.Fatal("runner commands handled locally")
	}
	// Unknown commands are answered like the CLI does, without a task.
	if r := run("/frobnicate"); r == nil || r["local"] != true {
		t.Fatalf("unknown = %v", r)
	}
	evs, _ := st.RecentEvents(sess.ID, 10)
	if len(evs) != 2 || !strings.Contains(string(evs[1].Payload), "未知指令 /frobnicate") {
		t.Fatalf("transcript = %d events", len(evs))
	}
	if r := run("/rename 新名字"); r == nil {
		t.Fatal("codex /rename not handled")
	}
	if got, _ := st.GetSession(sess.ID); got.Title != "新名字" {
		t.Fatalf("title = %q", got.Title)
	}
	if run("hello") != nil {
		t.Fatal("plain text treated as a command")
	}
}

func TestSettingsReachTheCLI(t *testing.T) {
	old := agents
	agents = &AgentSet{at: time.Now(), agents: []AgentInfo{{Name: "claude", Binary: "/bin/echo", Available: true}}}
	defer func() { agents = old }()
	cmd, _ := buildAgentCommand(TaskSpec{Agent: "claude", Profile: "manual", IsNew: true, NativeID: "x",
		Model: "sonnet", Effort: "high"}, "/tmp")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--model sonnet") || !strings.Contains(args, "--effort high") {
		t.Fatalf("args = %s", args)
	}
	// Levels the CLI reports pass through; a malformed value never does.
	cmd, _ = buildAgentCommand(TaskSpec{Agent: "claude", Profile: "manual", IsNew: true, NativeID: "x", Effort: "--debug"}, "/tmp")
	if strings.Contains(strings.Join(cmd.Args, " "), "--effort") {
		t.Fatal("malformed effort passed to the CLI")
	}
}

func TestCodexCommands(t *testing.T) {
	models := map[string]any{"data": []any{
		map[string]any{"id": "gpt-a", "displayName": "GPT-A", "isDefault": true,
			"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "high"}}},
	}}
	start := func(prompt string) (*sessionRig, agentSession, string) {
		rig, s := newRig(t, "codex", "read-only", time.Minute)
		s.(*codexSession).spec.Prompt = prompt
		s.start()
		init := rig.next()
		rig.feed(s, map[string]any{"id": init["id"], "result": map[string]any{}})
		rig.next() // initialized
		events := filepath.Join(filepath.Dir(rig.control), "events.jsonl")
		return rig, s, events
	}
	read := func(path string) string { b, _ := os.ReadFile(path); return string(b) }

	rig, s, ev := start("/model gpt-a high")
	req := rig.next()
	if req["method"] != "model/list" {
		t.Fatalf("expected model/list, got %v", req)
	}
	rig.feed(s, map[string]any{"id": req["id"], "result": models})
	if st, _, ok := s.outcome(); !ok || st != StatusCompleted {
		t.Fatal("command did not finish")
	}
	if log := read(ev); !strings.Contains(log, `"session_settings":{"effort":"high","model":"gpt-a"}`) || !strings.Contains(log, "已切换到 GPT-A") {
		t.Fatalf("events = %s", log)
	}

	rig, s, ev = start("/model gpt-a ultra")
	req = rig.next()
	rig.feed(s, map[string]any{"id": req["id"], "result": models})
	if log := read(ev); strings.Contains(log, "session_settings") || !strings.Contains(log, "不支持推理强度 ultra") {
		t.Fatalf("unsupported effort accepted: %s", log)
	}

	_, s, ev = start("/effort low")
	if _, _, ok := s.outcome(); !ok || !strings.Contains(read(ev), `"session_settings":{"effort":"low"}`) {
		t.Fatalf("effort: %s", read(ev))
	}
	_, s, ev = start("/status")
	if _, _, ok := s.outcome(); !ok || !strings.Contains(read(ev), "只读") {
		t.Fatalf("status: %s", read(ev))
	}
	_ = http.StatusOK
}

// The turn's collaborationMode settings must carry the effort: left empty
// they reset it, and the separate "effort" parameter is silently ignored.
func TestCodexTurnCarriesModelAndEffort(t *testing.T) {
	rig, s := newRig(t, "codex", "read-only", time.Minute)
	cs := s.(*codexSession)
	cs.spec.Model, cs.spec.Effort = "gpt-a", "low"
	s.start()
	init := rig.next()
	rig.feed(s, map[string]any{"id": init["id"], "result": map[string]any{}})
	rig.next() // initialized
	ts := rig.next()
	if ts["params"].(map[string]any)["model"] != "gpt-a" {
		t.Fatalf("thread/start = %v", ts)
	}
	rig.feed(s, map[string]any{"id": ts["id"], "result": map[string]any{"thread": map[string]any{"id": "th"}, "model": "gpt-a"}})
	<-rig.nativeID
	p := rig.next()["params"].(map[string]any)
	settings := p["collaborationMode"].(map[string]any)["settings"].(map[string]any)
	if p["model"] != "gpt-a" || p["effort"] != "low" || settings["reasoning_effort"] != "low" || settings["model"] != "gpt-a" {
		t.Fatalf("turn/start params = %v", p)
	}
}

func TestValidEffortFollowsTheCLI(t *testing.T) {
	// Levels come from the CLI's model list; a new one passes unchanged.
	for _, e := range []string{"low", "xhigh", "max", "ultra", "high-2"} {
		if !validEffort(e) {
			t.Errorf("%q rejected", e)
		}
	}
	for _, e := range []string{"", "HIGH", "a b", "--model", "x;rm", strings.Repeat("a", 30)} {
		if validEffort(e) {
			t.Errorf("%q accepted", e)
		}
	}
}

func TestModelQueryTakesItsChildrenDown(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	// A CLI that starts a helper (an MCP server, say) and keeps running.
	_, _, cleanup, err := startModelQuery(t.Context(), "/bin/sh", "-c", "sleep 60 & echo $! > "+pidFile+"; sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 50 && child == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if child == 0 {
		t.Fatal("helper did not start")
	}
	cleanup()
	for i := 0; i < 50 && processAlive(child); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(child) {
		t.Fatalf("helper %d outlived the query", child)
	}
}
