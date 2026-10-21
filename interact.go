package main

// Interactions: everything an agent asks the user while a task runs —
// permission prompts, questions and plan approval — in one shape shared by
// both agents, the runner, the web service and the page.
//
// The runner owns the agent's stdin/stdout, so it is the one that receives a
// request and delivers the answer. The web service and the runner never talk
// directly; the task's control directory is the whole contract:
//
//	runner   writes req-<id>.json   (ctlRequest)
//	web      writes resp-<id>.json  (ctlResponse, from handleResolvePermission)
//	runner   writes done-<id>.json  (ctlDone, once the agent has the answer)
//
// That is what lets a pending question survive a web-service restart or a
// closed browser: the runner just keeps waiting for the file.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	kindPermission = "permission"
	kindQuestion   = "question"
	kindPlan       = "plan"
)

// Decision ids. Permission prompts offer a subset of the first five, chosen
// from what the agent says it supports; questions are answered with
// decisionAnswer (or skipped); plans use the last three.
const (
	decisionAllow        = "allow"
	decisionAllowSession = "allow_session"
	decisionAllowAlways  = "allow_always"
	decisionDeny         = "deny"
	decisionDenyStop     = "deny_stop"
	decisionAnswer       = "answer"
	decisionSkip         = "skip"
	decisionPlanAccept   = "approve_accept_edits"
	decisionPlanDefault  = "approve_default"
	decisionPlanRevise   = "revise"
)

type ctlOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

type ctlChoice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type ctlQuestion struct {
	ID       string      `json:"id"`
	Header   string      `json:"header,omitempty"`
	Question string      `json:"question"`
	Options  []ctlChoice `json:"options,omitempty"`
	Multi    bool        `json:"multi,omitempty"`
	Other    bool        `json:"other,omitempty"`
	Secret   bool        `json:"secret,omitempty"`
}

