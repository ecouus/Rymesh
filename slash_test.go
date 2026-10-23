package console

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"rymesh/internal/store"
)

func TestLocalCommands(t *testing.T) {
	cfg := testConfig(t)
	st, _ := store.OpenStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &store.Session{Agent: "codex", Cwd: cfg.DataDir, Title: "old"}
	_ = st.CreateSession(sess)
	_, _ = st.UpdateSessionSettings(sess.ID, func(s *store.SessionSettings) { s.Model, s.Effort = "gpt-x", "high" })
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
