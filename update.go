package console

// Update support for the agent CLIs themselves — Codex CLI and Claude Code —
// not for this console. The settings page asks npm for the latest published
// version and installs it with the package manager. A native Claude Code
// install (its binary lives outside node_modules) is updated with the CLI's
// own `claude update` command instead, because npm would update a different
// copy that the PATH never reaches.
//
// Neither CLI keeps a daemon: every task launches a fresh process, so an
// update takes effect for the next task without touching the console.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type agentCLISpec struct {
	Package string
	Label   string
}

var agentCLISpecs = map[string]agentCLISpec{
	"codex":  {Package: "@openai/codex", Label: "Codex CLI"},
	"claude": {Package: "@anthropic-ai/claude-code", Label: "Claude Code"},
}

type AgentUpdateStatus struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed string `json:"installed,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	Method    string `json:"method"`
	Command   string `json:"command"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// agentUpdateMu serializes installs: two npm installs must not share the same
// global prefix concurrently, and a second check would make the UI lie about
// which version is being installed.
var agentUpdateMu sync.Mutex

var agentVersionRE = regexp.MustCompile(`\d+(?:\.\d+)+`)

// agentVersion pulls the first dotted version out of a --version line such as
// "codex-cli 0.163.0" or "2.1.304 (Claude Code)".
func agentVersion(s string) string {
	if m := agentVersionRE.FindString(s); m != "" {
		return m
	}
	return strings.TrimSpace(s)
}

// compareAgentVersions compares dotted numeric versions. Missing segments count
// as zero, so "2.1" equals "2.1.0".
func compareAgentVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func resolveAgentBinary(bin string) string {
	if bin == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(bin); err == nil {
		return r
	}
	return bin
}

// agentUpdateMethod decides how to update a CLI. A binary that resolves into a
// node_modules tree is owned by npm. A native Claude Code install ships its own
// updater. Everything else falls back to npm, which is how both CLIs are
// normally installed.
func agentUpdateMethod(name, bin string) string {
	if bin == "" {
		return "npm"
	}
	p := filepath.ToSlash(resolveAgentBinary(bin))
	if strings.Contains(p, "/node_modules/") {
		return "npm"
	}
	if name == "claude" {
		return "native"
	}
	return "npm"
}

func npmBinary() (string, error) {
	if p, err := exec.LookPath("npm"); err == nil {
		return p, nil
	}
	// The systemd unit's PATH is minimal; these are the usual install points.
	for _, p := range []string{"/usr/bin/npm", "/usr/local/bin/npm", "/bin/npm"} {
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 npm；请先在服务器上安装 Node.js/npm")
}

// commandEnv keeps HOME available for npm's cache when the service runs
// without a login session.
func commandEnv() []string {
	env := os.Environ()
	if os.Getenv("HOME") == "" {
		if h, err := os.UserHomeDir(); err == nil {
			env = append(env, "HOME="+h)
		}
	}
	return env
}

// agentUpdateCommand returns the argv and the human-readable command for the
// agent's install method.
func agentUpdateCommand(name string, info AgentInfo) (method string, argv []string, text string, err error) {
	spec, ok := agentCLISpecs[name]
	if !ok {
		return "", nil, "", fmt.Errorf("未知 Agent: %s", name)
	}
	method = agentUpdateMethod(name, info.Binary)
	if method == "native" {
		bin := info.Binary
		if bin == "" {
			bin = "claude"
		}
		return method, []string{bin, "update"}, "claude update", nil
	}
	npm, err := npmBinary()
	if err != nil {
		return method, nil, "", err
	}
	return method, []string{npm, "install", "-g", spec.Package + "@latest"},
		"npm install -g " + spec.Package + "@latest", nil
}

// latestAgentVersion asks npm for the published version. It is the source of
// truth even for a native Claude Code install, whose releases track the npm
// package. network failures surface as an error instead of a stale "latest".
func latestAgentVersion(pkg string) (string, error) {
	npm, err := npmBinary()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, "view", pkg, "version")
	cmd.Env = commandEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("查询 %s 超时（45 秒）", pkg)
		}
		return "", fmt.Errorf("npm view %s 失败：%s", pkg, tailText(string(out), 400))
	}
	v := agentVersion(string(out))
	if v == "" {
		return "", fmt.Errorf("npm view %s 没有返回版本号", pkg)
	}
	return v, nil
}

// checkAgentUpdates queries npm for every known agent in parallel. A failure
// for one package is reported on that agent only.
func checkAgentUpdates(agents []AgentInfo) []AgentUpdateStatus {
	checkedAt := nowRFC3339()
	type latestResult struct {
		version string
		err     error
	}
	latest := make(map[string]latestResult, len(agentCLISpecs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, spec := range agentCLISpecs {
		wg.Add(1)
		go func(name string, spec agentCLISpec) {
			defer wg.Done()
			v, err := latestAgentVersion(spec.Package)
			mu.Lock()
			latest[name] = latestResult{version: v, err: err}
			mu.Unlock()
		}(name, spec)
	}
	wg.Wait()

	out := make([]AgentUpdateStatus, 0, len(agents))
	for _, a := range agents {
		spec, ok := agentCLISpecs[a.Name]
		if !ok {
			continue
		}
		st := AgentUpdateStatus{
			Name:      a.Name,
			Label:     spec.Label,
			Installed: agentVersion(a.Version),
			Method:    agentUpdateMethod(a.Name, a.Binary),
			CheckedAt: checkedAt,
		}
		if _, _, text, err := agentUpdateCommand(a.Name, a); err == nil {
			st.Command = text
		}
		res := latest[a.Name]
		if res.err != nil {
			st.Error = res.err.Error()
		} else {
			st.Latest = res.version
			st.Available = st.Installed == "" || compareAgentVersions(res.version, st.Installed) > 0
		}
		out = append(out, st)
	}
	return out
}

// installAgentUpdate runs the update command and returns the method and the
// command text. Combined output is returned separately so the caller can show
// it when the exit code is non-zero.
func installAgentUpdate(name string, info AgentInfo) (method, command, output string, err error) {
	method, argv, command, err := agentUpdateCommand(name, info)
	if err != nil {
		return method, command, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = commandEnv()
	out, runErr := cmd.CombinedOutput()
	output = strings.TrimSpace(string(out))
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return method, command, output, fmt.Errorf("更新超时（10 分钟）")
		}
		return method, command, output, fmt.Errorf("%v：%s", runErr, tailText(output, 1200))
	}
	return method, command, output, nil
}

// tailText keeps the end of a command's output: installers print progress and
// then the error, and the error is what the user needs to see.
func tailText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
