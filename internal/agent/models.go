package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"rymesh/internal/util"
)

type ModelChoice struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Default     bool     `json:"default,omitempty"`
	Efforts     []string `json:"efforts,omitempty"`
	Description string   `json:"description,omitempty"`
}

// validEffort accepts a reasoning-effort name as the CLI reports it (the
// levels come from its model list, so new ones need no change here).
func validEffort(e string) bool {
	if e == "" || len(e) > 24 || e[0] < 'a' || e[0] > 'z' { // never read as a flag
		return false
	}
	for _, r := range e {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// Every picker request starts a CLI on this node. Only local CLI protocol
// messages travel through the console; the CLI owns authentication and discovery.
// The CLI runs in its own process group so anything it spawned goes with it.
func startModelQuery(ctx context.Context, bin string, args ...string) (io.WriteCloser, io.ReadCloser, func(), error) {
	if bin == "" {
		return nil, nil, nil, errors.New("Agent 不可用")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, nil, nil, err
	}
	return in, out, func() {
		_ = in.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}, nil
}

// CodexModelList asks app-server for the models this Codex account can use.
func CodexModelList(ctx context.Context, bin string) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	in, out, cleanup, err := startModelQuery(ctx, bin, "app-server")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	send := json.NewEncoder(in).Encode
	if err := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "rymesh", "version": util.Version}}}); err != nil {
		return nil, err
	}
	var models []ModelChoice
	seenCursors := map[string]bool{}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if (m.ID == 1 || m.ID == 2) && m.Error != nil {
			return nil, fmt.Errorf("Codex 模型查询失败：%s", m.Error.Message)
		}
		switch m.ID {
		case 1:
			if err := send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
				return nil, err
			}
			if err := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "model/list", "params": map[string]any{}}); err != nil {
				return nil, err
			}
		case 2:
			list, err := parseCodexModels(m.Result)
			if err != nil {
				return nil, err
			}
			models = append(models, list...)
			var page struct {
				NextCursor string `json:"nextCursor"`
			}
			_ = json.Unmarshal(m.Result, &page)
			if page.NextCursor == "" {
				if len(models) == 0 {
					return nil, errors.New("Codex 没有返回可用模型")
				}
				return models, nil
			}
			if seenCursors[page.NextCursor] {
				return nil, errors.New("Codex 返回了重复的模型分页游标")
			}
			seenCursors[page.NextCursor] = true
			if err := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "model/list", "params": map[string]any{"cursor": page.NextCursor}}); err != nil {
				return nil, err
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if sc.Err() != nil {
		return nil, sc.Err()
	}
	return nil, errors.New("Codex 没有返回模型列表")
}

// Claude's SDK initialize control response contains the CLI's model picker,
// including account-specific options and per-model effort capabilities. No user
// prompt is sent, and the query is not saved as a conversation.
func ClaudeModelList(ctx context.Context, bin string) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Only the model list is read: no MCP servers start and no hooks run, so
	// opening the picker has no side effects on the server.
	in, out, cleanup, err := startModelQuery(ctx, bin, "-p", "--input-format", "stream-json",
		"--output-format", "stream-json", "--verbose", "--no-session-persistence",
		"--strict-mcp-config", "--settings", `{"disableAllHooks":true}`)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	const requestID = "models-init"
	if err := json.NewEncoder(in).Encode(map[string]any{"type": "control_request", "request_id": requestID,
		"request": map[string]any{"subtype": "initialize"}}); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Type != "control_response" || m.Response.RequestID != requestID {
			continue
		}
		if m.Response.Subtype != "success" {
			return nil, fmt.Errorf("Claude 模型查询失败：%s", OrDefault(m.Response.Error, "CLI 不支持模型查询"))
		}
		return parseClaudeModels(m.Response.Response)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if sc.Err() != nil {
		return nil, sc.Err()
	}
	return nil, errors.New("Claude 没有返回模型列表，请检查 CLI 版本与登录状态")
}

func parseClaudeModels(result json.RawMessage) ([]ModelChoice, error) {
	var r struct {
		Models []struct {
			Value                 string   `json:"value"`
			DisplayName           string   `json:"displayName"`
			Description           string   `json:"description"`
			SupportedEffortLevels []string `json:"supportedEffortLevels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, err
	}
	if len(r.Models) == 0 {
		return nil, errors.New("Claude 没有返回可用模型，请检查 CLI 版本与登录状态")
	}
	out := make([]ModelChoice, 0, len(r.Models))
	for _, m := range r.Models {
		id := m.Value
		if id == "default" {
			id = ""
		} // Empty settings restore the CLI's default.
		out = append(out, ModelChoice{ID: id, Label: OrDefault(m.DisplayName, m.Value), Default: m.Value == "default",
			Description: m.Description, Efforts: m.SupportedEffortLevels})
	}
	return out, nil
}

func parseCodexModels(result json.RawMessage) ([]ModelChoice, error) {
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
	var out []ModelChoice
	for _, m := range r.Data {
		if m.Hidden {
			continue
		}
		c := ModelChoice{ID: m.ID, Label: OrDefault(m.DisplayName, m.ID), Default: m.IsDefault, Description: m.Description}
		for _, e := range m.SupportedEff {
			c.Efforts = append(c.Efforts, e.Effort)
		}
		out = append(out, c)
	}
	return out, nil
}

// HermesModelList asks `hermes acp` for its models: a new ACP session reports
// them. Nothing is prompted, and Hermes keeps no record of a session that
// never ran a turn.
func HermesModelList(ctx context.Context, bin string) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	in, out, cleanup, err := startModelQuery(ctx, bin, "acp")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	send := json.NewEncoder(in).Encode
	if err := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "rymesh", "version": util.Version}}}); err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == 0 {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("Hermes 模型查询失败：%s", m.Error.Message)
		}
		switch m.ID {
		case 1:
			if err := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new",
				"params": map[string]any{"cwd": home, "mcpServers": []any{}}}); err != nil {
				return nil, err
			}
		case 2:
			return parseACPModels(m.Result)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if sc.Err() != nil {
		return nil, sc.Err()
	}
	return nil, errors.New("Hermes 没有返回模型列表")
}

func parseACPModels(result json.RawMessage) ([]ModelChoice, error) {
	var r struct {
		Models struct {
			Current   string `json:"currentModelId"`
			Available []struct {
				ID          string `json:"modelId"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, err
	}
	var out []ModelChoice
	for _, m := range r.Models.Available {
		out = append(out, ModelChoice{ID: m.ID, Label: OrDefault(m.Name, m.ID), Description: m.Description,
			Default: m.ID == r.Models.Current})
	}
	if len(out) == 0 {
		return nil, errors.New("Hermes 没有返回可用模型")
	}
	return out, nil
}
