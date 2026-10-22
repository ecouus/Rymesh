package console

// Slash commands, so the console behaves like the CLI it drives.
//
// Claude Code runs its own slash commands from stream-json input (the reply
// is the CLI's), but each console message is a fresh `claude -p --resume`
// process, so "this session only" settings such as /model and /effort would
// be forgotten by the next message. The console therefore keeps them per
// session and passes them as --model / --effort; it saves them only after the
// CLI confirmed the change, so the CLI stays the one that validates.
//
// Codex app-server has no slash commands; the console implements Codex's on
// top of the protocol (codexrpc.go): /model, /effort, /compact, /review,
// /status. /clear and /new start a new conversation for both agents, and
// /rename also renames the console session.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionSettings are per-conversation choices applied to every message.
type SessionSettings struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

func (s *Store) GetSessionSettings(id string) SessionSettings {
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(s.GetSessionMeta(id)), &m)
	var st SessionSettings
	_ = json.Unmarshal(m["settings"], &st)
	return st
}

// UpdateSessionSettings changes the settings stored in the session's meta,
// next to (never over) the import cursor that shares the document.
func (s *Store) UpdateSessionSettings(id string, fn func(*SessionSettings)) (SessionSettings, error) {
	m := map[string]any{}
	if cur := strings.TrimSpace(s.GetSessionMeta(id)); cur != "" {
		_ = json.Unmarshal([]byte(cur), &m)
	}
	st := s.GetSessionSettings(id)
	fn(&st)
	m["settings"] = st
	b, err := json.Marshal(m)
	if err != nil {
		return st, err
	}
	_, err = s.db.Exec(`UPDATE sessions SET meta=? WHERE id=?`, string(b), id)
	return st, err
}

// slashCommand splits "/model sonnet" into ("model", "sonnet").
func slashCommand(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || strings.HasPrefix(t, "//") {
		return "", "", false
	}
	name, arg, _ = strings.Cut(t[1:], " ")
	if name == "" || strings.ContainsAny(name, "/\n") {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(arg), true
}

// codexCommands are the Codex CLI commands the console implements.
var codexCommands = []commandInfo{
	{Name: "model", Description: "选择模型和推理强度", Args: "[模型] [强度]"},
	{Name: "effort", Description: "设置推理强度", Args: "<low|medium|high|xhigh|max|ultra>"},
	{Name: "compact", Description: "压缩对话上下文，节省上下文窗口"},
	{Name: "review", Description: "审查当前未提交的改动（可附加说明）", Args: "[说明]"},
	{Name: "status", Description: "显示当前会话的模型、权限与目录"},
	{Name: "new", Description: "开始新的对话"},
	{Name: "clear", Description: "开始新的对话（清空上下文）"},
	{Name: "rename", Description: "重命名对话", Args: "<名称>"},
}

// Descriptions for Claude's built-in commands; skills and custom commands
// from the CLI's own list show by name.
var claudeCommandText = map[string]string{
	"model": "切换模型", "effort": "设置推理强度", "compact": "压缩对话上下文",
	"clear": "开始新的对话（清空上下文）", "new": "开始新的对话", "context": "查看上下文占用",
	"cost": "查看用量", "usage": "查看用量", "init": "生成 CLAUDE.md 项目说明",
	"review": "审查代码改动", "rename": "重命名对话", "mcp": "查看 MCP 服务器",
	"agents": "查看子代理", "config": "查看配置", "memory": "查看记忆", "status": "查看状态",
	"output-style": "设置输出风格", "security-review": "安全审查", "pr-comments": "查看 PR 评论",
	"todos": "查看待办", "release-notes": "查看更新说明", "doctor": "诊断安装",
}

type commandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Args        string `json:"args,omitempty"`
}

const claudeCommandsMeta = "slash_commands.claude"

// handleAgentCommands lists the commands the composer offers after "/".
func (s *Server) handleAgentCommands(w http.ResponseWriter, r *http.Request) {
	var out []commandInfo
	switch r.PathValue("name") {
	case "codex":
		out = codexCommands
	case "claude":
		// The list the CLI itself reported in its last init message: built-ins,
		// skills and custom commands, as typing "/" in the CLI would show.
		var names []string
		_ = json.Unmarshal([]byte(s.store.GetMeta(claudeCommandsMeta)), &names)
		if len(names) == 0 {
			for n := range claudeCommandText {
				names = append(names, n)
			}
		}
		seen := map[string]bool{"new": true} // console-provided; added below
		for _, n := range names {
			if strings.HasPrefix(n, "_") || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, commandInfo{Name: n, Description: claudeCommandText[n]})
		}
		out = append(out, commandInfo{Name: "new", Description: claudeCommandText["new"]})
	default:
		writeErr(w, 404, "未知 Agent")
		return
	}
	writeJSON(w, 200, map[string]any{"commands": out})
}

type modelChoice struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Default     bool     `json:"default,omitempty"`
	Efforts     []string `json:"efforts,omitempty"`
	Description string   `json:"description,omitempty"`
}

// claudeModels mirrors the aliases `claude /model` lists.
var claudeModels = []modelChoice{
	{ID: "", Label: "默认", Default: true},
	{ID: "best", Label: "best（最强可用）"},
	{ID: "opus", Label: "Opus"}, {ID: "sonnet", Label: "Sonnet"}, {ID: "haiku", Label: "Haiku"},
	{ID: "fable", Label: "Fable"}, {ID: "opusplan", Label: "Opus 规划 + Sonnet 执行"},
	{ID: "opus[1m]", Label: "Opus（1M 上下文）"}, {ID: "sonnet[1m]", Label: "Sonnet（1M 上下文）"},
	{ID: "fable[1m]", Label: "Fable（1M 上下文）"},
}

var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

