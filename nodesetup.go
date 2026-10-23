package console

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rymesh/internal/config"
	"rymesh/internal/sys"
	"rymesh/internal/util"
)

const nodeServiceName = "rymesh-node"

func agentHome() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".rymesh"), nil
}

// personalConfig is a fresh config for one user's machine: loopback only,
// rooted in their home directory; the page is reached through a hub or a
// login ticket.
func personalConfig(base string, port int) (*config.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c, err := config.NewConfig()
	if err != nil {
		return nil, err
	}
	c.Port = port
	c.ListenAddrs = []string{"127.0.0.1:" + strconv.Itoa(port)}
	c.EasyTierIface = ""
	c.DataDir = filepath.Join(base, "data")
	c.LogDir = filepath.Join(base, "log")
	c.DefaultCWD = home
	c.AllowedRoots = []string{home}
	c.ImportOnStart = true
	if h, err := os.Hostname(); err == nil {
		c.NodeName = strings.TrimSuffix(h, ".local")
	}
	return c, nil
}

type setupResult struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Port    int    `json:"port"`
	Service string `json:"service"`
	Config  string `json:"config"`
	Warning string `json:"warning,omitempty"`
}

func nodeSetupMain(args []string) int {
	fset := flag.NewFlagSet("node setup", flag.ContinueOnError)
	token := fset.String("token", "", "node token the client will use (required)")
	port := fset.Int("port", config.DefaultPort, "loopback port for a new install")
	name := fset.String("name", "", "device name")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	res, err := nodeSetup(*token, *port, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("安装失败:"), Tr(err.Error()))
		return 1
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	return 0
}

func nodeSetup(token string, port int, name string) (*setupResult, error) {
	if len(token) < 16 {
		return nil, errors.New("需要 --token（至少 16 个字符）")
	}
	base, err := agentHome()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	cfgPath := filepath.Join(base, "config.json")
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取已有配置失败：%w", err)
		}
		if cfg, err = personalConfig(base, port); err != nil {
			return nil, err
		}
	}
	// An existing install keeps its port, directories and settings; only the
	// token the client will present, and the name if given, change.
	cfg.NodeToken = token
	if name != "" {
		cfg.NodeName = name
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		return nil, err
	}
	res := &setupResult{OK: true, Version: util.Version, Port: cfg.Port, Config: cfgPath}
	res.Service, res.Warning = startNodeService(exe, cfgPath, base)
	if !waitHealthy(cfg.Port, 15*time.Second) {
		return nil, fmt.Errorf("节点已安装（%s），但 15 秒内没有在 127.0.0.1:%d 响应；日志在 %s",
			res.Service, cfg.Port, filepath.Join(cfg.LogDir, "rymesh.log"))
	}
	return res, nil
}

func waitHealthy(port int, timeout time.Duration) bool {
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/health"
	c := &http.Client{Timeout: 2 * time.Second, Transport: nodeTransport}
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(400 * time.Millisecond) {
		if resp, err := c.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
	}
	return false
}

// startNodeService registers the node with the platform's service manager and
// (re)starts it, falling back to a detached process. The returned warning
// explains a fallback.
func startNodeService(exe, cfgPath, base string) (service, warning string) {
	home, _ := os.UserHomeDir()
	path := servicePath() // where codex and claude live
	// A detached instance from an earlier fallback would hold the port and
	// make the managed service fail to bind.
	stopDetached(base)
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() == 0 {
			unit := "/etc/systemd/system/" + nodeServiceName + ".service"
			if err := os.WriteFile(unit, []byte(nodeUnit(exe, cfgPath, home, path, "multi-user.target")), 0o644); err == nil {
				if systemctl(false, "daemon-reload") == nil && systemctl(false, "enable", nodeServiceName) == nil &&
					systemctl(false, "restart", nodeServiceName) == nil {
					return "systemd", ""
				}
			}
			warning = "systemd 不可用，已改为后台进程（重启后不会自动启动）"
		} else {
			dir := filepath.Join(home, ".config", "systemd", "user")
			unit := filepath.Join(dir, nodeServiceName+".service")
			if os.MkdirAll(dir, 0o755) == nil &&
				os.WriteFile(unit, []byte(nodeUnit(exe, cfgPath, home, path, "default.target")), 0o644) == nil &&
				systemctl(true, "daemon-reload") == nil && systemctl(true, "enable", nodeServiceName) == nil &&
				systemctl(true, "restart", nodeServiceName) == nil {
				// Without linger, user services stop when the last session ends.
				if err := exec.Command("loginctl", "enable-linger", os.Getenv("USER")).Run(); err != nil {
					return "systemd-user", "未能开启 linger：退出 SSH 后节点可能随会话停止（可由管理员执行 loginctl enable-linger）"
				}
				return "systemd-user", ""
			}
			warning = "用户级 systemd 不可用，已改为后台进程（重启后不会自动启动）"
		}
	case "darwin":
		if err := launchdInstall(exe, cfgPath, home, path, base); err == nil {
			return "launchd", ""
		} else if errors.Is(err, errLaunchdDisabled) {
			warning = fmt.Sprintf("%v。现在先以普通后台进程运行", err)
		} else {
			warning = fmt.Sprintf("launchd 注册失败（%v），已改为后台进程", err)
		}
	}
	if err := startDetached(exe, cfgPath, base); err != nil {
		return "none", fmt.Sprintf("无法启动节点：%v", err)
	}
	return "nohup", warning
}

