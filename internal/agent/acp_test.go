package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"rymesh/internal/store"
)

// A Hermes turn as `hermes acp` plays it (recorded from Hermes Agent 0.22):
// handshake, a new session switched to the chosen mode, streamed text, a tool
// call, an edit approval and the end of the turn.
func TestACPSessionTurn(t *testing.T) {
	rig, s := newRig(t, "hermes", "accept_edits", time.Minute)
	go s.start()
	init := rig.next()
	if init["method"] != "initialize" {
		t.Fatalf("first = %v", init)
	}
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{
		"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true,
			"sessionCapabilities": map[string]any{"resume": map[string]any{}}}}})
	nw := rig.next()
	if nw["method"] != "session/new" {
		t.Fatalf("second = %v", nw)
	}
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": nw["id"], "result": map[string]any{
		"sessionId": "sess-1",
		"modes": map[string]any{"currentModeId": "default", "availableModes": []any{
			map[string]any{"id": "default"}, map[string]any{"id": "accept_edits"}}}}})
	if id := <-rig.nativeID; id != "sess-1" {
		t.Fatalf("native id = %q", id)
	}
	mode := rig.next()
	if mode["method"] != "session/set_mode" || mode["params"].(map[string]any)["modeId"] != "accept_edits" {
		t.Fatalf("mode = %v", mode)
	}
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": mode["id"], "result": map[string]any{}})
	prompt := rig.next()
	if prompt["method"] != "session/prompt" {
		t.Fatalf("prompt = %v", prompt)
	}
	upd := func(u map[string]any) {
		rig.feed(s, map[string]any{"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": "sess-1", "update": u}})
	}
	upd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Writ"}})
	upd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ing it."}})
	upd(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-1", "kind": "edit", "title": "write_file: /tmp/x",
		"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "Preparing"}}}})
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{
		"sessionId": "sess-1",
		"options": []any{
			map[string]any{"kind": "allow_once", "name": "Allow edit", "optionId": "allow_once"},
			map[string]any{"kind": "reject_once", "name": "Deny", "optionId": "deny"}},
		"toolCall": map[string]any{"toolCallId": "edit-approval-1", "title": "Approve edit: /tmp/x", "kind": "edit",
			"rawInput": map[string]any{"tool": "write_file", "arguments": map[string]any{"path": "/tmp/x", "content": "hi"}}}}})
	req := rig.request()
	if req.ToolName != "write_file" || req.Description != "Approve edit: /tmp/x" || optionIDs(req) != "allow,deny" {
		t.Fatalf("request = %+v", req)
	}
	var in map[string]any
	if json.Unmarshal(req.Input, &in) != nil || in["path"] != "/tmp/x" {
		t.Fatalf("input = %s", req.Input)
	}
	rig.answer(req.ID, CtlResponse{Decision: DecisionAllow})
	reply := rig.next()
	outcome := reply["result"].(map[string]any)["outcome"].(map[string]any)
	if reply["id"] != float64(0) || outcome["outcome"] != "selected" || outcome["optionId"] != "allow_once" {
		t.Fatalf("permission reply = %v", reply)
	}
	if d := rig.done(req.ID); d.Decision != "allowed" {
		t.Fatalf("done = %+v", d)
	}
	upd(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "edit-approval-1", "status": "completed"})
	upd(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tc-1", "status": "completed",
		"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "written"}}}})
	upd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "\n\ndone"}})
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": prompt["id"], "result": map[string]any{
		"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 2}}})
	if st, _, ok := s.outcome(); !ok || st != store.StatusCompleted {
		t.Fatalf("outcome = %q %v", st, ok)
	}

	b, _ := os.ReadFile(rig.events)
	var kinds []string
	var texts []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var env struct {
			Payload struct {
				ACP     string         `json:"acp"`
				Payload map[string]any `json:"payload"`
			} `json:"payload"`
		}
		_ = json.Unmarshal([]byte(line), &env)
		if env.Payload.ACP == "" {
			continue
		}
		kinds = append(kinds, env.Payload.ACP)
		if env.Payload.ACP == "assistant" {
			texts = append(texts, env.Payload.Payload["text"].(string))
		}
		if env.Payload.ACP == "tool" && env.Payload.Payload["status"] == "completed" &&
			env.Payload.Payload["output"] != "written" {
			t.Fatalf("tool result = %v", env.Payload.Payload)
		}
	}
	if got := strings.Join(kinds, ","); got != "assistant,tool,tool,assistant,usage" {
		t.Fatalf("events = %s", got)
	}
	if texts[0] != "Writing it." || texts[1] != "done" {
		t.Fatalf("texts = %q", texts)
	}
}

