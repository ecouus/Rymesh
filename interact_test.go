package console

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rymesh/internal/agent"
	"rymesh/internal/store"
)

func TestResolveValidatesChoicesAndRecordsLabel(t *testing.T) {
	cfg := testConfig(t)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &store.Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	_ = st.CreateSession(sess)
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: store.StatusWaitingInput}
	_ = st.CreateTask(task)
	perm := agent.CtlRequest{Options: []agent.CtlOption{{ID: agent.DecisionAllow, Label: "允许"},
		{ID: agent.DecisionAllowSession, Label: "本会话允许", Detail: "Bash(ls)"}}}
	q := agent.CtlRequest{Questions: []agent.CtlQuestion{{ID: "c", Header: "颜色", Question: "颜色？"}}}
	p1 := &store.Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "Bash", Status: "pending", Kind: store.KindPermission, Detail: perm.Detail()}
	p2 := &store.Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "AskUserQuestion", Status: "pending", Kind: store.KindQuestion, Detail: q.Detail()}
	_ = st.CreatePermission(p1)
	_ = st.CreatePermission(p2)
	tok := srv.auth.token()
	info, _ := srv.auth.verifyToken(tok)
	post := func(id, body string) int {
		req := httptest.NewRequest("POST", "/api/permissions/"+id, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		req.Header.Set("X-CSRF-Token", info.CSRF)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(p1.ID, `{"decision":"allow_always"}`); c != 400 {
		t.Fatalf("option not offered must be rejected: %d", c)
	}
	if c := post(p2.ID, `{"decision":"answer"}`); c != 400 {
		t.Fatalf("empty answer must be rejected: %d", c)
	}
	if c := post(p1.ID, `{"decision":"allow_session"}`); c != 200 {
		t.Fatalf("allow_session: %d", c)
	}
	if c := post(p2.ID, `{"decision":"answer","answers":{"c":["蓝"]}}`); c != 200 {
		t.Fatalf("answer: %d", c)
	}
	evs, _ := st.RecentEvents(sess.ID, 10)
	var all string
	for _, e := range evs {
		all += string(e.Payload)
	}
	if !strings.Contains(all, `"label":"本会话允许"`) || !strings.Contains(all, `颜色：蓝`) || !strings.Contains(all, `"decision":"answered"`) {
		t.Fatalf("result events = %s", all)
	}
	if got, _ := st.GetPermission(p2.ID); got.Status != "answered" {
		t.Fatalf("question status = %s", got.Status)
	}
}
