package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionRig wires an agent session to in-memory stdin and a temp control dir,
// playing the agent (feed) and the web service (answer).
type sessionRig struct {
	t        *testing.T
	control  string
	sent     chan map[string]any
	stop     chan struct{}
	nativeID chan string
}

func newRig(t *testing.T, agent, profile string, timeout time.Duration) (*sessionRig, agentSession) {
	t.Helper()
	dir := t.TempDir()
	control := filepath.Join(dir, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := newEventWriter(filepath.Join(dir, "events.jsonl"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	pr, pw := io.Pipe()
	rig := &sessionRig{t: t, control: control, sent: make(chan map[string]any, 32),
		stop: make(chan struct{}), nativeID: make(chan string, 4)}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				rig.sent <- m
			}
		}
	}()
	t.Cleanup(func() { close(rig.stop); pw.Close() })
	spec := TaskSpec{Agent: agent, Profile: profile, Cwd: dir, Prompt: "hello", IsNew: true,
		PermTimeout: int(timeout / time.Second)}
	var sess agentSession
	if agent == "codex" {
		sess = newCodexSession(spec, control, w, pw, rig.stop, func(id string) { rig.nativeID <- id }, timeout)
	} else {
		sess = &claudeSession{spec: spec, control: control, w: w, stdin: pw, stop: rig.stop,
			onNativeID: func(id string) { rig.nativeID <- id }, timeout: timeout}
	}
	return rig, sess
}

func (r *sessionRig) next() map[string]any {
	r.t.Helper()
	select {
	case m := <-r.sent:
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("agent stdin: nothing sent")
	}
	return nil
}

func (r *sessionRig) feed(s agentSession, v any) {
	b, _ := json.Marshal(v)
	s.onLine(b)
}

// request waits for the runner to publish a request, as the ingestor would.
func (r *sessionRig) request() ctlRequest {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ents, _ := os.ReadDir(r.control)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "req-") {
				b, _ := os.ReadFile(filepath.Join(r.control, e.Name()))
				var req ctlRequest
				if json.Unmarshal(b, &req) == nil && req.ID != "" {
					if _, err := os.Stat(filepath.Join(r.control, "done-"+req.ID+".json")); err != nil {
						return req
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("no pending request published")
	return ctlRequest{}
}

func (r *sessionRig) answer(id string, resp ctlResponse) {
	if err := atomicWriteJSON(filepath.Join(r.control, "resp-"+id+".json"), resp); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sessionRig) done(id string) ctlDone {
	r.t.Helper()
	var d ctlDone
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(r.control, "done-"+id+".json")); err == nil && json.Unmarshal(b, &d) == nil {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("no done file")
	return d
}

func optionIDs(req ctlRequest) string {
	var ids []string
	for _, o := range req.Options {
		ids = append(ids, o.ID)
	}
	return strings.Join(ids, ",")
}

func claudeControl(id, tool string, input any, suggestions any) map[string]any {
	req := map[string]any{"subtype": "can_use_tool", "tool_name": tool, "input": input, "tool_use_id": "tu-" + id}
	if suggestions != nil {
		req["permission_suggestions"] = suggestions
	}
	return map[string]any{"type": "control_request", "request_id": id, "request": req}
}

func controlReply(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if m["type"] != "control_response" {
		t.Fatalf("expected control_response, got %v", m)
	}
	resp := m["response"].(map[string]any)
	if resp["subtype"] != "success" {
		t.Fatalf("control response not success: %v", resp)
	}
	return resp["response"].(map[string]any)
}

func TestClaudeSessionPermissionChoices(t *testing.T) {
	rig, s := newRig(t, "claude", "manual", time.Minute)
	s.start()
	if first := rig.next(); first["type"] != "user" {
		t.Fatalf("first message = %v", first)
	}
	sugg := []any{
		map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "npm test:*"}},
			"behavior": "allow", "destination": "localSettings"},
		map[string]any{"type": "addDirectories", "directories": []any{"/w"}, "destination": "session"},
		map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"},
	}
	cases := []struct {
		decision string
		check    func(map[string]any)
	}{
		{decisionAllowSession, func(r map[string]any) {
			perms := r["updatedPermissions"].([]any)
			for _, p := range perms {
				pm := p.(map[string]any)
				if pm["destination"] != "session" || pm["type"] == "setMode" {
					t.Fatalf("session allow must grant rules only, for the session: %v", perms)
				}
			}
		}},
		{decisionAllowMode, func(r map[string]any) {
			perms := r["updatedPermissions"].([]any)
			if len(perms) != 1 || perms[0].(map[string]any)["mode"] != "acceptEdits" {
				t.Fatalf("mode allow = %v", perms)
			}
		}},
		{decisionAllowAlways, func(r map[string]any) {
			if r["updatedPermissions"].([]any)[0].(map[string]any)["destination"] != "localSettings" {
				t.Fatalf("always allow lost the destination: %v", r)
			}
		}},
		{decisionDenyStop, func(r map[string]any) {
			if r["behavior"] != "deny" || r["interrupt"] != true {
				t.Fatalf("deny_stop = %v", r)
			}
			// The reply is sent before stopped is recorded; give it a moment.
			time.Sleep(50 * time.Millisecond)
			if st, _, ok := s.outcome(); !ok || st != StatusCancelled {
				t.Fatalf("deny_stop outcome = %s %v", st, ok)
			}
		}},
	}
	for i, c := range cases {
		rig.feed(s, claudeControl(string(rune('a'+i)), "Bash", map[string]any{"command": "npm test"}, sugg))
		req := rig.request()
		if req.Kind != kindPermission || optionIDs(req) != "allow,allow_session,allow_mode,allow_always,deny,deny_stop" {
			t.Fatalf("options = %s (%s)", optionIDs(req), req.Kind)
		}
		if o, _ := req.option(decisionAllowAlways); !strings.Contains(o.Detail, "Bash(npm test:*)") {
			t.Fatalf("always detail = %q", o.Detail)
		}
		rig.answer(req.ID, ctlResponse{Decision: c.decision})
		c.check(controlReply(t, rig.next()))
		rig.done(req.ID)
	}
}

