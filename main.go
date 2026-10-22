package console

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "1.1.1"

var agents *AgentSet

// Main is the agentconsole command line (cmd/agentconsole).
func Main() {
	agents = newAgentSet()
	if len(os.Args) < 2 {
		help()
		return
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serveMain(os.Args[2:]))
	case "install":
		os.Exit(installMain(os.Args[2:]))
	case "status":
		os.Exit(statusMain(os.Args[2:]))
	case "password":
		os.Exit(passwordMain(os.Args[2:]))
	case "import":
		os.Exit(importMain(os.Args[2:]))
	case "node":
		os.Exit(nodeMain(os.Args[2:]))
	case "client":
		os.Exit(clientMain(os.Args[2:]))
	case "tasks":
		os.Exit(tasksMain(os.Args[2:]))
	case "cancel":
		os.Exit(cancelMain(os.Args[2:]))
	case "logs":
		os.Exit(logsMain(os.Args[2:]))
	case "doctor":
		os.Exit(doctorMain(os.Args[2:]))
	case "run-task":
		os.Exit(runTaskCLI(os.Args[2:]))
	case "mcp-permission":
		os.Exit(mcpPermissionCLI(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("agentconsole " + version)
	case "help", "--help", "-h":
		help()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		help()
		os.Exit(2)
	}
}

func help() {
	fmt.Print(`agentconsole — 多 Agent Web 控制台（Codex CLI / Claude Code）

用法:
  agentconsole client              在本机启动控制台（首次自动配置、注册后台服务）并免密打开浏览器
  agentconsole client open         在浏览器中打开本机控制台
  agentconsole node setup ...      在本机安装/升级节点（通常由客户端通过 SSH 自动执行）
  agentconsole node token|add|list 管理节点令牌与远程设备
  agentconsole install [选项]      在服务器上生成配置、安装 systemd、开机自启并启动
  agentconsole serve               前台运行 Web 服务（systemd 使用）
  agentconsole status              查看服务、Agent、任务与会话状态
  agentconsole password            查看当前密码
  agentconsole password --set NEW  修改密码并重启服务
  agentconsole password --generate 生成新密码并重启服务
  agentconsole password --force-change
                                   设置密码后要求下次登录时修改
  agentconsole import              重新导入原生 Codex / Claude 历史会话
  agentconsole tasks [--all]       查看任务
  agentconsole cancel <task-id>    取消任务
  agentconsole logs [-n 100]       查看服务日志
  agentconsole doctor              环境自检
  agentconsole version             版本

install 选项:
  --config PATH       配置文件（默认 /etc/agentconsole/config.json）
  --port PORT         监听端口（默认 8899）
  --data DIR          数据目录（默认 /var/lib/agentconsole）
  --log DIR           日志目录（默认 /var/log/agentconsole）
  --no-start          只写配置和 systemd 单元，不启动
  --reset-password    将登录密码重置为默认密码 admin，并要求首次登录修改

安全说明:
  - 只监听 127.0.0.1 与 EasyTier 网卡地址，不监听 0.0.0.0。
  - Codex 非交互模式无人工审批通道，默认使用 read-only 沙箱；可选择
    workspace-write（仍受沙箱限制），绝不使用 danger-full-access。
  - Claude Code 通过 MCP permission-prompt-tool 提供逐次人工批准/拒绝。
`)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func serveMain(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		fmt.Fprintln(os.Stderr, "请先运行: agentconsole install")
		return 1
	}
	if err := cfg.ensureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "创建目录失败:", err)
		return 1
	}
	logPath := filepath.Join(cfg.LogDir, "agentconsole.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开日志失败:", err)
		return 1
	}
	defer logFile.Close()
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "", log.LstdFlags|log.Lmicroseconds)
	st, err := openStore(cfg.DataDir)
	if err != nil {
		logger.Printf("打开数据库失败: %v", err)
		return 1
	}
	defer st.Close()
	addrs, err := listenAddresses(cfg)
	if err != nil {
		logger.Printf("监听地址无效: %v", err)
		return 1
	}
	srv := newServer(cfg, *configPath, st, logger, addrs)
	srv.warmNodes()

	if cfg.ImportOnStart {
		rep := srv.runImport()
		logger.Printf("原生会话导入: codex=%d claude=%d imported=%d skipped=%d excluded=%d errors=%d",
			rep.CodexFiles, rep.ClaudeFiles, rep.Imported, rep.Skipped, rep.Excluded, rep.Errors)
		go srv.autoImport(30 * time.Second)
	}
	srv.ing.Reconcile()
	srv.schedule()

	// Every request's context derives from baseCtx. Shutdown alone waits for
	// SSE streams, which never finish on their own, so a stop took the full
	// timeout — long enough for launchd to refuse re-registering the service.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       180 * time.Second,
		// WriteTimeout intentionally unset: SSE connections are long-lived.
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}
	var listeners []net.Listener
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Printf("监听 %s 失败: %v", addr, err)
			continue
		}
		listeners = append(listeners, ln)
		logger.Printf("listening on http://%s", addr)
		go func(l net.Listener) {
			if err := httpSrv.Serve(l); err != nil && err != http.ErrServerClosed {
				logger.Printf("serve %s: %v", l.Addr(), err)
			}
		}(ln)
	}
	if len(listeners) == 0 {
		logger.Printf("没有任何监听地址可用")
		return 1
	}
	_ = os.MkdirAll("/run/agentconsole", 0o755)
	pidPath := pidFilePath(addrs)
	_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
	defer os.Remove(pidPath)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Printf("收到 %s，正在停止 Web 服务（后台 Agent 任务继续运行）", sig)
	cancelBase() // end live streams and relays now
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	logger.Printf("Web 服务已停止；运行中的 Agent 进程不受影响")
	return 0
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func installMain(args []string) int {
	fset := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	port := fset.Int("port", 0, "listen port")
	dataDir := fset.String("data", "", "data dir")
	logDir := fset.String("log", "", "log dir")
	noStart := fset.Bool("no-start", false, "do not start")
	resetPW := fset.Bool("reset-password", false, "reset password")
	unitPath := fset.String("unit", "/etc/systemd/system/agentconsole.service", "systemd unit path")
	noEnable := fset.Bool("no-enable", false, "do not enable at boot")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*configPath)
	newPW := ""
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "读取配置失败:", err)
			return 1
		}
		cfg, newPW, err = newConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成配置失败:", err)
			return 1
		}
	}
	if *port > 0 {
		cfg.Port = *port
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *logDir != "" {
		cfg.LogDir = *logDir
	}
	// password.txt is removed once the password is changed, so its absence
	// must not be read as "no password yet": that would reset it to the default.
	if *resetPW || newPW == "" && cfg.PasswordHash == "" {
		cfg.PasswordHash = hashPassword(defaultPassword)
		cfg.MustChangePassword = true
		newPW = defaultPassword
	}
	if err := cfg.normalize(); err != nil {
		fmt.Fprintln(os.Stderr, "配置无效:", err)
		return 1
	}
	if err := cfg.ensureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "创建目录失败:", err)
		return 1
	}
	if err := saveConfig(*configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "保存配置失败:", err)
		return 1
	}
	if newPW != "" {
		_ = os.WriteFile(filepath.Join(filepath.Dir(*configPath), "password.txt"),
			[]byte(newPW+"\n"), 0o600)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.Abs(exe)
	unit := systemdUnit(exe, *configPath)
	if err := os.WriteFile(*unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写入 systemd 单元失败:", err)
		return 1
	}
	if out, err := runSystemctl("daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "systemctl daemon-reload 失败: %v\n%s\n", err, out)
	}
	if !*noEnable {
		if out, err := runSystemctl("enable", "agentconsole"); err != nil {
			fmt.Fprintf(os.Stderr, "systemctl enable 失败: %v\n%s\n", err, out)
		}
	}
	if !*noStart {
		if out, err := runSystemctl("restart", "agentconsole"); err != nil {
			fmt.Fprintf(os.Stderr, "systemctl restart 失败: %v\n%s\n", err, out)
			return 1
		}
		waitHealth(cfg.Port, 15*time.Second)
	}
	addrs, _ := listenAddresses(cfg)
	fmt.Println()
	fmt.Println("Agent Console 已安装")
	fmt.Println("  配置:   " + *configPath)
	fmt.Println("  数据:   " + cfg.DataDir)
	fmt.Println("  日志:   " + filepath.Join(cfg.LogDir, "agentconsole.log"))
	fmt.Println("  单元:   " + *unitPath)
	for _, a := range addrs {
		fmt.Println("  访问:   http://" + a)
	}
	if pw := readPasswordFile(*configPath); pw != "" {
		fmt.Println("  密码:   " + pw + "   (agentconsole password 可再次查看)")
	}
	fmt.Println()
	fmt.Println("手机连接 EasyTier 后，打开上面的 EasyTier 地址即可。")
	return 0
}

