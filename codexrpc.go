package console

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// codexSession drives `codex app-server`, the JSON-RPC protocol Codex's own IDE
// integrations use. Unlike `codex exec` it can ask the user: command and file
// approvals (with "for this session" and "always" choices) and, in plan mode,
// structured questions. One task is one turn: initialize, start or resume the
// thread, start the turn, answer requests, and close stdin when the turn
// completes (app-server then exits on its own).
type codexSession struct {
	spec       TaskSpec
	control    string
	w          *eventWriter
	stdin      io.WriteCloser
	stop       <-chan struct{}
	onNativeID func(string)
	timeout    time.Duration

	mu     sync.Mutex // serialises writes to stdin
	closed bool

	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[string]string // request id -> method

	state    sync.Mutex
	threadID string
	model    string
	items    map[string]json.RawMessage // fileChange items, for their approval cards
	usage    json.RawMessage
	status   string
	message  string
	finished bool
}

func newCodexSession(spec TaskSpec, control string, w *eventWriter, stdin io.WriteCloser,
	stop <-chan struct{}, onNativeID func(string), timeout time.Duration) *codexSession {
	return &codexSession{spec: spec, control: control, w: w, stdin: stdin, stop: stop,
		onNativeID: onNativeID, timeout: timeout,
		pending: map[string]string{}, items: map[string]json.RawMessage{}}
}

// codexModes maps a console profile onto app-server settings. The approval
// policy is what makes Codex ask instead of silently failing in its sandbox.
func codexModes(profile string) (sandbox, approval, collab string) {
	switch profile {
	case "workspace-write":
		return "workspace-write", "on-request", "default"
	case "danger-full-access":
		return "danger-full-access", "never", "default"
	case "plan":
		return "read-only", "on-request", "plan"
	}
	return "read-only", "on-request", "default"
}

func (c *codexSession) send(v any) {
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

func (c *codexSession) closeInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.stdin.Close()
	}
}

func (c *codexSession) call(method string, params any) {
	id := c.nextID.Add(1)
	c.pmu.Lock()
	c.pending[strconv.FormatInt(id, 10)] = method
	c.pmu.Unlock()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (c *codexSession) finish(status, message string) {
	c.state.Lock()
	if !c.finished {
		c.finished, c.status, c.message = true, status, message
	}
	c.state.Unlock()
	c.closeInput()
}

func (c *codexSession) outcome() (string, string, bool) {
	c.state.Lock()
	defer c.state.Unlock()
	return c.status, c.message, c.finished
}

func (c *codexSession) start() {
	c.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agentconsole", "version": version},
		"capabilities": map[string]any{"experimentalApi": true}, // collaborationMode (plan)
	})
}

func (c *codexSession) onLine(line []byte) {
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
		go c.handleServerRequest(m.ID, m.Method, m.Params)
	case len(m.ID) > 0:
		errMsg := ""
		if m.Error != nil {
			errMsg = orDefault(m.Error.Message, "未知错误")
		}
		c.handleResponse(string(m.ID), m.Result, errMsg)
	case m.Method != "":
		c.handleNotification(m.Method, m.Params, line)
	}
}