func TestClaudeSessionQuestionAndPlan(t *testing.T) {
	rig, s := newRig(t, "claude", "plan", time.Minute)
	q := map[string]any{"questions": []any{map[string]any{"question": "颜色？", "header": "颜色", "multiSelect": true,
		"options": []any{map[string]any{"label": "红"}, map[string]any{"label": "蓝"}}}}}
	rig.feed(s, claudeControl("q1", "AskUserQuestion", q, nil))
	req := rig.request()
	if req.Kind != kindQuestion || len(req.Questions) != 1 || !req.Questions[0].Multi || !req.Questions[0].Other {
		t.Fatalf("question request = %+v", req)
	}
	rig.answer(req.ID, ctlResponse{Decision: decisionAnswer, Answers: map[string][]string{"颜色？": {"红", "紫色（自填）"}}})
	r := controlReply(t, rig.next())
	ans := r["updatedInput"].(map[string]any)["answers"].(map[string]any)
	if r["behavior"] != "allow" || ans["颜色？"] != "红, 紫色（自填）" {
		t.Fatalf("answer reply = %v", r)
	}
	if d := rig.done(req.ID); d.Decision != "answered" {
		t.Fatalf("done = %+v", d)
	}

	rig.feed(s, claudeControl("p1", "ExitPlanMode", map[string]any{"plan": "# 计划"}, nil))
	req = rig.request()
	if req.Kind != kindPlan || req.Plan != "# 计划" {
		t.Fatalf("plan request = %+v", req)
	}
	rig.answer(req.ID, ctlResponse{Decision: decisionPlanAccept})
	r = controlReply(t, rig.next())
	perm := r["updatedPermissions"].([]any)[0].(map[string]any)
	if r["behavior"] != "allow" || perm["type"] != "setMode" || perm["mode"] != "acceptEdits" {
		t.Fatalf("plan approval = %v", r)
	}

	rig.feed(s, claudeControl("p2", "ExitPlanMode", map[string]any{"plan": "x"}, nil))
	req = rig.request()
	rig.answer(req.ID, ctlResponse{Decision: decisionPlanRevise, Message: "再细一点"})
	if r = controlReply(t, rig.next()); r["behavior"] != "deny" || r["message"] != "再细一点" {
		t.Fatalf("plan revise = %v", r)
	}
}