var (
	codexModelsMu  sync.Mutex
	codexModelsAt  time.Time
	codexModelsVal []modelChoice
)

// codexModelList asks app-server for the models this Codex account can use.
func codexModelList(ctx context.Context, bin string) ([]modelChoice, error) {
	codexModelsMu.Lock()
	defer codexModelsMu.Unlock()
	if codexModelsVal != nil && time.Since(codexModelsAt) < 10*time.Minute {
		return codexModelsVal, nil
	}
	if bin == "" {
		return nil, errors.New("Codex 不可用")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); _ = cmd.Wait() }()
	send := func(v any) { b, _ := json.Marshal(v); _, _ = in.Write(append(b, '\n')) }
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "agentconsole", "version": version}}})
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.ID {
		case 1:
			send(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
			send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "model/list", "params": map[string]any{}})
		case 2:
			list, err := parseCodexModels(m.Result)
			if err == nil {
				codexModelsVal, codexModelsAt = list, time.Now()
			}
			return list, err
		}
	}
	return nil, errors.New("Codex 没有返回模型列表")
}

func parseCodexModels(result json.RawMessage) ([]modelChoice, error) {
	var r struct {
		Data []struct {
			ID           string `json:"id"`
			DisplayName  string `json:"displayName"`
			IsDefault    bool   `json:"isDefault"`
			Hidden       bool   `json:"hidden"`
			Description  string `json:"description"`
			SupportedEff []struct {
				Effort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, err
	}
	var out []modelChoice
	for _, m := range r.Data {
		if m.Hidden {
			continue
		}
		c := modelChoice{ID: m.ID, Label: orDefault(m.DisplayName, m.ID), Default: m.IsDefault, Description: m.Description}
		for _, e := range m.SupportedEff {
			c.Efforts = append(c.Efforts, e.Effort)
		}
		out = append(out, c)
	}
	return out, nil
}

// handleAgentModels feeds the composer's model picker.
func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("name") {
	case "claude":
		writeJSON(w, 200, map[string]any{"models": claudeModels, "efforts": claudeEfforts})
	case "codex":
		a, _ := s.agents.Get("codex")
		list, err := codexModelList(r.Context(), a.Binary)
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"models": list})
	default:
		writeErr(w, 404, "未知 Agent")
	}
}

// handleLocalCommand answers the commands the console handles without
// starting the agent. handled is false when the message should run as usual.
func (s *Server) handleLocalCommand(w http.ResponseWriter, sess *Session, text string) (handled bool) {
	name, arg, ok := slashCommand(text)
	if !ok {
		return false
	}
	reply := func(msg string) {
		s.insertLocalExchange(sess, text, msg)
		writeJSON(w, 200, map[string]any{"ok": true, "local": true})
	}
	switch name {
	case "clear", "new":
		// The CLI starts a fresh conversation; so does the console. The old
		// one stays in the list, as it stays in the CLI's /resume.
		label := sess.Agent
		if a, ok := s.agents.Get(sess.Agent); ok && a.Label != "" {
			label = a.Label
		}
		next := &Session{Agent: sess.Agent, Cwd: sess.Cwd, Title: "新会话 · " + label, Source: "web"}
		if err := s.store.CreateSession(next); err != nil {
			writeErr(w, 500, err.Error())
			return true
		}
		settings := s.store.GetSessionSettings(sess.ID)
		_, _ = s.store.UpdateSessionSettings(next.ID, func(st *SessionSettings) { *st = settings })
		writeJSON(w, 200, map[string]any{"ok": true, "local": true, "session": next})
		return true
	case "rename":
		if arg == "" {
			reply("用法：/rename <名称>")
			return true
		}
		title := truncateTitle(arg, 120)
		_ = s.store.UpdateSession(sess.ID, &title, nil)
		if sess.Agent == "claude" {
			return false // Claude renames its own session too
		}
		reply("对话已重命名为：" + title)
		return true
	}
	if sess.Agent == "codex" {
		for _, c := range codexCommands {
			if c.Name == name {
				return false // runs in the runner (codexrpc.go)
			}
		}
		var names []string
		for _, c := range codexCommands {
			names = append(names, "/"+c.Name)
		}
		reply("未知指令 /" + name + "。可用：" + strings.Join(names, " "))
		return true
	}
	return false
}

// insertLocalExchange records a command and the console's answer in the
// transcript, as the CLI would print them.
func (s *Server) insertLocalExchange(sess *Session, text, answer string) {
	taskID := "local-" + randomID(9)
	ts := nowRFC3339()
	u, _ := json.Marshal(map[string]any{"text": text})
	a, _ := json.Marshal(map[string]any{"text": answer})
	_, _ = s.store.InsertEvent(&Event{SessionID: sess.ID, TaskID: taskID, TaskSeq: 1, TS: ts, Kind: "user", Payload: u})
	_, _ = s.store.InsertEvent(&Event{SessionID: sess.ID, TaskID: taskID, TaskSeq: 2, TS: ts, Kind: "assistant", Payload: a})
	_ = s.store.TouchSession(sess.ID)
}

// claudeSettingFrom recognises a /model or /effort the CLI confirmed. Only a
// confirmation is saved, so a name the CLI rejects never sticks.
func claudeSettingFrom(prompt, result string) (map[string]any, bool) {
	name, arg, ok := slashCommand(prompt)
	if !ok || arg == "" {
		return nil, false
	}
	switch {
	case name == "model" && strings.HasPrefix(result, "Set model to"):
		if arg == "default" {
			arg = ""
		}
		return map[string]any{"model": arg}, true
	case name == "effort" && strings.HasPrefix(result, "Set effort level to"):
		if arg == "auto" {
			arg = ""
		}
		return map[string]any{"effort": arg}, true
	}
	return nil, false
}
