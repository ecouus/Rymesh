package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rymesh/internal/store"
	"rymesh/internal/util"
)

// acpSession drives an agent that speaks the Agent Client Protocol on stdio
// (`hermes acp`): JSON-RPC with session/new, session/load or session/resume,
// session/prompt, session/update notifications and session/request_permission
// requests. One task is one prompt turn; stdin is closed when the turn ends
// and the agent exits.
//
// ACP streams the answer in chunks. The transcript keeps whole messages, as
// it does for Codex, so chunks are collected and written as one event when
// something else happens (a tool call) or the turn ends.
type acpSession struct {
	spec       TaskSpec
	control    string
	w          *eventWriter
	stdin      io.WriteCloser
	stop       <-chan struct{}
	onNativeID func(string)
	timeout    time.Duration
	label      string // agent name in messages ("Hermes")

	mu     sync.Mutex // serialises writes to stdin
	closed bool

	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[string]string // request id -> method

	state     sync.Mutex
	sessionID string
	setup     []acpCall // mode/model changes before the prompt
	text      strings.Builder
	thought   strings.Builder
	tools     map[string]map[string]any // tool calls seen, by id
	// inTurn is set once the prompt is sent. Loading a session replays its
	// history as updates first; those are not part of this turn.
	inTurn   bool
	status   string
	message  string
	finished bool
}

type acpCall struct {
	method string
	params map[string]any
}

func newACPSession(spec TaskSpec, control string, w *eventWriter, stdin io.WriteCloser,
	stop <-chan struct{}, onNativeID func(string), timeout time.Duration, label string) *acpSession {
	return &acpSession{spec: spec, control: control, w: w, stdin: stdin, stop: stop,
		onNativeID: onNativeID, timeout: timeout, label: label,
		pending: map[string]string{}, tools: map[string]map[string]any{}}
}

func (c *acpSession) send(v any) {
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

func (c *acpSession) closeInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.stdin.Close()
	}
}

func (c *acpSession) call(method string, params any) {
	id := c.nextID.Add(1)
	c.pmu.Lock()
	c.pending[strconv.FormatInt(id, 10)] = method
	c.pmu.Unlock()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (c *acpSession) finish(status, message string) {
	c.state.Lock()
	if !c.finished {
		c.finished, c.status, c.message = true, status, message
	}
	c.state.Unlock()
	c.closeInput()
}

func (c *acpSession) outcome() (string, string, bool) {
	c.state.Lock()
	defer c.state.Unlock()
	return c.status, c.message, c.finished
}

// event records one transcript event; ingest's normalizeACP reads it back.
func (c *acpSession) event(kind string, payload map[string]any) {
	c.w.append(map[string]any{"acp": kind, "payload": payload})
}

func (c *acpSession) start() {
	c.call("initialize", map[string]any{
		"protocolVersion": 1,
		// The console has no editor buffers or terminals to lend: the agent
		// reads, writes and runs on its own, and asks before it does.
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "rymesh", "version": util.Version},
	})
}

func (c *acpSession) onLine(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		c.w.appendEnvelope(map[string]any{"stream": "text", "text": string(line)})
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		go c.handleRequest(m.ID, m.Method, m.Params)
	case len(m.ID) > 0:
		errMsg := ""
		if m.Error != nil {
			errMsg = OrDefault(m.Error.Message, "未知错误")
		}
		c.handleResponse(string(m.ID), m.Result, errMsg)
	case m.Method == "session/update":
		c.handleUpdate(m.Params)
	}
}