func TestClaudeSessionRequestExpires(t *testing.T) {
	rig, s := newRig(t, "claude", "manual", 0)
	s.(*claudeSession).timeout = 100 * time.Millisecond
	rig.feed(s, claudeControl("x", "Bash", map[string]any{"command": "rm -rf /"}, nil))
	r := controlReply(t, rig.next())
	if r["behavior"] != "deny" || r["message"] != "审批超时" {
		t.Fatalf("expired reply = %v", r)
	}
	// The done record follows the reply; wait for it so the temp dir cleanup
	// does not race the write.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := filepath.Glob(filepath.Join(rig.control, "done-*.json")); len(m) == 1 {
			b, _ := os.ReadFile(m[0])
			if !strings.Contains(string(b), `"expired"`) {
				t.Fatalf("done = %s", b)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no done record for the expired request")
}

func TestCodexSessionHandshakeApprovalAndQuestion(t *testing.T) {
	rig, s := newRig(t, "codex", "plan", time.Minute)
	s.start()
	init := rig.next()
	if init["method"] != "initialize" {
		t.Fatalf("first = %v", init)
	}
	rig.feed(s, map[string]any{"id": init["id"], "result": map[string]any{}})
	if m := rig.next(); m["method"] != "initialized" {
		t.Fatalf("expected initialized, got %v", m)
	}
	ts := rig.next()
	p := ts["params"].(map[string]any)
	if ts["method"] != "thread/start" || p["sandbox"] != "read-only" || p["approvalPolicy"] != "on-request" {
		t.Fatalf("thread/start = %v", ts)
	}
	rig.feed(s, map[string]any{"id": ts["id"], "result": map[string]any{"thread": map[string]any{"id": "th-1"}, "model": "gpt-x"}})
	if id := <-rig.nativeID; id != "th-1" {
		t.Fatalf("native id = %s", id)
	}
	turn := rig.next()
	cm := turn["params"].(map[string]any)["collaborationMode"].(map[string]any)
	if turn["method"] != "turn/start" || cm["mode"] != "plan" || cm["settings"].(map[string]any)["model"] != "gpt-x" {
		t.Fatalf("turn/start = %v", turn)
	}

	amend := map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []any{"npm", "test"}}}
	rig.feed(s, map[string]any{"id": 77, "method": "item/commandExecution/requestApproval", "params": map[string]any{
		"itemId": "i1", "command": "npm test", "reason": "需要运行测试",
		"availableDecisions": []any{"accept", amend, "acceptForSession", "decline", "cancel"}}})
	req := rig.request()
	if optionIDs(req) != "allow,allow_always,allow_session,deny,deny_stop" || req.Description != "需要运行测试" {
		t.Fatalf("codex options = %s / %q", optionIDs(req), req.Description)
	}
	rig.answer(req.ID, ctlResponse{Decision: decisionAllowAlways})
	reply := rig.next()
	dec, _ := json.Marshal(reply["result"].(map[string]any)["decision"])
	if reply["id"] != float64(77) || !strings.Contains(string(dec), "execpolicy_amendment") {
		t.Fatalf("always reply = %v", reply)
	}

	rig.feed(s, map[string]any{"id": 78, "method": "item/tool/requestUserInput", "params": map[string]any{
		"itemId": "i2", "isBlocking": true, "questions": []any{map[string]any{"id": "fmt", "header": "格式",
			"question": "JSON 还是 YAML？", "isOther": true, "options": []any{map[string]any{"label": "JSON", "description": "严格"}}}}}})
	req = rig.request()
	if req.Kind != kindQuestion || req.Questions[0].ID != "fmt" || !req.Questions[0].Other {
		t.Fatalf("codex question = %+v", req)
	}
	rig.answer(req.ID, ctlResponse{Decision: decisionAnswer, Answers: map[string][]string{"fmt": {"TOML"}}})
	reply = rig.next()
	got := reply["result"].(map[string]any)["answers"].(map[string]any)["fmt"].(map[string]any)["answers"].([]any)
	if len(got) != 1 || got[0] != "TOML" {
		t.Fatalf("answers reply = %v", reply)
	}

	rig.feed(s, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{
		"status": "failed", "error": map[string]any{"message": "quota"}}}})
	if st, msg, ok := s.outcome(); !ok || st != StatusFailed || msg != "quota" {
		t.Fatalf("outcome = %s %q %v", st, msg, ok)
	}
}

func TestNormalizeCodexRPC(t *testing.T) {
	ev := func(method string, params map[string]any) []normEvent {
		return normalizeCodex(map[string]any{"method": method, "params": params})
	}
	out := ev("item/completed", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "c1",
		"command": "ls", "status": "completed", "aggregatedOutput": "a.txt", "exitCode": 0}})
	if len(out) != 1 || out[0].Kind != "tool" || out[0].Payload.(map[string]any)["output"] != "a.txt" {
		t.Fatalf("command = %+v", out)
	}
	if out = ev("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "hi"}}); out[0].Kind != "assistant" {
		t.Fatalf("message = %+v", out)
	}
	if out = ev("item/completed", map[string]any{"item": map[string]any{"type": "plan", "text": "# p"}}); out[0].Kind != "plan" {
		t.Fatalf("plan = %+v", out)
	}
	if out = ev("item/started", map[string]any{"item": map[string]any{"type": "agentMessage", "text": ""}}); len(out) != 0 {
		t.Fatalf("started message should be silent: %+v", out)
	}
	// Exec-format events from older task logs still normalize.
	if out = normalizeCodex(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "old"}}); out[0].Kind != "assistant" {
		t.Fatalf("legacy = %+v", out)
	}
}

func TestResolveValidatesChoicesAndRecordsLabel(t *testing.T) {
	cfg := testConfig(t)
	st, err := openStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	_ = st.CreateSession(sess)
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: StatusWaitingInput}
	_ = st.CreateTask(task)
	perm := ctlRequest{Options: []ctlOption{{ID: decisionAllow, Label: "允许"},
		{ID: decisionAllowSession, Label: "本会话允许", Detail: "Bash(ls)"}}}
	q := ctlRequest{Questions: []ctlQuestion{{ID: "c", Header: "颜色", Question: "颜色？"}}}
	p1 := &Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "Bash", Status: "pending", Kind: kindPermission, Detail: perm.detail()}
	p2 := &Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "AskUserQuestion", Status: "pending", Kind: kindQuestion, Detail: q.detail()}
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