func (c *codexSession) handleResponse(id string, result json.RawMessage, errMsg string) {
	c.pmu.Lock()
	method := c.pending[id]
	delete(c.pending, id)
	c.pmu.Unlock()
	if errMsg != "" {
		msg := fmt.Sprintf("Codex %s 失败：%s", method, errMsg)
		c.w.append(map[string]any{"method": "error", "params": map[string]any{"error": map[string]any{"message": msg}}})
		c.finish(StatusFailed, msg)
		return
	}
	switch method {
	case "initialize":
		c.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
		if name, arg, ok := slashCommand(c.spec.Prompt); ok {
			c.command(name, arg)
			return
		}
		c.openThread()
	case "model/list":
		c.modelCommand(result)
	case "thread/compact/start", "review/start":
		// The work runs as a turn; turn/completed ends the task.
	case "thread/start", "thread/resume":
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model string `json:"model"`
		}
		_ = json.Unmarshal(result, &r)
		c.state.Lock()
		c.threadID, c.model = r.Thread.ID, r.Model
		c.state.Unlock()
		if r.Thread.ID != "" {
			c.onNativeID(r.Thread.ID)
		}
		if name, arg, ok := slashCommand(c.spec.Prompt); ok {
			c.threadCommand(name, arg, r.Thread.ID)
			return
		}
		_, _, collab := codexModes(c.spec.Profile)
		params := map[string]any{
			"threadId": r.Thread.ID,
			"input":    []any{map[string]any{"type": "text", "text": c.spec.Prompt}},
		}
		model := orDefault(c.spec.Model, r.Model)
		if c.spec.Model != "" {
			params["model"] = c.spec.Model
		}
		if c.spec.Effort != "" {
			params["effort"] = c.spec.Effort
		}
		if model != "" {
			// Sent every turn: the mode is per turn, so leaving plan mode is as
			// explicit as entering it. Its settings carry the effort too: left
			// out, they reset it, overriding the turn's own "effort".
			settings := map[string]any{"model": model}
			if c.spec.Effort != "" {
				settings["reasoning_effort"] = c.spec.Effort
			}
			params["collaborationMode"] = map[string]any{"mode": collab, "settings": settings}
		}
		c.call("turn/start", params)
	}
}

// openThread starts the conversation's thread, or resumes it.
func (c *codexSession) openThread() {
	sandbox, approval, _ := codexModes(c.spec.Profile)
	params := map[string]any{"cwd": c.spec.Cwd, "sandbox": sandbox, "approvalPolicy": approval}
	if c.spec.Model != "" {
		params["model"] = c.spec.Model
	}
	if !c.spec.IsNew && c.spec.NativeID != "" {
		params["threadId"] = c.spec.NativeID
		c.call("thread/resume", params)
	} else {
		c.call("thread/start", params)
	}
}

// reply prints a command's answer in the transcript, as the CLI prints it.
func (c *codexSession) reply(text string) {
	c.w.append(map[string]any{"method": "agentconsole/reply", "params": map[string]any{"text": text}})
}

// command runs one of Codex's slash commands (codexCommands in slash.go).
func (c *codexSession) command(name, arg string) {
	switch name {
	case "model":
		c.call("model/list", map[string]any{})
	case "effort":
		levels := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
		for _, l := range levels {
			if arg == l {
				c.w.state(map[string]any{"session_settings": map[string]any{"effort": arg}})
				c.reply("推理强度已设为 " + arg)
				c.finish(StatusCompleted, "")
				return
			}
		}
		if arg == "default" {
			c.w.state(map[string]any{"session_settings": map[string]any{"effort": ""}})
			c.reply("推理强度已恢复为模型默认值")
		} else {
			c.reply("用法：/effort <" + strings.Join(levels, "|") + "|default>")
		}
		c.finish(StatusCompleted, "")
	case "status":
		sandbox, approval, collab := codexModes(c.spec.Profile)
		lines := []string{
			"**模型**：" + orDefault(c.spec.Model, "默认") + "　**推理强度**：" + orDefault(c.spec.Effort, "默认"),
			"**权限**：" + profileLabel("codex", c.spec.Profile) + "（sandbox " + sandbox + "，approval " + approval + "，" + collab + " 模式）",
			"**目录**：`" + c.spec.Cwd + "`",
			"**会话**：`" + orDefault(c.spec.NativeID, "尚未开始") + "`",
		}
		c.reply(strings.Join(lines, "\n\n"))
		c.finish(StatusCompleted, "")
	case "compact", "review":
		if name == "compact" && (c.spec.IsNew || c.spec.NativeID == "") {
			c.reply("还没有可以压缩的对话。")
			c.finish(StatusCompleted, "")
			return
		}
		c.openThread() // continues in threadCommand
	default:
		c.reply("未知指令 /" + name)
		c.finish(StatusCompleted, "")
	}
}

// threadCommand runs a command that needs the conversation's thread.
func (c *codexSession) threadCommand(name, arg, threadID string) {
	switch name {
	case "compact":
		c.call("thread/compact/start", map[string]any{"threadId": threadID})
	case "review":
		target := map[string]any{"type": "uncommittedChanges"}
		if arg != "" {
			target = map[string]any{"type": "custom", "instructions": arg}
		}
		c.call("review/start", map[string]any{"threadId": threadID, "target": target, "delivery": "inline"})
	default:
		c.finish(StatusCompleted, "")
	}
}

