package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildAgentCommand(t *testing.T) {
	old := Agents
	Agents = &AgentSet{at: time.Now(), agents: []AgentInfo{
		{Name: "codex", Binary: "/bin/echo", Available: true},
		{Name: "claude", Binary: "/bin/echo", Available: true},
	}}
	defer func() { Agents = old }()

	// Codex always runs app-server; the mode travels in the JSON-RPC requests.
	spec := TaskSpec{Agent: "codex", Cwd: "/tmp/work", IsNew: true, Profile: "read-only"}
	cmd, err := buildAgentCommand(spec, "/tmp/control")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(cmd.Args[1:], " "); args != "app-server" {
		t.Fatalf("codex args = %q, want app-server", args)
	}
	for profile, want := range map[string][3]string{
		"read-only":          {"read-only", "on-request", "default"},
		"workspace-write":    {"workspace-write", "on-request", "default"},
		"plan":               {"read-only", "on-request", "plan"},
		"danger-full-access": {"danger-full-access", "never", "default"},
		"workspace":          {"read-only", "on-request", "default"}, // unknown falls back
	} {
		sb, ap, mode := codexModes(profile)
		if [3]string{sb, ap, mode} != want {
			t.Fatalf("codexModes(%s) = %s %s %s", profile, sb, ap, mode)
		}
	}

	cspec := TaskSpec{Agent: "claude", Cwd: "/tmp/work", IsNew: true, NativeID: "11111111-2222-4333-8444-555555555555",
		PermTimeout: 60, Profile: "manual"}
	cmd, err = buildAgentCommand(cspec, "/tmp/control")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--input-format stream-json", "--output-format stream-json",
		"--permission-prompt-tool stdio", "--permission-mode default",
		"--session-id 11111111-2222-4333-8444-555555555555"} {
		if !strings.Contains(args, want) {
			t.Fatalf("claude new args %q missing %q", args, want)
		}
	}
	if strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("manual mode must not bypass permissions: %q", args)
	}

	// Every installed Claude mode is forwarded as-is.
	for _, mode := range []string{"acceptEdits", "plan", "auto", "dontAsk"} {
		cspec.Profile = mode
		cmd, _ = buildAgentCommand(cspec, "/tmp/control")
		modeArgs := strings.Join(cmd.Args, " ")
		if !strings.Contains(modeArgs, "--permission-mode "+mode) {
			t.Fatalf("claude mode %s not forwarded: %q", mode, modeArgs)
		}
		if strings.Contains(modeArgs, "--dangerously-skip-permissions") {
			t.Fatalf("claude mode %s unexpectedly bypasses permissions", mode)
		}
	}

	cspec.Profile = "bypassPermissions"
	cmd, _ = buildAgentCommand(cspec, "/tmp/control")
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--permission-mode bypassPermissions") ||
		!strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("claude bypass mode incomplete: %q", args)
	}
	cspec.Profile = "manual"
	cspec.IsNew = false
	cmd, _ = buildAgentCommand(cspec, "/tmp/control")
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--resume 11111111-2222-4333-8444-555555555555") ||
		strings.Contains(args, "--session-id") {
		t.Fatalf("claude resume args wrong: %q", args)
	}
}

func TestExtractNativeID(t *testing.T) {
	cases := []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"thread_id": "t1"}, "t1"},
		{map[string]any{"session_id": "s1"}, "s1"},
		{map[string]any{"subtype": "init", "session_id": "s2"}, "s2"},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := ExtractNativeID(c.raw); got != c.want {
			t.Fatalf("extractNativeID(%v) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// One-click update / restart
// ---------------------------------------------------------------------------

func TestAgentVersionExtraction(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.163.0":     "0.163.0",
		"2.1.304 (Claude Code)": "2.1.304",
		"v1.2.3\n":              "1.2.3",
		"1.2":                   "1.2",
		"no version here":       "no version here",
		"":                      "",
	}
	for in, want := range cases {
		if got := AgentVersion(in); got != want {
			t.Errorf("agentVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareAgentVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.163.0", "0.164.0", -1},
		{"0.164.0", "0.163.0", 1},
		{"2.1.304", "2.1.304", 0},
		{"2.1", "2.1.0", 0},
		{"2.10.0", "2.9.9", 1},
	}
	for _, c := range cases {
		if got := compareAgentVersions(c.a, c.b); got != c.want {
			t.Errorf("compareAgentVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// A binary inside node_modules belongs to npm; a native Claude Code install
// must use its own updater, because npm would update a different copy.
func TestAgentUpdateMethod(t *testing.T) {
	dir := t.TempDir()
	npmPath := filepath.Join(dir, "lib", "node_modules", "@openai", "codex", "bin", "codex.js")
	if got := agentUpdateMethod("codex", npmPath); got != "npm" {
		t.Errorf("node_modules codex method = %q, want npm", got)
	}
	if got := agentUpdateMethod("claude", filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")); got != "npm" {
		t.Errorf("node_modules claude method = %q, want npm", got)
	}
	if got := agentUpdateMethod("claude", filepath.Join(dir, ".local", "share", "claude", "versions", "2.1.304")); got != "native" {
		t.Errorf("native claude method = %q, want native", got)
	}
	if got := agentUpdateMethod("codex", ""); got != "npm" {
		t.Errorf("missing binary method = %q, want npm", got)
	}
}
