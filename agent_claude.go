package console

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// agentSession speaks one agent CLI's stdio protocol for the runner: it sends
// the prompt, records every stdout line as an event, and answers the agent's
// requests (permissions, questions, plans) with what the user decided.
type agentSession interface {
	start()
	onLine(line []byte)
	// outcome is the final status the protocol itself announced, if any. When
	// ok is false the process exit code decides the task status.
	outcome() (status, message string, ok bool)
}

func newAgentSession(spec TaskSpec, control string, w *eventWriter, stdin io.WriteCloser,
	stop <-chan struct{}, onNativeID func(string)) agentSession {
	timeout := time.Duration(spec.PermTimeout) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	if spec.Agent == "codex" {
		return newCodexSession(spec, control, w, stdin, stop, onNativeID, timeout)
	}
	return &claudeSession{spec: spec, control: control, w: w, stdin: stdin, stop: stop,
		onNativeID: onNativeID, timeout: timeout}
}

// claudeSession drives `claude -p --input-format stream-json
// --permission-prompt-tool stdio`. The prompt goes in as one user message and
// stdin stays open, because permission answers travel back on it as
// control_response messages. Claude ends the turn with a result message, after
// which stdin is closed and the process exits.
type claudeSession struct {
	spec       TaskSpec
	control    string
	w          *eventWriter
	stdin      io.WriteCloser
	stop       <-chan struct{}
	onNativeID func(string)
	timeout    time.Duration

	mu     sync.Mutex // serialises writes to stdin
	closed bool
	seenID string
	// stopped is set when the user chose "deny and stop": Claude then exits
	// with an error, but the task was cancelled, not failed.
	stopped atomic.Bool
}

func (c *claudeSession) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	_, _ = c.stdin.Write(append(b, '\n'))
}

func (c *claudeSession) closeInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.stdin.Close()
	}
}

func (c *claudeSession) start() {
	c.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": c.spec.Prompt}})
}

func (c *claudeSession) outcome() (string, string, bool) {
	if c.stopped.Load() {
		return StatusCancelled, "用户拒绝并停止了本轮任务", true
	}
	return "", "", false
}

func (c *claudeSession) onLine(line []byte) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		c.w.appendEnvelope(map[string]any{"stream": "text", "text": string(line)})
		return
	}
	switch str(raw["type"]) {
	case "control_request":
		go c.handleControl(line)
		return
	case "control_response", "control_cancel_request":
		return // protocol traffic, not transcript
	}
	c.w.append(raw)
	if id := extractNativeID(raw); id != "" && id != c.seenID {
		c.seenID = id
		c.onNativeID(id)
	}
	if str(raw["type"]) == "result" {
		c.closeInput()
	}
}

func (c *claudeSession) handleControl(line []byte) {
	var msg struct {
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return
	}
	var sub struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(msg.Request, &sub)
	if sub.Subtype != "can_use_tool" {
		c.send(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": msg.RequestID, "error": "unsupported control request: " + sub.Subtype}})
		return
	}
	var tr claudeToolRequest
	if json.Unmarshal(msg.Request, &tr) != nil {
		return
	}
	req := claudeInteraction(tr)
	req.ID = randomID(9)
	resp, ok := awaitUser(c.control, req, c.timeout, c.stop)
	select {
	case <-c.stop:
		return // the agent is gone; nobody to answer
	default:
	}
	reply, status, message := claudeReply(tr, resp, ok)
	if ok && resp.Decision == decisionDenyStop {
		c.stopped.Store(true)
	}
	c.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": msg.RequestID, "response": reply}})
	finishUser(c.control, req.ID, status, message)
}