// modelCommand lists the models (bare /model) or switches to one, checked
// against the list Codex reports: "/model gpt-6-sol high".
func (c *codexSession) modelCommand(result json.RawMessage) {
	defer c.finish(StatusCompleted, "")
	models, err := parseCodexModels(result)
	if err != nil {
		c.reply("读取模型列表失败：" + err.Error())
		return
	}
	_, arg, _ := slashCommand(c.spec.Prompt)
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		var b strings.Builder
		b.WriteString("当前模型：**" + orDefault(c.spec.Model, "默认") + "**，推理强度：**" + orDefault(c.spec.Effort, "默认") + "**\n\n")
		for _, m := range models {
			mark := ""
			if m.Default {
				mark = "（默认）"
			}
			b.WriteString("- `" + m.ID + "` " + m.Label + mark + " · 强度：" + strings.Join(m.Efforts, "/") + "\n")
		}
		b.WriteString("\n用法：`/model <模型> [强度]`，`/model default` 恢复默认")
		c.reply(b.String())
		return
	}
	if fields[0] == "default" {
		c.w.state(map[string]any{"session_settings": map[string]any{"model": "", "effort": ""}})
		c.reply("已恢复为默认模型")
		return
	}
	for _, m := range models {
		if m.ID != fields[0] {
			continue
		}
		set := map[string]any{"model": m.ID}
		msg := "已切换到 " + m.Label
		if len(fields) > 1 {
			ok := false
			for _, e := range m.Efforts {
				ok = ok || e == fields[1]
			}
			if !ok {
				c.reply(m.Label + " 不支持推理强度 " + fields[1] + "，可选：" + strings.Join(m.Efforts, "/"))
				return
			}
			set["effort"] = fields[1]
			msg += "，推理强度 " + fields[1]
		}
		c.w.state(map[string]any{"session_settings": set})
		c.reply(msg)
		return
	}
	c.reply("没有模型 " + fields[0] + "。输入 /model 查看可用模型。")
}

// Only these notifications become transcript events; deltas and status noise
// are dropped (the completed item carries the full text).
var codexLogged = map[string]bool{
	"item/started": true, "item/completed": true, "turn/completed": true, "error": true,
}