// Resuming a conversation uses session/resume, and an approval left
// unanswered is cancelled rather than guessed.
func TestACPSessionResumeAndExpiry(t *testing.T) {
	rig, s := newRig(t, "hermes", "default", time.Second)
	acp := s.(*acpSession)
	acp.spec.IsNew, acp.spec.NativeID = false, "old-session"
	go s.start()
	init := rig.next()
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{
		"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}}})
	res := rig.next()
	if res["method"] != "session/resume" || res["params"].(map[string]any)["sessionId"] != "old-session" {
		t.Fatalf("resume = %v", res)
	}
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": res["id"], "result": map[string]any{
		"modes": map[string]any{"currentModeId": "default"}}})
	prompt := rig.next()
	if prompt["params"].(map[string]any)["sessionId"] != "old-session" {
		t.Fatalf("prompt = %v", prompt)
	}
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "session/request_permission", "params": map[string]any{
		"options":  []any{map[string]any{"kind": "allow_once", "name": "Allow", "optionId": "a"}},
		"toolCall": map[string]any{"toolCallId": "x", "title": "t"}}})
	reply := rig.next()
	if reply["result"].(map[string]any)["outcome"].(map[string]any)["outcome"] != "cancelled" {
		t.Fatalf("expired reply = %v", reply)
	}
}

// Hermes advertises both load and resume (Hermes Agent 0.22); the console
// loads, and the history Hermes replays before answering is not part of the
// new turn.
func TestACPLoadIgnoresReplay(t *testing.T) {
	rig, s := newRig(t, "hermes", "default", time.Minute)
	acp := s.(*acpSession)
	acp.spec.IsNew, acp.spec.NativeID = false, "old"
	go s.start()
	init := rig.next()
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{
		"agentCapabilities": map[string]any{"loadSession": true,
			"sessionCapabilities": map[string]any{"resume": map[string]any{}}}}})
	res := rig.next()
	if res["method"] != "session/load" || res["params"].(map[string]any)["sessionId"] != "old" {
		t.Fatalf("load = %v", res)
	}
	upd := func(u map[string]any) {
		rig.feed(s, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"update": u}})
	}
	upd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "old answer"}})
	upd(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "old-tool", "kind": "edit"})
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": res["id"], "result": map[string]any{
		"modes": map[string]any{"currentModeId": "default"}}})
	prompt := rig.next()
	if prompt["params"].(map[string]any)["sessionId"] != "old" {
		t.Fatalf("prompt = %v", prompt)
	}
	upd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "new answer"}})
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": prompt["id"], "result": map[string]any{"stopReason": "end_turn"}})
	b, _ := os.ReadFile(rig.events)
	if strings.Contains(string(b), "old answer") || strings.Contains(string(b), "old-tool") || !strings.Contains(string(b), "new answer") {
		t.Fatalf("events = %s", b)
	}
}

// A load of a session Hermes does not have answers {} (resume would have
// started a new session silently); the turn fails with a reason and no
// prompt is sent.
func TestACPLoadUnknownSession(t *testing.T) {
	rig, s := newRig(t, "hermes", "default", time.Minute)
	acp := s.(*acpSession)
	acp.spec.IsNew, acp.spec.NativeID = false, "gone"
	go s.start()
	init := rig.next()
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{
		"agentCapabilities": map[string]any{"loadSession": true}}})
	res := rig.next()
	rig.feed(s, map[string]any{"jsonrpc": "2.0", "id": res["id"], "result": map[string]any{}})
	st, msg, ok := s.outcome()
	if !ok || st != store.StatusFailed || !strings.Contains(msg, "找不到这段对话") {
		t.Fatalf("outcome = %q %q %v", st, msg, ok)
	}
}