// systemdUnit renders the service definition. It is a pure function of its
// arguments so tests can assert the unit never points into a temporary build
// directory (which happened when the binary was installed from /tmp).
func systemdUnit(exe, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Agent Console — Codex / Claude Code multi-agent web console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve --config %s
WorkingDirectory=%s
Environment=HOME=/root
Environment=PATH=/root/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=always
RestartSec=2
KillMode=process
KillSignal=SIGTERM
TimeoutStopSec=15
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, exe, configPath, filepath.Dir(exe))
}

func runSystemctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func waitHealth(port int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readPasswordFile(configPath string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "password.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pidFilePath keeps separate instances from clobbering each other's PID file
// (for example a test instance on another port). The port is part of the name.
func pidFilePath(addrs []string) string {
	tag := "default"
	if len(addrs) > 0 {
		if _, port, err := net.SplitHostPort(addrs[0]); err == nil && port != "" {
			tag = port
		}
	}
	return filepath.Join("/run/agentconsole", "agentconsole-"+tag+".pid")
}

// ---------------------------------------------------------------------------
// CLI utilities
// ---------------------------------------------------------------------------

func statusMain(args []string) int {
	fset := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	_ = fset.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return 1
	}
	fmt.Println("agentconsole", version)
	fmt.Println("配置:", *configPath)
	fmt.Println("数据:", cfg.DataDir)
	fmt.Println("日志:", filepath.Join(cfg.LogDir, "agentconsole.log"))
	addrs, _ := listenAddresses(cfg)
	for _, a := range addrs {
		fmt.Println("监听: http://" + a)
	}
	if pid := readPID(pidFilePath(addrs)); pid > 0 && processAlive(pid) {
		fmt.Println("Web 服务: running (pid", pid, ")")
	} else {
		fmt.Println("Web 服务: not running (或非 systemd 启动)")
	}
	if out, err := runSystemctl("is-active", "agentconsole"); err == nil {
		fmt.Println("systemd:", out)
	} else {
		fmt.Println("systemd:", out)
	}
	st, err := openStore(cfg.DataDir)
	if err == nil {
		defer st.Close()
		sc, _ := st.SourceCounts()
		tc, _ := st.TaskCounts()
		sess, _ := st.ListSessions("", "", true)
		fmt.Printf("会话: %d (web=%d imported=%d)\n", len(sess), sc["web"], sc["imported"])
		fmt.Printf("任务: running=%d waiting=%d pending=%d completed=%d failed=%d interrupted=%d cancelled=%d\n",
			tc[StatusRunning], tc[StatusWaitingInput], tc[StatusPending], tc[StatusCompleted],
			tc[StatusFailed], tc[StatusInterrupted], tc[StatusCancelled])
	}
	for _, a := range agents.List(false) {
		state := "unavailable"
		if a.Available {
			state = "available"
		}
		auth := "unauth"
		if a.Authenticated {
			auth = "auth"
		}
		fmt.Printf("Agent %-7s %-10s %-6s %s\n", a.Name, state, auth, a.Version)
	}
	return 0
}

func passwordMain(args []string) int {
	fset := flag.NewFlagSet("password", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	set := fset.String("set", "", "set a new password")
	gen := fset.Bool("generate", false, "generate a new password")
	forceChange := fset.Bool("force-change", false, "require a password change on next login")
	noRestart := fset.Bool("no-restart", false, "do not restart service")
	_ = fset.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return 1
	}
	if *set == "" && !*gen {
		pw := readPasswordFile(*configPath)
		if pw == "" {
			fmt.Fprintln(os.Stderr, "没有保存明文密码（修改密码后不再保留）；请用 --generate 或 --set 重置。")
			return 1
		}
		fmt.Println(pw)
		return 0
	}
	pw := *set
	if *gen || pw == "" {
		pw, err = randomPassword()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	cfg.PasswordHash = hashPassword(pw)
	cfg.MustChangePassword = *forceChange
	if err := saveConfig(*configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "保存失败:", err)
		return 1
	}
	// The new password is shown once here and not persisted in plain text.
	_ = os.Remove(filepath.Join(filepath.Dir(*configPath), "password.txt"))
	fmt.Println("新密码:", pw)
	if !*noRestart {
		if out, err := runSystemctl("restart", "agentconsole"); err != nil {
			fmt.Fprintf(os.Stderr, "重启服务失败: %v\n%s\n", err, out)
		} else {
			fmt.Println("服务已重启；后台 Agent 任务因 KillMode=process 不受影响。")
		}
	}
	return 0
}

func importMain(args []string) int {
	fset := flag.NewFlagSet("import", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	_ = fset.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return 1
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		return 1
	}
	defer st.Close()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	imp := newImporter(st, logger, cfg.ImportExcludeRoots, cfg.ImportHistoryLimit)
	rep := imp.ImportAll()
	b, _ := json.Marshal(rep)
	_ = st.SetMeta("last_import_report", string(b))
	_ = st.SetMeta("last_import_at", rep.FinishedAt)
	fmt.Printf("Codex 文件: %d\nClaude 文件: %d\n新增导入: %d\n跳过: %d\n排除: %d\n错误: %d\n",
		rep.CodexFiles, rep.ClaudeFiles, rep.Imported, rep.Skipped, rep.Excluded, rep.Errors)
	for _, e := range rep.ErrorSamples {
		fmt.Println("  error:", e)
	}
	return 0
}

func tasksMain(args []string) int {
	fset := flag.NewFlagSet("tasks", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	all := fset.Bool("all", false, "include finished tasks")
	_ = fset.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	var statuses []string
	if !*all {
		statuses = []string{StatusRunning, StatusWaitingInput, StatusPending}
	}
	list, _ := st.ListTasks(statuses, 100)
	if len(list) == 0 {
		fmt.Println("没有任务")
		return 0
	}
	fmt.Printf("%-14s %-10s %-9s %-8s %-20s %s\n", "ID", "STATUS", "AGENT", "PID", "CREATED", "PROMPT")
	for _, t := range list {
		fmt.Printf("%-14s %-10s %-9s %-8d %-20s %s\n", t.ID, t.Status, t.Agent, t.PID,
			t.CreatedAt, truncateTitle(t.Prompt, 60))
	}
	return 0
}

func cancelMain(args []string) int {
	fset := flag.NewFlagSet("cancel", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	_ = fset.Parse(args)
	rest := fset.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "用法: agentconsole cancel <task-id>")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	t, err := st.GetTask(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "任务不存在")
		return 1
	}
	switch t.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		fmt.Println("任务已结束:", t.Status)
		return 0
	case StatusPending:
		_ = st.FinishTask(t.ID, StatusCancelled, nil, "用户取消（尚未启动）", nowRFC3339())
		fmt.Println("已取消")
		return 0
	}
	_ = st.RequestCancel(t.ID)
	killTask(t, syscall.SIGTERM)
	fmt.Println("已发送停止信号；状态会由后台写入")
	return 0
}