func (c *codexSession) handleNotification(method string, params json.RawMessage, line []byte) {
	switch method {
	case "item/started":
		var p struct {
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Item.Type == "fileChange" {
			var whole struct {
				Item json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal(params, &whole)
			c.state.Lock()
			c.items[p.Item.ID] = whole.Item
			c.state.Unlock()
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Last json.RawMessage `json:"last"`
			} `json:"tokenUsage"`
		}
		_ = json.Unmarshal(params, &p)
		c.state.Lock()
		c.usage = p.TokenUsage.Last
		c.state.Unlock()
		return
	case "turn/completed":
		c.state.Lock()
		usage := c.usage
		c.state.Unlock()
		if len(usage) > 0 {
			c.w.append(map[string]any{"method": "agentconsole/usage", "params": map[string]any{"usage": usage}})
		}
	}
	if !codexLogged[method] {
		return
	}
	c.w.append(json.RawMessage(line))
	if method == "turn/completed" {
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		switch p.Turn.Status {
		case "completed":
			c.finish(StatusCompleted, "")
		case "interrupted":
			c.finish(StatusCancelled, "本轮已停止")
		default:
			msg := "Codex 本轮失败"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			c.finish(StatusFailed, msg)
		}
	}
}

func (c *codexSession) respond(id json.RawMessage, result any) {
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *codexSession) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	req := ctlRequest{ID: randomID(9), Kind: kindPermission}
	var reply func(resp ctlResponse, ok bool) (any, string, string)
	switch method {
	case "item/commandExecution/requestApproval":
		var p struct {
			ItemID             string            `json:"itemId"`
			Reason             string            `json:"reason"`
			Command            string            `json:"command"`
			Cwd                string            `json:"cwd"`
			AvailableDecisions []json.RawMessage `json:"availableDecisions"`
		}
		_ = json.Unmarshal(params, &p)
		opts, wire := codexDecisionOptions(p.AvailableDecisions)
		in, _ := json.Marshal(map[string]any{"command": p.Command, "cwd": p.Cwd})
		req.ToolName, req.Input, req.ToolUseID, req.Description, req.Options = "shell", in, p.ItemID, p.Reason, opts
		reply = codexDecisionReply(wire)
	case "item/fileChange/requestApproval":
		var p struct {
			ItemID string `json:"itemId"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(params, &p)
		c.state.Lock()
		item := c.items[p.ItemID]
		c.state.Unlock()
		var it struct {
			Changes json.RawMessage `json:"changes"`
		}
		_ = json.Unmarshal(item, &it)
		in, _ := json.Marshal(map[string]any{"changes": it.Changes})
		opts, wire := codexDecisionOptions(nil)
		req.ToolName, req.Input, req.ToolUseID, req.Description, req.Options = "file_change", in, p.ItemID, p.Reason, opts
		reply = codexDecisionReply(wire)
	case "item/permissions/requestApproval":
		var p struct {
			ItemID      string          `json:"itemId"`
			Reason      string          `json:"reason"`
			Permissions json.RawMessage `json:"permissions"`
		}
		_ = json.Unmarshal(params, &p)
		req.ToolName, req.Input, req.ToolUseID, req.Description = "permissions", p.Permissions, p.ItemID, p.Reason
		req.Options = []ctlOption{{ID: decisionAllow, Label: "允许（本轮）"},
			{ID: decisionAllowSession, Label: "本会话允许"}, {ID: decisionDeny, Label: "拒绝"}}
		reply = func(resp ctlResponse, ok bool) (any, string, string) {
			if ok && (resp.Decision == decisionAllow || resp.Decision == decisionAllowSession) {
				scope := "turn"
				if resp.Decision == decisionAllowSession {
					scope = "session"
				}
				return map[string]any{"permissions": p.Permissions, "scope": scope}, "allowed", ""
			}
			if !ok {
				return map[string]any{"permissions": map[string]any{}}, "expired", "审批超时"
			}
			return map[string]any{"permissions": map[string]any{}}, "denied", resp.Message
		}
	case "item/tool/requestUserInput":
		var p struct {
			ItemID string `json:"itemId"`
		}
		_ = json.Unmarshal(params, &p)
		req.Kind, req.ToolName, req.ToolUseID, req.Questions = kindQuestion, "request_user_input", p.ItemID, codexQuestions(params)
		reply = func(resp ctlResponse, ok bool) (any, string, string) {
			if !ok {
				return map[string]any{"answers": map[string]any{}}, "expired", "用户未在时限内回答"
			}
			if resp.Decision != decisionAnswer {
				return map[string]any{"answers": map[string]any{}}, "denied", "用户跳过了这个问题"
			}
			answers := map[string]any{}
			for q, vals := range resp.Answers {
				answers[q] = map[string]any{"answers": vals}
			}
			return map[string]any{"answers": answers}, "answered", ""
		}
	case "mcpServer/elicitation/request":
		c.w.notice("MCP 服务器请求填写表单，控制台暂不支持，已拒绝。")
		c.respond(id, map[string]any{"action": "decline"})
		return
	default:
		c.send(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "agentconsole does not support " + method}})
		return
	}
	resp, ok := awaitUser(c.control, req, c.timeout, c.stop)
	select {
	case <-c.stop:
		return
	default:
	}
	result, status, message := reply(resp, ok)
	c.respond(id, result)
	finishUser(c.control, req.ID, status, message)
}

// codexDecisionReply answers an approval with the exact decision value Codex
// offered for the chosen option.
func codexDecisionReply(wire map[string]json.RawMessage) func(ctlResponse, bool) (any, string, string) {
	decline := json.RawMessage(`"decline"`)
	return func(resp ctlResponse, ok bool) (any, string, string) {
		if !ok {
			return map[string]any{"decision": decline}, "expired", "审批超时"
		}
		raw, known := wire[resp.Decision]
		if !known {
			raw = decline
		}
		status := statusFor(resp.Decision)
		if !known {
			status = "denied"
		}
		return map[string]any{"decision": raw}, status, resp.Message
	}
}