func (c *acpSession) handleResponse(id string, result json.RawMessage, errMsg string) {
	c.pmu.Lock()
	method := c.pending[id]
	delete(c.pending, id)
	c.pmu.Unlock()
	if errMsg != "" {
		switch method {
		case "session/set_mode", "session/set_model":
			// The turn can still run with the agent's own setting.
			c.w.notice(fmt.Sprintf("%s 未接受 %s：%s", c.label, method, errMsg))
			c.next()
			return
		}
		msg := fmt.Sprintf("%s %s 失败：%s", c.label, method, errMsg)
		c.event("error", map[string]any{"text": msg})
		c.finish(store.StatusFailed, msg)
		return
	}
	switch method {
	case "initialize":
		var r struct {
			AgentCapabilities struct {
				LoadSession         bool `json:"loadSession"`
				SessionCapabilities struct {
					Resume *struct{} `json:"resume"`
				} `json:"sessionCapabilities"`
			} `json:"agentCapabilities"`
		}
		_ = json.Unmarshal(result, &r)
		params := map[string]any{"cwd": c.spec.Cwd, "mcpServers": []any{}}
		switch {
		case c.spec.IsNew || c.spec.NativeID == "":
			c.call("session/new", params)
		case r.AgentCapabilities.LoadSession:
			// Preferred over resume: Hermes answers a resume of a session it
			// cannot find by quietly starting a new one, and the prompt then
			// goes to an id it does not know. A failed load says so.
			params["sessionId"] = c.spec.NativeID
			c.call("session/load", params)
		case r.AgentCapabilities.SessionCapabilities.Resume != nil:
			params["sessionId"] = c.spec.NativeID
			c.call("session/resume", params)
		default:
			msg := fmt.Sprintf("%s 不支持继续已有的会话，请新开一个对话", c.label)
			c.event("error", map[string]any{"text": msg})
			c.finish(store.StatusFailed, msg)
		}
	case "session/new", "session/load", "session/resume":
		if method == "session/load" && acpEmptyResult(result) {
			msg := fmt.Sprintf("%[1]s 找不到这段对话（可能已在 %[1]s 中删除），请新开一个对话", c.label)
			c.event("error", map[string]any{"text": msg})
			c.finish(store.StatusFailed, msg)
			return
		}
		var r struct {
			SessionID string `json:"sessionId"`
			Modes     struct {
				Current string `json:"currentModeId"`
				Modes   []struct {
					ID string `json:"id"`
				} `json:"availableModes"`
			} `json:"modes"`
			Models struct {
				Current string `json:"currentModelId"`
			} `json:"models"`
		}
		_ = json.Unmarshal(result, &r)
		sid := OrDefault(r.SessionID, c.spec.NativeID)
		if r.SessionID != "" {
			c.onNativeID(r.SessionID)
		}
		var setup []acpCall
		if c.spec.Profile != "" && c.spec.Profile != r.Modes.Current {
			for _, m := range r.Modes.Modes {
				if m.ID == c.spec.Profile {
					setup = append(setup, acpCall{"session/set_mode", map[string]any{"sessionId": sid, "modeId": m.ID}})
				}
			}
		}
		if c.spec.Model != "" && c.spec.Model != r.Models.Current {
			setup = append(setup, acpCall{"session/set_model", map[string]any{"sessionId": sid, "modelId": c.spec.Model}})
		}
		c.state.Lock()
		c.sessionID, c.setup = sid, setup
		c.state.Unlock()
		c.next()
	case "session/set_mode", "session/set_model":
		c.next()
	case "session/prompt":
		var r struct {
			StopReason string         `json:"stopReason"`
			Usage      map[string]any `json:"usage"`
		}
		_ = json.Unmarshal(result, &r)
		c.flush()
		if len(r.Usage) > 0 {
			c.event("usage", map[string]any{"usage": r.Usage})
		}
		switch r.StopReason {
		case "cancelled":
			c.finish(store.StatusCancelled, "本轮已停止")
		case "refusal":
			c.event("error", map[string]any{"text": "模型拒绝回答"})
			c.finish(store.StatusFailed, "模型拒绝回答")
		case "max_tokens", "max_turn_requests":
			c.w.notice(fmt.Sprintf("%s 已达到本轮的上限（%s）", c.label, r.StopReason))
			c.finish(store.StatusCompleted, "")
		default:
			c.finish(store.StatusCompleted, "")
		}
	}
}