// ctlRequest is req-<id>.json. Its first fields match the older MCP bridge's
// file (mcpPermReq), so requests from tasks started before an upgrade still
// parse; Kind is then empty and treated as a plain permission.
type ctlRequest struct {
	ID          string          `json:"id"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	CreatedAt   string          `json:"created_at"`
	ExpiresAt   string          `json:"expires_at"`
	Kind        string          `json:"kind,omitempty"`
	Description string          `json:"description,omitempty"`
	Options     []ctlOption     `json:"options,omitempty"`
	Questions   []ctlQuestion   `json:"questions,omitempty"`
	Plan        string          `json:"plan,omitempty"`
}

// ctlResponse is resp-<id>.json. Answers maps a question id to the chosen
// labels or typed text.
type ctlResponse struct {
	Decision string              `json:"decision"`
	Message  string              `json:"message,omitempty"`
	Answers  map[string][]string `json:"answers,omitempty"`
}

// ctlDone is done-<id>.json: what the agent was finally told.
type ctlDone struct {
	Decision string `json:"decision"` // allowed | denied | answered | expired
	Message  string `json:"message,omitempty"`
	At       string `json:"at"`
}

// requestDetail is what the permissions table keeps beyond tool and input, so
// the page can render the same card after a reload.
func (r ctlRequest) detail() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"description": r.Description, "options": r.Options,
		"questions": r.Questions, "plan": r.Plan,
	})
	return b
}

func (r ctlRequest) kind() string {
	if r.Kind == "" {
		return kindPermission
	}
	return r.Kind
}

// option returns the request's option with this id.
func (r ctlRequest) option(id string) (ctlOption, bool) {
	for _, o := range r.Options {
		if o.ID == id {
			return o, true
		}
	}
	return ctlOption{}, false
}

// statusFor maps a decision to the status stored on the permission row.
func statusFor(decision string) string {
	switch decision {
	case decisionAllow, decisionAllowSession, decisionAllowAlways,
		decisionPlanAccept, decisionPlanDefault, "allowed":
		return "allowed"
	case decisionAnswer:
		return "answered"
	}
	return "denied"
}

// awaitUser publishes req and blocks until the user answers, the request
// expires, or stop closes (the agent exited). ok is false on expiry or stop.
func awaitUser(dir string, req ctlRequest, timeout time.Duration, stop <-chan struct{}) (resp ctlResponse, ok bool) {
	now := time.Now()
	if req.ID == "" {
		req.ID = randomID(9)
	}
	req.CreatedAt = nowRFC3339()
	req.ExpiresAt = now.Add(timeout).UTC().Format(time.RFC3339Nano)
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	if err := atomicWriteJSON(filepath.Join(dir, "req-"+req.ID+".json"), req); err != nil {
		return resp, false
	}
	respPath := filepath.Join(dir, "resp-"+req.ID+".json")
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if b, err := os.ReadFile(respPath); err == nil {
			if json.Unmarshal(b, &resp) == nil && resp.Decision != "" {
				return resp, true
			}
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return resp, false
		case <-stop:
			return resp, false
		}
	}
}

// finishUser records what the agent was told, which also tells the web
// service that the request is no longer pending.
func finishUser(dir, id, decision, message string) {
	_ = atomicWriteJSON(filepath.Join(dir, "done-"+id+".json"),
		ctlDone{Decision: decision, Message: message, At: nowRFC3339()})
}

// ---------------------------------------------------------------------------
// Claude Code: stdio control protocol (--permission-prompt-tool stdio)
// ---------------------------------------------------------------------------

// claudeToolRequest is the body of a can_use_tool control request.
type claudeToolRequest struct {
	ToolName    string           `json:"tool_name"`
	Input       json.RawMessage  `json:"input"`
	Description string           `json:"description"`
	BlockedPath string           `json:"blocked_path"`
	ToolUseID   string           `json:"tool_use_id"`
	Suggestions []map[string]any `json:"permission_suggestions"`
}

var settingsLabels = map[string]string{
	"localSettings":   "项目本地设置",
	"projectSettings": "项目设置",
	"userSettings":    "用户设置",
	"session":         "本会话",
}

// suggestionText describes one permission update the way the CLI lists it.
func suggestionText(s map[string]any) string {
	switch str(s["type"]) {
	case "addRules", "replaceRules":
		rules, _ := s["rules"].([]any)
		var parts []string
		for _, r := range rules {
			m, _ := r.(map[string]any)
			name, content := str(m["toolName"]), str(m["ruleContent"])
			if content != "" {
				parts = append(parts, name+"("+content+")")
			} else if name != "" {
				parts = append(parts, name)
			}
		}
		return strings.Join(parts, "、")
	case "addDirectories":
		dirs, _ := s["directories"].([]any)
		var parts []string
		for _, d := range dirs {
			parts = append(parts, str(d))
		}
		return "访问目录 " + strings.Join(parts, "、")
	case "setMode":
		return "切换为 " + str(s["mode"]) + " 模式"
	}
	return str(s["type"])
}

func joinTexts(ss []map[string]any) string {
	var parts []string
	for _, s := range ss {
		if t := suggestionText(s); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "；")
}

// claudeInteraction turns a can_use_tool request into what the page shows.
func claudeInteraction(r claudeToolRequest) ctlRequest {
	req := ctlRequest{ToolName: r.ToolName, Input: r.Input, ToolUseID: r.ToolUseID, Description: r.Description}
	switch r.ToolName {
	case "AskUserQuestion":
		var in struct {
			Questions []struct {
				Question    string      `json:"question"`
				Header      string      `json:"header"`
				Options     []ctlChoice `json:"options"`
				MultiSelect bool        `json:"multiSelect"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(r.Input, &in)
		req.Kind, req.Description = kindQuestion, ""
		for _, q := range in.Questions {
			// Claude keys answers by the question text.
			req.Questions = append(req.Questions, ctlQuestion{ID: q.Question, Header: q.Header,
				Question: q.Question, Options: q.Options, Multi: q.MultiSelect, Other: true})
		}
		return req
	case "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(r.Input, &in)
		req.Kind, req.Plan, req.Description = kindPlan, in.Plan, ""
		req.Options = []ctlOption{
			{ID: decisionPlanAccept, Label: "批准，自动接受编辑"},
			{ID: decisionPlanDefault, Label: "批准，逐次审批"},
			{ID: decisionPlanRevise, Label: "继续规划"},
		}
		return req
	}
	req.Kind = kindPermission
	if req.Description == "" && r.BlockedPath != "" {
		req.Description = "需要访问 " + r.BlockedPath
	}
	req.Options = []ctlOption{{ID: decisionAllow, Label: "允许"}}
	if len(r.Suggestions) > 0 {
		req.Options = append(req.Options, ctlOption{ID: decisionAllowSession, Label: "本会话允许",
			Detail: joinTexts(r.Suggestions)})
	}
	var persistent []map[string]any
	dest := ""
	for _, s := range r.Suggestions {
		if d := str(s["destination"]); d != "" && d != "session" {
			persistent = append(persistent, s)
			dest = d
		}
	}
	if len(persistent) > 0 {
		label := settingsLabels[dest]
		if label == "" {
			label = dest
		}
		req.Options = append(req.Options, ctlOption{ID: decisionAllowAlways, Label: "始终允许",
			Detail: joinTexts(persistent) + "（写入" + label + "）"})
	}
	req.Options = append(req.Options,
		ctlOption{ID: decisionDeny, Label: "拒绝"},
		ctlOption{ID: decisionDenyStop, Label: "拒绝并停止"})
	return req
}

