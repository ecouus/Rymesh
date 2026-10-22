package console

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AgentInfo struct {
	Name                string `json:"name"`
	Label               string `json:"label"`
	Binary              string `json:"binary,omitempty"`
	Version             string `json:"version,omitempty"`
	Available           bool   `json:"available"`
	Authenticated       bool   `json:"authenticated"`
	AuthDetail          string `json:"auth_detail"`
	SupportsResume      bool   `json:"supports_resume"`
	SupportsStream      bool   `json:"supports_stream"`
	SupportsPermissions bool   `json:"supports_permissions"`
	PermissionDetail    string `json:"permission_detail"`
	Model               string `json:"model,omitempty"`
	Error               string `json:"error,omitempty"`
}

type AgentSet struct {
	mu         sync.Mutex
	at         time.Time
	agents     []AgentInfo
	refreshing bool
	homeDir    string
}

func newAgentSet() *AgentSet {
	h, _ := os.UserHomeDir()
	return &AgentSet{homeDir: h}
}

// List returns the detected agents. Detection runs each CLI with --version
// (~160 ms of Node start-up), so once the cache is stale the old result is
// served and refreshed in the background; only the first call and an explicit
// force (after an update or restart) wait for it.
func (a *AgentSet) List(force bool) []AgentInfo {
	a.mu.Lock()
	if force || a.agents == nil {
		defer a.mu.Unlock()
		a.agents = []AgentInfo{a.detectCodex(), a.detectClaude()}
		a.at = time.Now()
		return a.agents
	}
	if time.Since(a.at) >= 30*time.Second && !a.refreshing {
		a.refreshing = true
		go func() {
			fresh := []AgentInfo{a.detectCodex(), a.detectClaude()}
			a.mu.Lock()
			a.agents, a.at, a.refreshing = fresh, time.Now(), false
			a.mu.Unlock()
		}()
	}
	out := a.agents
	a.mu.Unlock()
	return out
}

func (a *AgentSet) Get(name string) (AgentInfo, bool) {
	for _, x := range a.List(false) {
		if x.Name == name {
			return x, true
		}
	}
	return AgentInfo{}, false
}

func runBrief(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *AgentSet) detectCodex() AgentInfo {
	info := AgentInfo{
		Name:                "codex",
		Label:               "Codex CLI",
		SupportsResume:      true,
		SupportsStream:      true,
		SupportsPermissions: false,
		PermissionDetail:    "非交互 exec 模式没有人工审批通道；使用只读/工作区沙箱限制副作用，绝不启用 danger-full-access。",
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		info.Error = "未安装（PATH 中找不到 codex）"
		return info
	}
	info.Binary = bin
	info.Available = true
	if out, err := runBrief(6*time.Second, bin, "--version"); err == nil {
		info.Version = firstLine(out)
	} else {
		info.Available = false
		info.Error = "codex --version 失败"
	}
	auth := filepath.Join(a.homeDir, ".codex", "auth.json")
	if st, err := os.Stat(auth); err == nil && st.Size() > 0 {
		info.Authenticated = true
		info.AuthDetail = "复用 " + shortenHome(auth, a.homeDir) + " 中的现有登录状态"
		if b, err := os.ReadFile(auth); err == nil {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			if _, ok := m["OPENAI_API_KEY"]; ok {
				info.AuthDetail = "复用 ~/.codex/auth.json（API key 或 OAuth）"
			}
		}
	} else {
		info.AuthDetail = "未检测到 ~/.codex/auth.json，请先在服务器上运行 codex login"
	}
	// Read only the model key from config.toml; never expose the file.
	if b, err := os.ReadFile(filepath.Join(a.homeDir, ".codex", "config.toml")); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "model") && strings.Contains(line, "=") && !strings.HasPrefix(line, "model_") {
				info.Model = strings.Trim(strings.TrimSpace(strings.SplitN(line, "=", 2)[1]), `"'`)
				break
			}
		}
	}
	return info
}

func (a *AgentSet) detectClaude() AgentInfo {
	info := AgentInfo{
		Name:                "claude",
		Label:               "Claude Code",
		SupportsResume:      true,
		SupportsStream:      true,
		SupportsPermissions: true,
		PermissionDetail:    "通过 stdio 控制协议在网页上审批与回答提问，等待状态可跨浏览器断线与服务重启保留。",
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		info.Error = "未安装（PATH 中找不到 claude）"
		return info
	}
	info.Binary = bin
	info.Available = true
	if out, err := runBrief(10*time.Second, bin, "--version"); err == nil {
		info.Version = firstLine(out)
	} else {
		info.Available = false
		info.Error = "claude --version 失败"
	}
	settingsPath := filepath.Join(a.homeDir, ".claude", "settings.json")
	if b, err := os.ReadFile(settingsPath); err == nil {
		var m struct {
			Model string            `json:"model"`
			Env   map[string]string `json:"env"`
		}
		if json.Unmarshal(b, &m) == nil {
			info.Model = m.Model
			if m.Env["ANTHROPIC_AUTH_TOKEN"] != "" || m.Env["ANTHROPIC_API_KEY"] != "" || m.Env["ANTHROPIC_BASE_URL"] != "" {
				info.Authenticated = true
				info.AuthDetail = "复用 Claude Code 现有环境/登录配置（不读取、不向前端传输凭据）"
			}
		}
	}
	if !info.Authenticated {
		if st, err := os.Stat(filepath.Join(a.homeDir, ".claude", ".credentials.json")); err == nil && st.Size() > 0 {
			info.Authenticated = true
			info.AuthDetail = "复用 ~/.claude 中的现有登录状态"
		}
	}
	if !info.Authenticated {
		if out, err := runBrief(10*time.Second, bin, "auth", "status"); err == nil &&
			(strings.Contains(strings.ToLower(out), "logged") || strings.Contains(out, "已登录")) {
			info.Authenticated = true
			info.AuthDetail = "claude auth status: 已登录"
		}
	}
	if !info.Authenticated {
		info.AuthDetail = "未检测到现有认证，请先在服务器上运行 claude 完成登录"
	}
	return info
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func shortenHome(p, home string) string {
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~/" + strings.TrimPrefix(p, home+string(filepath.Separator))
	}
	return p
}