// servicePath is the PATH the service runs agents with. A process started by
// Finder or launchd has only the system directories, so the user's shell is
// asked for the PATH it sets up (nvm, Homebrew, ~/.local/bin...).
func servicePath() string {
	cur := os.Getenv("PATH")
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// -i as well as -l: many setups (nvm) extend PATH only for interactive shells.
	out, err := exec.CommandContext(ctx, sh, "-ilc", `printf '\n__ACPATH__%s\n' "$PATH"`).Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if p, ok := strings.CutPrefix(line, "__ACPATH__"); ok && p != "" {
				return mergePath(p, cur)
			}
		}
	}
	return cur
}

func mergePath(first, second string) string {
	seen := map[string]bool{}
	var out []string
	for _, d := range strings.Split(first+":"+second, ":") {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, ":")
}

// nodeUnit mirrors the main service unit: KillMode=process lets running agent
// tasks (detached runners) outlive a restart or upgrade of the node.
func nodeUnit(exe, cfgPath, home, path, wantedBy string) string {
	return fmt.Sprintf(`[Unit]
Description=Rymesh node
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve --config %s
WorkingDirectory=%s
Environment=HOME=%s
Environment=PATH=%s
Restart=always
RestartSec=2
KillMode=process
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=%s
`, exe, cfgPath, home, home, path, wantedBy)
}

func systemctl(user bool, args ...string) error {
	if user {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

const launchdLabel = "com.rymesh.node"

var errLaunchdDisabled = errors.New("后台服务已在“系统设置 → 通用 → 登录项与扩展”中被关闭（允许在后台：rymesh）。打开它之后，控制台会随系统启动、崩溃后自动恢复")

// launchdInstall registers a LaunchAgent. AbandonProcessGroup keeps detached
// runners alive when launchd restarts the node, like KillMode=process.
func launchdInstall(exe, cfgPath, home, path, base string) error {
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	plist := filepath.Join(dir, launchdLabel+".plist")
	logPath := filepath.Join(base, "log", "launchd.out")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>serve</string><string>--config</string><string>%s</string></array>
  <key>EnvironmentVariables</key>
  <dict><key>HOME</key><string>%s</string><key>PATH</key><string>%s</string></dict>
  <key>WorkingDirectory</key><string>%s</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>AbandonProcessGroup</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlEsc(exe), xmlEsc(cfgPath), xmlEsc(home), xmlEsc(path), xmlEsc(home), xmlEsc(logPath), xmlEsc(logPath))
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	// Turning the app off under System Settings → General → Login Items
	// disables the label in launchd; every bootstrap then fails with an I/O
	// error. That is the user's choice, so say so instead of overriding it.
	if out, err := exec.Command("launchctl", "print-disabled", "gui/"+uid).Output(); err == nil &&
		strings.Contains(string(out), `"`+launchdLabel+`" => disabled`) {
		return errLaunchdDisabled
	}
	// gui/<uid> exists while the user is logged in at the console; over SSH
	// only user/<uid> may. Try both.
	var lastErr error
	for _, domain := range []string{"gui/" + uid, "user/" + uid} {
		target := domain + "/" + launchdLabel
		_ = exec.Command("launchctl", "bootout", target).Run()
		// bootout returns before the old job is gone; bootstrapping too early
		// fails with "Input/output error". Wait for it to disappear (up to
		// launchd's own 20 s exit timeout).
		for i := 0; i < 200 && exec.Command("launchctl", "print", target).Run() == nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		for attempt := 0; attempt < 3; attempt++ {
			out, err := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput()
			if err == nil {
				return nil
			}
			lastErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			time.Sleep(500 * time.Millisecond)
		}
	}
	return lastErr
}

func isRymesh(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	return err == nil && (strings.Contains(string(out), "rymesh"))
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// startDetached is the last resort: a background process in its own session,
// replacing the one recorded in node.pid.
// stopDetached stops the background process started by startDetached, if any.
func stopDetached(base string) {
	pidPath := filepath.Join(base, "node.pid")
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	// A stale pid file can name an unrelated process that reused the number;
	// only stop it if it is really rymesh.
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 && sys.ProcessAlive(pid) && isRymesh(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 50 && sys.ProcessAlive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	_ = os.Remove(pidPath)
}

func startDetached(exe, cfgPath, base string) error {
	pidPath := filepath.Join(base, "node.pid")
	stopDetached(base)
	logf, err := os.OpenFile(filepath.Join(base, "log", "node.out"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "serve", "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Started by the Mac app (or anything launched from Finder) this process
	// has only the system directories on PATH; the agents live where the
	// user's shell puts them (nvm, Homebrew, ~/.local/bin).
	cmd.Env = withEnv(os.Environ(), "PATH", servicePath())
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	return cmd.Process.Release()
}

// withEnv returns env with key set to value, replacing an earlier setting.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