// claudeReply builds the can_use_tool response for the user's answer, and the
// status and message to record. ok is false when the request expired.
func claudeReply(r claudeToolRequest, resp ctlResponse, ok bool) (reply map[string]any, status, message string) {
	deny := func(msg string, stop bool) (map[string]any, string, string) {
		m := map[string]any{"behavior": "deny", "message": msg}
		if stop {
			m["interrupt"] = true
		}
		return m, "denied", msg
	}
	allow := func(input json.RawMessage, perms []map[string]any) map[string]any {
		m := map[string]any{"behavior": "allow", "updatedInput": input}
		if len(perms) > 0 {
			m["updatedPermissions"] = perms
		}
		return m
	}
	if !ok {
		if r.ToolName == "AskUserQuestion" {
			return deny("用户未在时限内回答", false)
		}
		reply, _, msg := deny("审批超时", false)
		return reply, "expired", msg
	}
	switch r.ToolName {
	case "AskUserQuestion":
		if resp.Decision != decisionAnswer {
			return deny(orDefault(resp.Message, "用户跳过了这个问题"), false)
		}
		var in map[string]any
		_ = json.Unmarshal(r.Input, &in)
		if in == nil {
			in = map[string]any{}
		}
		answers := map[string]string{}
		for q, vals := range resp.Answers {
			answers[q] = strings.Join(vals, ", ")
		}
		in["answers"] = answers
		b, _ := json.Marshal(in)
		return allow(b, nil), "answered", ""
	case "ExitPlanMode":
		switch resp.Decision {
		case decisionPlanAccept, decisionPlanDefault:
			mode := "acceptEdits"
			if resp.Decision == decisionPlanDefault {
				mode = "default"
			}
			return allow(r.Input, []map[string]any{{"type": "setMode", "mode": mode, "destination": "session"}}), "allowed", ""
		}
		return deny(orDefault(resp.Message, "请继续完善计划"), false)
	}
	switch resp.Decision {
	case decisionAllow, "allowed":
		return allow(r.Input, nil), "allowed", ""
	case decisionAllowSession:
		perms := make([]map[string]any, 0, len(r.Suggestions))
		for _, s := range r.Suggestions {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			c["destination"] = "session"
			perms = append(perms, c)
		}
		return allow(r.Input, perms), "allowed", ""
	case decisionAllowAlways:
		return allow(r.Input, r.Suggestions), "allowed", ""
	case decisionDenyStop:
		return deny(orDefault(resp.Message, "用户拒绝并停止了本轮任务"), true)
	}
	return deny(orDefault(resp.Message, "用户拒绝了此次操作"), false)
}

// ---------------------------------------------------------------------------
// Codex: app-server server requests
// ---------------------------------------------------------------------------

// codexDecisionOptions maps Codex's availableDecisions onto the shared ids and
// remembers the exact value to send back for each.
func codexDecisionOptions(available []json.RawMessage) ([]ctlOption, map[string]json.RawMessage) {
	if len(available) == 0 {
		for _, d := range []string{"accept", "acceptForSession", "decline", "cancel"} {
			b, _ := json.Marshal(d)
			available = append(available, b)
		}
	}
	var opts []ctlOption
	wire := map[string]json.RawMessage{}
	add := func(o ctlOption, raw json.RawMessage) {
		if _, dup := wire[o.ID]; dup {
			return
		}
		opts = append(opts, o)
		wire[o.ID] = raw
	}
	var tail []ctlOption // deny options go last, as in the CLI
	var tailRaw []json.RawMessage
	for _, raw := range available {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "accept":
				add(ctlOption{ID: decisionAllow, Label: "允许"}, raw)
			case "acceptForSession":
				add(ctlOption{ID: decisionAllowSession, Label: "本会话允许", Detail: "同类操作本会话不再询问"}, raw)
			case "decline":
				tail, tailRaw = append(tail, ctlOption{ID: decisionDeny, Label: "拒绝"}), append(tailRaw, raw)
			case "cancel":
				tail, tailRaw = append(tail, ctlOption{ID: decisionDenyStop, Label: "拒绝并停止本轮"}), append(tailRaw, raw)
			}
			continue
		}
		var obj map[string]struct {
			Amendment []string `json:"execpolicy_amendment"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			if a, ok := obj["acceptWithExecpolicyAmendment"]; ok {
				add(ctlOption{ID: decisionAllowAlways, Label: "始终允许",
					Detail: "以后不再询问：" + strings.Join(a.Amendment, " ")}, raw)
			}
		}
	}
	for i, o := range tail {
		add(o, tailRaw[i])
	}
	return opts, wire
}

// codexQuestions converts item/tool/requestUserInput questions.
func codexQuestions(params json.RawMessage) []ctlQuestion {
	var p struct {
		Questions []struct {
			ID       string      `json:"id"`
			Header   string      `json:"header"`
			Question string      `json:"question"`
			IsOther  bool        `json:"isOther"`
			IsSecret bool        `json:"isSecret"`
			Options  []ctlChoice `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(params, &p)
	out := make([]ctlQuestion, 0, len(p.Questions))
	for _, q := range p.Questions {
		out = append(out, ctlQuestion{ID: q.ID, Header: q.Header, Question: q.Question,
			Options: q.Options, Other: q.IsOther || len(q.Options) == 0, Secret: q.IsSecret})
	}
	return out
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// describeAnswers summarises answers for the transcript ("配置格式：JSON").
func describeAnswers(qs []ctlQuestion, answers map[string][]string) string {
	var parts []string
	for _, q := range qs {
		vals := answers[q.ID]
		if len(vals) == 0 {
			continue
		}
		v := strings.Join(vals, ", ")
		if q.Secret {
			v = "••••"
		}
		name := q.Header
		if name == "" {
			name = q.Question
		}
		parts = append(parts, fmt.Sprintf("%s：%s", name, v))
	}
	return strings.Join(parts, "；")
}
