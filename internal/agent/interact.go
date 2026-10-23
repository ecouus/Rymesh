package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rymesh/internal/store"
	"rymesh/internal/util"
)

// Decision ids. Permission prompts offer a subset of the first five, chosen
// from what the agent says it supports; questions are answered with
// decisionAnswer (or skipped); plans use the last three.
const (
	DecisionAllow        = "allow"
	DecisionAllowSession = "allow_session"
	decisionAllowAlways  = "allow_always"
	decisionAllowMode    = "allow_mode"
	DecisionDeny         = "deny"
	decisionDenyStop     = "deny_stop"
	DecisionAnswer       = "answer"
	DecisionSkip         = "skip"
	decisionPlanAccept   = "approve_accept_edits"
	decisionPlanDefault  = "approve_default"
	decisionPlanRevise   = "revise"
)

type CtlOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

type ctlChoice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type CtlQuestion struct {
	ID       string      `json:"id"`
	Header   string      `json:"header,omitempty"`
	Question string      `json:"question"`
	Options  []ctlChoice `json:"options,omitempty"`
	Multi    bool        `json:"multi,omitempty"`
	Other    bool        `json:"other,omitempty"`
	Secret   bool        `json:"secret,omitempty"`
}

// CtlRequest is req-<id>.json. Its first fields match the older MCP bridge's
// file (mcpPermReq), so requests from tasks started before an upgrade still
// parse; Kind is then empty and treated as a plain permission.
type CtlRequest struct {
	ID          string          `json:"id"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	CreatedAt   string          `json:"created_at"`
	ExpiresAt   string          `json:"expires_at"`
	Kind        string          `json:"kind,omitempty"`
	Description string          `json:"description,omitempty"`
	Options     []CtlOption     `json:"options,omitempty"`
	Questions   []CtlQuestion   `json:"questions,omitempty"`
	Plan        string          `json:"plan,omitempty"`
}

// CtlResponse is resp-<id>.json. Answers maps a question id to the chosen
// labels or typed text.
type CtlResponse struct {
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
func (r CtlRequest) Detail() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"description": r.Description, "options": r.Options,
		"questions": r.Questions, "plan": r.Plan,
	})
	return b
}

func (r CtlRequest) RequestKind() string {
	if r.Kind == "" {
		return store.KindPermission
	}
	return r.Kind
}

// Option returns the request's option with this id.
func (r CtlRequest) Option(id string) (CtlOption, bool) {
	for _, o := range r.Options {
		if o.ID == id {
			return o, true
		}
	}
	return CtlOption{}, false
}

// StatusFor maps a decision to the status stored on the permission row.
func StatusFor(decision string) string {
	switch decision {
	case DecisionAllow, DecisionAllowSession, decisionAllowAlways, decisionAllowMode,
		decisionPlanAccept, decisionPlanDefault, "allowed":
		return "allowed"
	case DecisionAnswer:
		return "answered"
	}
	return "denied"
}

// awaitUser publishes req and blocks until the user answers, the request
// expires, or stop closes (the agent exited). ok is false on expiry or stop.
func awaitUser(dir string, req CtlRequest, timeout time.Duration, stop <-chan struct{}) (resp CtlResponse, ok bool) {
	now := time.Now()
	if req.ID == "" {
		req.ID = util.RandomID(9)
	}
	req.CreatedAt = util.NowRFC3339()
	req.ExpiresAt = now.Add(timeout).UTC().Format(time.RFC3339Nano)
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	if err := util.AtomicWriteJSON(filepath.Join(dir, "req-"+req.ID+".json"), req); err != nil {
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
	_ = util.AtomicWriteJSON(filepath.Join(dir, "done-"+id+".json"),
		ctlDone{Decision: decision, Message: message, At: util.NowRFC3339()})
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
	switch util.Str(s["type"]) {
	case "addRules", "replaceRules":
		rules, _ := s["rules"].([]any)
		var parts []string
		for _, r := range rules {
			m, _ := r.(map[string]any)
			name, content := util.Str(m["toolName"]), util.Str(m["ruleContent"])
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
			parts = append(parts, shortPath(util.Str(d)))
		}
		return fmt.Sprintf("访问目录 %s", strings.Join(parts, "、"))
	case "setMode":
		return fmt.Sprintf("切换为 %s 模式", util.Str(s["mode"]))
	}
	return util.Str(s["type"])
}

// splitSuggestions separates rule and directory grants from a mode switch.
func splitSuggestions(ss []map[string]any) (rules []map[string]any, mode map[string]any) {
	for _, s := range ss {
		if util.Str(s["type"]) == "setMode" {
			mode = s
		} else {
			rules = append(rules, s)
		}
	}
	return rules, mode
}

func modeLabel(mode string) string {
	switch mode {
	case "acceptEdits":
		return "自动接受编辑"
	case "bypassPermissions":
		return "跳过所有权限检查"
	case "plan":
		return "进入计划模式"
	}
	return fmt.Sprintf("切换为 %s 模式", mode)
}

// shortPath keeps the last two components of a long path ("…/e2e/work"),
// which is what tells directories apart on a phone-width card.
func shortPath(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) <= 4 {
		return p
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
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
func claudeInteraction(r claudeToolRequest) CtlRequest {
	req := CtlRequest{ToolName: r.ToolName, Input: r.Input, ToolUseID: r.ToolUseID, Description: r.Description}
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
		req.Kind, req.Description = store.KindQuestion, ""
		for _, q := range in.Questions {
			// Claude keys answers by the question text.
			req.Questions = append(req.Questions, CtlQuestion{ID: q.Question, Header: q.Header,
				Question: q.Question, Options: q.Options, Multi: q.MultiSelect, Other: true})
		}
		return req
	case "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(r.Input, &in)
		req.Kind, req.Plan, req.Description = store.KindPlan, in.Plan, ""
		req.Options = []CtlOption{
			{ID: decisionPlanAccept, Label: "批准，自动接受编辑"},
			{ID: decisionPlanDefault, Label: "批准，逐次审批"},
			{ID: decisionPlanRevise, Label: "继续规划"},
		}
		return req
	}
	req.Kind = store.KindPermission
	if req.Description == "" && r.BlockedPath != "" {
		req.Description = fmt.Sprintf("需要访问 %s", r.BlockedPath)
	}
	req.Options = []CtlOption{{ID: DecisionAllow, Label: "允许"}}
	rules, mode := splitSuggestions(r.Suggestions)
	if len(rules) > 0 {
		req.Options = append(req.Options, CtlOption{ID: DecisionAllowSession, Label: "本会话允许",
			Detail: joinTexts(rules)})
	}
	// A mode switch changes how everything else in the session runs, so it is
	// its own choice (as in the CLI), never folded into "allow for session".
	if mode != nil {
		req.Options = append(req.Options, CtlOption{ID: decisionAllowMode,
			Label: fmt.Sprintf("允许，并在本会话%s", modeLabel(util.Str(mode["mode"])))})
	}
	var persistent []map[string]any
	dest := ""
	for _, s := range rules {
		if d := util.Str(s["destination"]); d != "" && d != "session" {
			persistent = append(persistent, s)
			dest = d
		}
	}
	if len(persistent) > 0 {
		label := settingsLabels[dest]
		if label == "" {
			label = dest
		}
		req.Options = append(req.Options, CtlOption{ID: decisionAllowAlways, Label: "始终允许",
			Detail: fmt.Sprintf("%s（写入%s）", joinTexts(persistent), label)})
	}
	req.Options = append(req.Options,
		CtlOption{ID: DecisionDeny, Label: "拒绝"},
		CtlOption{ID: decisionDenyStop, Label: "拒绝并停止"})
	return req
}

// claudeReply builds the can_use_tool response for the user's answer, and the
// status and message to record. ok is false when the request expired.
func claudeReply(r claudeToolRequest, resp CtlResponse, ok bool) (reply map[string]any, status, message string) {
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
		if resp.Decision != DecisionAnswer {
			return deny(OrDefault(resp.Message, "用户跳过了这个问题"), false)
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
		return deny(OrDefault(resp.Message, "请继续完善计划"), false)
	}
	switch resp.Decision {
	case DecisionAllow, "allowed":
		return allow(r.Input, nil), "allowed", ""
	case decisionAllowMode:
		_, mode := splitSuggestions(r.Suggestions)
		if mode == nil {
			return allow(r.Input, nil), "allowed", ""
		}
		m := map[string]any{"type": "setMode", "mode": mode["mode"], "destination": "session"}
		return allow(r.Input, []map[string]any{m}), "allowed", ""
	case DecisionAllowSession:
		rules, _ := splitSuggestions(r.Suggestions)
		perms := make([]map[string]any, 0, len(rules))
		for _, s := range rules {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			c["destination"] = "session"
			perms = append(perms, c)
		}
		return allow(r.Input, perms), "allowed", ""
	case decisionAllowAlways:
		rules, _ := splitSuggestions(r.Suggestions)
		return allow(r.Input, rules), "allowed", ""
	case decisionDenyStop:
		return deny(OrDefault(resp.Message, "用户拒绝并停止了本轮任务"), true)
	}
	return deny(OrDefault(resp.Message, "用户拒绝了此次操作"), false)
}

// ---------------------------------------------------------------------------
// Codex: app-server server requests
// ---------------------------------------------------------------------------

// codexDecisionOptions maps Codex's availableDecisions onto the shared ids and
// remembers the exact value to send back for each.
func codexDecisionOptions(available []json.RawMessage) ([]CtlOption, map[string]json.RawMessage) {
	if len(available) == 0 {
		for _, d := range []string{"accept", "acceptForSession", "decline", "cancel"} {
			b, _ := json.Marshal(d)
			available = append(available, b)
		}
	}
	var opts []CtlOption
	wire := map[string]json.RawMessage{}
	add := func(o CtlOption, raw json.RawMessage) {
		if _, dup := wire[o.ID]; dup {
			return
		}
		opts = append(opts, o)
		wire[o.ID] = raw
	}
	var tail []CtlOption // deny options go last, as in the CLI
	var tailRaw []json.RawMessage
	for _, raw := range available {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "accept":
				add(CtlOption{ID: DecisionAllow, Label: "允许"}, raw)
			case "acceptForSession":
				add(CtlOption{ID: DecisionAllowSession, Label: "本会话允许", Detail: "同类操作本会话不再询问"}, raw)
			case "decline":
				tail, tailRaw = append(tail, CtlOption{ID: DecisionDeny, Label: "拒绝"}), append(tailRaw, raw)
			case "cancel":
				tail, tailRaw = append(tail, CtlOption{ID: decisionDenyStop, Label: "拒绝并停止本轮"}), append(tailRaw, raw)
			}
			continue
		}
		var obj map[string]struct {
			Amendment []string `json:"execpolicy_amendment"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			if a, ok := obj["acceptWithExecpolicyAmendment"]; ok {
				add(CtlOption{ID: decisionAllowAlways, Label: "始终允许",
					Detail: fmt.Sprintf("以后不再询问：%s", strings.Join(a.Amendment, " "))}, raw)
			}
		}
	}
	for i, o := range tail {
		add(o, tailRaw[i])
	}
	return opts, wire
}

// codexQuestions converts item/tool/requestUserInput questions.
func codexQuestions(params json.RawMessage) []CtlQuestion {
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
	out := make([]CtlQuestion, 0, len(p.Questions))
	for _, q := range p.Questions {
		out = append(out, CtlQuestion{ID: q.ID, Header: q.Header, Question: q.Question,
			Options: q.Options, Other: q.IsOther || len(q.Options) == 0, Secret: q.IsSecret})
	}
	return out
}

func OrDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// DescribeAnswers summarises answers for the transcript ("配置格式：JSON").
func DescribeAnswers(qs []CtlQuestion, answers map[string][]string) string {
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