func logsMain(args []string) int {
	fset := flag.NewFlagSet("logs", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	n := fset.Int("n", 100, "number of lines")
	_ = fset.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	path := filepath.Join(cfg.LogDir, "agentconsole.log")
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	lines := tailLines(f, *n)
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}

func tailLines(f *os.File, n int) []string {
	if n <= 0 {
		n = 100
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var ring []string
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	return ring
}

func doctorMain(args []string) int {
	fset := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
	_ = fset.Parse(args)
	ok := true
	fmt.Println("== 配置 ==")
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Println("✗", err)
		return 1
	}
	fmt.Println("✓", *configPath)
	fmt.Println("== 目录 ==")
	for _, d := range []string{cfg.DataDir, cfg.LogDir} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			fmt.Println("✓", d)
		} else {
			fmt.Println("✗", d, err)
			ok = false
		}
	}
	fmt.Println("== EasyTier ==")
	if iface, err := net.InterfaceByName(cfg.EasyTierIface); err == nil {
		addrs, _ := iface.Addrs()
		fmt.Println("✓", cfg.EasyTierIface)
		for _, a := range addrs {
			fmt.Println("  ", a.String())
		}
	} else {
		fmt.Println("✗ 网卡", cfg.EasyTierIface, "不存在")
		ok = false
	}
	fmt.Println("== 监听地址 ==")
	addrs, err := listenAddresses(cfg)
	if err != nil {
		fmt.Println("✗", err)
		ok = false
	}
	for _, a := range addrs {
		fmt.Println("  ", a)
	}
	fmt.Println("== Agent ==")
	for _, a := range agents.List(true) {
		mark := "✓"
		if !a.Available {
			mark = "✗"
			ok = false
		}
		fmt.Printf("%s %-7s %s %s\n", mark, a.Name, a.Version, a.AuthDetail)
	}
	fmt.Println("== systemd ==")
	if out, err := runSystemctl("is-active", "agentconsole"); err == nil {
		fmt.Println("✓", out)
	} else {
		fmt.Println("!", out)
	}
	fmt.Println("== 数据库 ==")
	if st, err := openStore(cfg.DataDir); err == nil {
		st.Close()
		fmt.Println("✓", filepath.Join(cfg.DataDir, "agentconsole.db"))
	} else {
		fmt.Println("✗", err)
		ok = false
	}
	if ok {
		fmt.Println("\n所有关键检查通过")
		return 0
	}
	return 1
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
	return pid
}

func runTaskCLI(args []string) int {
	fset := flag.NewFlagSet("run-task", flag.ContinueOnError)
	dir := fset.String("task-dir", "", "task directory")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "run-task: --task-dir required")
		return 2
	}
	return runTaskMain(*dir)
}

func mcpPermissionCLI(args []string) int {
	fset := flag.NewFlagSet("mcp-permission", flag.ContinueOnError)
	dir := fset.String("dir", "", "control dir")
	timeout := fset.Int("timeout", 3600, "timeout seconds")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "mcp-permission: --dir required")
		return 2
	}
	return mcpPermissionMain(*dir, *timeout)
}