// next sends the next setup call, or the prompt once there are none left.
func (c *acpSession) next() {
	c.state.Lock()
	sid := c.sessionID
	var call *acpCall
	if len(c.setup) > 0 {
		call, c.setup = &c.setup[0], c.setup[1:]
	}
	c.state.Unlock()
	if call != nil {
		c.call(call.method, call.params)
		return
	}
	c.state.Lock()
	c.inTurn = true
	c.state.Unlock()
	// Slash commands are prompts too: the agent recognises "/name args".
	c.call("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": c.spec.Prompt}},
	})
}

// flush writes the collected thought and message text as whole events.
func (c *acpSession) flush() {
	c.state.Lock()
	thought, text := c.thought.String(), c.text.String()
	c.thought.Reset()
	c.text.Reset()
	c.state.Unlock()
	if t := strings.TrimSpace(thought); t != "" {
		c.event("thinking", map[string]any{"text": t})
	}
	if t := strings.TrimSpace(text); t != "" {
		c.event("assistant", map[string]any{"text": t})
	}
}

func (c *acpSession) handleUpdate(params json.RawMessage) {
	var p struct {
		Update map[string]any `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil || p.Update == nil {
		return
	}
	u := p.Update
	kind := util.Str(u["sessionUpdate"])
	c.state.Lock()
	replay := !c.inTurn
	c.state.Unlock()
	if replay && kind != "available_commands_update" {
		return
	}
	switch kind {
	case "agent_message_chunk":
		c.state.Lock()
		c.text.WriteString(acpText(u["content"]))
		c.state.Unlock()
	case "agent_thought_chunk":
		c.state.Lock()
		c.thought.WriteString(acpText(u["content"]))
		c.state.Unlock()
	case "tool_call":
		c.flush()
		id := util.Str(u["toolCallId"])
		t := map[string]any{"id": id, "name": acpToolName(u), "status": "running",
			"input": acpToolInput(u)}
		c.state.Lock()
		c.tools[id] = t
		c.state.Unlock()
		c.event("tool", t)
	case "tool_call_update":
		id := util.Str(u["toolCallId"])
		c.state.Lock()
		t, known := c.tools[id]
		c.state.Unlock()
		if !known {
			return // e.g. the approval step of an edit, shown as its own card
		}
		status := util.Str(u["status"])
		if out := acpContent(u["content"]); out != "" {
			t["output"] = out
		}
		if status != "completed" && status != "failed" {
			return // only the final state is worth a transcript event
		}
		t["status"] = status
		c.event("tool", t)
	case "plan":
		entries, _ := u["entries"].([]any)
		var lines []string
		for _, e := range entries {
			m, _ := e.(map[string]any)
			mark := "○"
			switch util.Str(m["status"]) {
			case "completed":
				mark = "✓"
			case "in_progress":
				mark = "…"
			}
			lines = append(lines, mark+" "+util.Str(m["content"]))
		}
		if len(lines) > 0 {
			c.flush()
			c.event("notice", map[string]any{"text": fmt.Sprintf("计划：%s", strings.Join(lines, "　"))})
		}
	case "available_commands_update":
		// What "/" offers in the composer (ingest keeps the latest list).
		c.event("commands", map[string]any{"commands": u["availableCommands"]})
	}
}

func (c *acpSession) respond(id json.RawMessage, result any) {
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// acpDecisions maps ACP permission option kinds onto the console's decisions.
var acpDecisions = map[string]string{
	"allow_once":    DecisionAllow,
	"allow_always":  decisionAllowAlways,
	"reject_once":   DecisionDeny,
	"reject_always": DecisionDeny,
}

func (c *acpSession) handleRequest(id json.RawMessage, method string, params json.RawMessage) {
	if method != "session/request_permission" {
		// The client advertised no fs or terminal capabilities.
		c.send(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "rymesh does not support " + method}})
		return
	}
	var p struct {
		Options []struct {
			Kind     string `json:"kind"`
			Name     string `json:"name"`
			OptionID string `json:"optionId"`
		} `json:"options"`
		ToolCall map[string]any `json:"toolCall"`
	}
	_ = json.Unmarshal(params, &p)
	c.flush()
	wire := map[string]string{} // decision -> optionId
	var opts []CtlOption
	for _, o := range p.Options {
		d, ok := acpDecisions[o.Kind]
		if !ok || wire[d] != "" {
			continue // one card button per decision ("deny" once)
		}
		wire[d] = o.OptionID
		opts = append(opts, CtlOption{ID: d, Label: o.Name})
	}
	if len(opts) == 0 {
		c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		return
	}
	in, _ := json.Marshal(acpToolInput(p.ToolCall))
	req := CtlRequest{ID: util.RandomID(9), Kind: store.KindPermission, ToolName: acpToolName(p.ToolCall),
		Input: in, ToolUseID: util.Str(p.ToolCall["toolCallId"]), Description: util.Str(p.ToolCall["title"]),
		Options: opts}
	resp, ok := awaitUser(c.control, req, c.timeout, c.stop)
	select {
	case <-c.stop:
		return
	default:
	}
	optionID, known := wire[resp.Decision]
	switch {
	case !ok:
		c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		finishUser(c.control, req.ID, "expired", "审批超时")
	case !known:
		// A decision the agent did not offer: refuse with its own deny
		// option when there is one, else cancel the request.
		if deny := wire[DecisionDeny]; deny != "" {
			c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": deny}})
		} else {
			c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		}
		finishUser(c.control, req.ID, "denied", resp.Message)
	default:
		c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}})
		finishUser(c.control, req.ID, StatusFor(resp.Decision), resp.Message)
	}
}

// acpEmptyResult reports a null or {} result: what a load of an unknown
// session returns, where a loaded one describes its modes and models.
func acpEmptyResult(result json.RawMessage) bool {
	var m map[string]any
	return json.Unmarshal(result, &m) != nil || len(m) == 0
}

// acpText is the text of a content block ({"type":"text","text":...}).
func acpText(v any) string {
	m, _ := v.(map[string]any)
	if util.Str(m["type"]) == "text" {
		return util.Str(m["text"])
	}
	return ""
}

// acpContent joins the text and diffs of a tool call's content list.
func acpContent(v any) string {
	list, _ := v.([]any)
	var parts []string
	for _, x := range list {
		m, _ := x.(map[string]any)
		switch util.Str(m["type"]) {
		case "content":
			if t := acpText(m["content"]); t != "" {
				parts = append(parts, t)
			}
		case "diff":
			parts = append(parts, "--- "+util.Str(m["path"])+"\n"+util.Str(m["newText"]))
		}
	}
	return strings.Join(parts, "\n\n")
}

// acpToolName names a tool call: the agent's own tool name when it reports
// one (in rawInput, or as the "tool: ..." title Hermes uses), else the ACP
// kind (execute, edit, read, fetch...).
func acpToolName(u map[string]any) string {
	if raw, _ := u["rawInput"].(map[string]any); raw != nil {
		if t := util.Str(raw["tool"]); t != "" {
			return t
		}
	}
	if name, _, ok := strings.Cut(util.Str(u["title"]), ": "); ok && name != "" && len(name) <= 32 &&
		!strings.ContainsAny(name, " \t") {
		return name
	}
	return OrDefault(util.Str(u["kind"]), "tool")
}

// acpToolInput is what the card shows as the call's input: the raw arguments
// when the agent reports them, else its title and the content it described.
func acpToolInput(u map[string]any) map[string]any {
	if raw, _ := u["rawInput"].(map[string]any); raw != nil {
		if args, ok := raw["arguments"].(map[string]any); ok {
			return args
		}
		return raw
	}
	in := map[string]any{"title": util.Str(u["title"])}
	if c := acpContent(u["content"]); c != "" {
		in["content"] = c
	}
	return in
}

// acpStderrWorthy keeps an ACP agent's stderr out of the transcript unless a
// line reports a failure: the protocol runs on stdout and these agents log
// routinely on stderr.
func acpStderrWorthy(line string) bool {
	for _, s := range []string{"ERROR", "CRITICAL", "Traceback", "Error:", "error:"} {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}
