package console

import (
	"encoding/json"
	"net/http"
	"strings"

	"rymesh/internal/agent"
	"rymesh/internal/store"
	"rymesh/internal/util"
)

// handleAgentCommands lists the commands the composer offers after "/".
func (s *Server) handleAgentCommands(w http.ResponseWriter, r *http.Request) {
	var out []agent.CommandInfo
	switch r.PathValue("name") {
	case "codex":
		out = agent.CodexCommands
	case "claude":
		// The list the CLI itself reported in its last init message: built-ins,
		// skills and custom commands, as typing "/" in the CLI would show.
		var names []string
		_ = json.Unmarshal([]byte(s.store.GetMeta(agent.ClaudeCommandsMeta)), &names)
		if len(names) == 0 {
			for n := range agent.ClaudeCommandText {
				names = append(names, n)
			}
		}
		seen := map[string]bool{"new": true} // console-provided; added below
		for _, n := range names {
			if strings.HasPrefix(n, "_") || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, agent.CommandInfo{Name: n, Description: agent.ClaudeCommandText[n]})
		}
		out = append(out, agent.CommandInfo{Name: "new", Description: agent.ClaudeCommandText["new"]})
	case "hermes":
		// What Hermes announced in its last session (ACP available commands).
		var list []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal([]byte(s.store.GetMeta(agent.HermesCommandsMeta)), &list)
		for _, c := range list {
			// steer and queue act on a turn still running; a console task is
			// over by the time another message can be sent.
			if c.Name != "" && c.Name != "steer" && c.Name != "queue" {
				out = append(out, agent.CommandInfo{Name: c.Name, Description: c.Description})
			}
		}
		out = append(out, agent.CommandInfo{Name: "new", Description: agent.ClaudeCommandText["new"]})
	default:
		writeErr(w, 404, "未知 Agent")
		return
	}
	writeJSON(w, 200, map[string]any{"commands": out})
}

// handleAgentModels feeds the composer's model picker.
func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.PathValue("name")
	if name != "claude" && name != "codex" && name != "hermes" {
		writeErr(w, 404, "未知 Agent")
		return
	}
	a, _ := s.agents.Get(name)
	var list []agent.ModelChoice
	var err error
	switch name {
	case "claude":
		list, err = agent.ClaudeModelList(r.Context(), a.Binary)
	case "codex":
		list, err = agent.CodexModelList(r.Context(), a.Binary)
	case "hermes":
		list, err = agent.HermesModelList(r.Context(), a.Binary)
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": list})
}

// handleLocalCommand answers the commands the console handles without
// starting the agent. handled is false when the message should run as usual.
func (s *Server) handleLocalCommand(w http.ResponseWriter, sess *store.Session, text string) (handled bool) {
	name, arg, ok := agent.SlashCommand(text)
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
		next := &store.Session{Agent: sess.Agent, Cwd: sess.Cwd, Title: "新会话 · " + label, Source: "web"}
		if err := s.store.CreateSession(next); err != nil {
			writeErr(w, 500, err.Error())
			return true
		}
		settings := s.store.GetSessionSettings(sess.ID)
		_, _ = s.store.UpdateSessionSettings(next.ID, func(st *store.SessionSettings) { *st = settings })
		writeJSON(w, 200, map[string]any{"ok": true, "local": true, "session": next})
		return true
	case "rename":
		if arg == "" {
			reply("用法：/rename <名称>")
			return true
		}
		title := store.TruncateTitle(arg, 120)
		_ = s.store.UpdateSession(sess.ID, &title, nil)
		if sess.Agent == "claude" {
			return false // Claude renames its own session too
		}
		reply("对话已重命名为：" + title)
		return true
	}
	if sess.Agent == "codex" {
		for _, c := range agent.CodexCommands {
			if c.Name == name {
				return false // runs in the runner (internal/agent/codexrpc.go)
			}
		}
		var names []string
		for _, c := range agent.CodexCommands {
			names = append(names, "/"+c.Name)
		}
		reply("未知指令 /" + name + "。可用：" + strings.Join(names, " "))
		return true
	}
	return false
}

// insertLocalExchange records a command and the console's answer in the
// transcript, as the CLI would print them.
func (s *Server) insertLocalExchange(sess *store.Session, text, answer string) {
	taskID := "local-" + util.RandomID(9)
	ts := util.NowRFC3339()
	u, _ := json.Marshal(map[string]any{"text": text})
	a, _ := json.Marshal(map[string]any{"text": answer})
	_, _ = s.store.InsertEvent(&store.Event{SessionID: sess.ID, TaskID: taskID, TaskSeq: 1, TS: ts, Kind: "user", Payload: u})
	_, _ = s.store.InsertEvent(&store.Event{SessionID: sess.ID, TaskID: taskID, TaskSeq: 2, TS: ts, Kind: "assistant", Payload: a})
	_ = s.store.TouchSession(sess.ID)
}
