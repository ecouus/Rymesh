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

	"rymesh/internal/agent"
	"rymesh/internal/config"
	"rymesh/internal/importer"
	"rymesh/internal/store"
	"rymesh/internal/sys"
	"rymesh/internal/util"
)

func listenAddresses(c *config.Config) ([]string, error) {
	if len(c.ListenAddrs) > 0 {
		var out []string
		for _, a := range c.ListenAddrs {
			host, _, err := net.SplitHostPort(a)
			if err != nil {
				return nil, fmt.Errorf("invalid listen address %q: %w", a, err)
			}
			ip := net.ParseIP(host)
			if ip != nil && (ip.IsUnspecified()) && !c.AllowPublicBind {
				return nil, fmt.Errorf("refusing to bind %s without allow_public_bind", a)
			}
			out = append(out, a)
		}
		return out, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(ip string) {
		a := net.JoinHostPort(ip, fmt.Sprintf("%d", c.Port))
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	add("127.0.0.1")
	if iface, err := net.InterfaceByName(c.EasyTierIface); err == nil {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil || ip.IsLoopback() {
				continue
			}
			add(ip.String())
		}
	}
	return out, nil
}

// Main is the rymesh command line (cmd/rymesh).
func Main() {
	agent.Agents = agent.NewAgentSet()
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
	case "version", "--version", "-v":
		fmt.Println("rymesh " + util.Version)
	case "help", "--help", "-h":
		help()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		help()
		os.Exit(2)
	}
}

func help() {
	fmt.Print(T(`rymesh — 多 Agent Web 控制台（Codex CLI / Claude Code）

用法:
  rymesh client              在本机启动控制台（首次自动配置、注册后台服务）并免密打开浏览器
  rymesh client open         在浏览器中打开本机控制台
  rymesh node setup ...      在本机安装/升级节点（通常由客户端通过 SSH 自动执行）
  rymesh node token|add|list 管理节点令牌与远程设备
  rymesh install [选项]      在服务器上生成配置、安装 systemd、开机自启并启动
  rymesh serve               前台运行 Web 服务（systemd 使用）
  rymesh status              查看服务、Agent、任务与会话状态
  rymesh import              重新导入原生 Codex / Claude 历史会话
  rymesh tasks [--all]       查看任务
  rymesh cancel <task-id>    取消任务
  rymesh logs [-n 100]       查看服务日志
  rymesh doctor              环境自检
  rymesh version             版本

install 选项:
  --config PATH       配置文件（默认 /etc/rymesh/config.json）
  --port PORT         监听端口（默认 8899）
  --data DIR          数据目录（默认 /var/lib/rymesh）
  --log DIR           日志目录（默认 /var/log/rymesh）
  --no-start          只写配置和 systemd 单元，不启动

安全说明:
  - 没有密码登录：Mac / Android 应用和 rymesh client open 用一次性票据登录，
    主控用节点令牌访问节点。
  - 只监听 127.0.0.1 与 EasyTier 网卡地址，不监听 0.0.0.0。
  - Agent 的权限请求与提问出现在网页上，由你选择允许一次、本会话允许、始终允许或拒绝，
    与终端里的 CLI 一致。
`))
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func serveMain(args []string) int {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("读取配置失败:"), Tr(err.Error()))
		fmt.Fprintln(os.Stderr, T("请先运行: rymesh install"))
		return 1
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, T("创建目录失败:"), Tr(err.Error()))
		return 1
	}
	logPath := filepath.Join(cfg.LogDir, "rymesh.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("打开日志失败:"), Tr(err.Error()))
		return 1
	}
	defer logFile.Close()
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "", log.LstdFlags|log.Lmicroseconds)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		logger.Printf("open database: %v", err)
		return 1
	}
	defer st.Close()
	addrs, err := listenAddresses(cfg)
	if err != nil {
		logger.Printf("invalid listen address: %v", err)
		return 1
	}
	srv := newServer(cfg, *configPath, st, logger, addrs)
	srv.warmNodes()

	if cfg.ImportOnStart {
		// In the background: a first import that reads a few hundred Codex
		// threads through the app-server takes a while, and an upgrade waits
		// for the node to answer.
		go func() {
			rep := srv.runImport()
			logger.Printf("native history import: codex=%d threads=%d claude=%d hermes=%d imported=%d skipped=%d excluded=%d errors=%d",
				rep.CodexFiles, rep.CodexThreads, rep.ClaudeFiles, rep.HermesSessions, rep.Imported, rep.Skipped,
				rep.Excluded, rep.Errors)
			srv.autoImport(30 * time.Second)
		}()
	}
	go srv.pruneTrashLoop()
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
			logger.Printf("listen on %s: %v", addr, err)
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
		logger.Printf("no listen address available")
		return 1
	}
	_ = os.MkdirAll("/run/rymesh", 0o755)
	pidPath := pidFilePath(addrs)
	_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
	defer os.Remove(pidPath)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Printf("got %s, stopping the web service (agent tasks keep running)", sig)
	cancelBase() // end live streams and relays now
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	logger.Printf("web service stopped; running agent processes are not affected")
	return 0
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func installMain(args []string) int {
	fset := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	port := fset.Int("port", 0, "listen port")
	dataDir := fset.String("data", "", "data dir")
	logDir := fset.String("log", "", "log dir")
	noStart := fset.Bool("no-start", false, "do not start")
	unitPath := fset.String("unit", "/etc/systemd/system/rymesh.service", "systemd unit path")
	noEnable := fset.Bool("no-enable", false, "do not enable at boot")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, T("读取配置失败:"), Tr(err.Error()))
			return 1
		}
		cfg, err = config.NewConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, T("生成配置失败:"), Tr(err.Error()))
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
	if err := cfg.Normalize(); err != nil {
		fmt.Fprintln(os.Stderr, T("配置无效:"), Tr(err.Error()))
		return 1
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, T("创建目录失败:"), Tr(err.Error()))
		return 1
	}
	if err := config.SaveConfig(*configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, T("保存配置失败:"), Tr(err.Error()))
		return 1
	}
	// Left by versions that had a password login.
	_ = os.Remove(filepath.Join(filepath.Dir(*configPath), "password.txt"))
	exe, _ := os.Executable()
	exe, _ = filepath.Abs(exe)
	unit := systemdUnit(exe, *configPath)
	if err := os.WriteFile(*unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, T("写入 systemd 单元失败:"), Tr(err.Error()))
		return 1
	}
	if out, err := runSystemctl("daemon-reload"); err != nil {
		fmt.Fprint(os.Stderr, T("systemctl daemon-reload 失败: %v\n%s\n", err, out))
	}
	if !*noEnable {
		if out, err := runSystemctl("enable", "rymesh"); err != nil {
			fmt.Fprint(os.Stderr, T("systemctl enable 失败: %v\n%s\n", err, out))
		}
	}
	if !*noStart {
		if out, err := runSystemctl("restart", "rymesh"); err != nil {
			fmt.Fprint(os.Stderr, T("systemctl restart 失败: %v\n%s\n", err, out))
			return 1
		}
		waitHealth(cfg.Port, 15*time.Second)
	}
	addrs, _ := listenAddresses(cfg)
	fmt.Println()
	fmt.Println(T("Rymesh 已安装"))
	fmt.Println(T("  配置:   %s", *configPath))
	fmt.Println(T("  数据:   %s", cfg.DataDir))
	fmt.Println(T("  日志:   %s", filepath.Join(cfg.LogDir, "rymesh.log")))
	fmt.Println(T("  单元:   %s", *unitPath))
	for _, a := range addrs {
		fmt.Println(T("  访问:   http://%s", a))
	}
	fmt.Println()
	fmt.Println(T("在 Mac 或 Android 应用里通过 SSH 添加这台服务器即可使用（部署会接管本安装）。"))
	return 0
}

// systemdUnit renders the service definition. It is a pure function of its
// arguments so tests can assert the unit never points into a temporary build
// directory (which happened when the binary was installed from /tmp).
func systemdUnit(exe, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Rymesh — remote console for Codex CLI / Claude Code
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

// pidFilePath keeps separate instances from clobbering each other's PID file
// (for example a test instance on another port). The port is part of the name.
func pidFilePath(addrs []string) string {
	tag := "default"
	if len(addrs) > 0 {
		if _, port, err := net.SplitHostPort(addrs[0]); err == nil && port != "" {
			tag = port
		}
	}
	return filepath.Join("/run/rymesh", "rymesh-"+tag+".pid")
}

// ---------------------------------------------------------------------------
// CLI utilities
// ---------------------------------------------------------------------------

func statusMain(args []string) int {
	fset := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	_ = fset.Parse(args)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("读取配置失败:"), Tr(err.Error()))
		return 1
	}
	fmt.Println("rymesh", util.Version)
	fmt.Println(T("配置:"), *configPath)
	fmt.Println(T("数据:"), cfg.DataDir)
	fmt.Println(T("日志:"), filepath.Join(cfg.LogDir, "rymesh.log"))
	addrs, _ := listenAddresses(cfg)
	for _, a := range addrs {
		fmt.Println(T("监听: http://%s", a))
	}
	if pid := readPID(pidFilePath(addrs)); pid > 0 && sys.ProcessAlive(pid) {
		fmt.Println(T("Web 服务: running (pid %d)", pid))
	} else {
		fmt.Println(T("Web 服务: not running (或非 systemd 启动)"))
	}
	if out, err := runSystemctl("is-active", "rymesh"); err == nil {
		fmt.Println("systemd:", out)
	} else {
		fmt.Println("systemd:", out)
	}
	st, err := store.OpenStore(cfg.DataDir)
	if err == nil {
		defer st.Close()
		sc, _ := st.SourceCounts()
		tc, _ := st.TaskCounts()
		sess, _ := st.ListSessions("", "", true)
		fmt.Print(T("会话: %d (web=%d imported=%d)\n", len(sess), sc["web"], sc["imported"]))
		fmt.Print(T("任务: running=%d waiting=%d pending=%d completed=%d failed=%d interrupted=%d cancelled=%d\n",
			tc[store.StatusRunning], tc[store.StatusWaitingInput], tc[store.StatusPending], tc[store.StatusCompleted],
			tc[store.StatusFailed], tc[store.StatusInterrupted], tc[store.StatusCancelled]))
	}
	for _, a := range agent.Agents.List(false) {
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

func importMain(args []string) int {
	fset := flag.NewFlagSet("import", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	_ = fset.Parse(args)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("读取配置失败:"), Tr(err.Error()))
		return 1
	}
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("打开数据库失败:"), Tr(err.Error()))
		return 1
	}
	defer st.Close()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	imp := importer.NewImporter(st, logger, cfg.ImportExcludeRoots, cfg.ImportHistoryLimit)
	rep := imp.ImportAll()
	b, _ := json.Marshal(rep)
	_ = st.SetMeta("last_import_report", string(b))
	_ = st.SetMeta("last_import_at", rep.FinishedAt)
	fmt.Print(T("Codex 文件: %d\nClaude 文件: %d\n新增导入: %d\n跳过: %d\n排除: %d\n错误: %d\n",
		rep.CodexFiles, rep.ClaudeFiles, rep.Imported, rep.Skipped, rep.Excluded, rep.Errors))
	for _, e := range rep.ErrorSamples {
		fmt.Println("  error:", e)
	}
	return 0
}

func tasksMain(args []string) int {
	fset := flag.NewFlagSet("tasks", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	all := fset.Bool("all", false, "include finished tasks")
	_ = fset.Parse(args)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
		return 1
	}
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
		return 1
	}
	defer st.Close()
	var statuses []string
	if !*all {
		statuses = []string{store.StatusRunning, store.StatusWaitingInput, store.StatusPending}
	}
	list, _ := st.ListTasks(statuses, 100)
	if len(list) == 0 {
		fmt.Println(T("没有任务"))
		return 0
	}
	fmt.Printf("%-14s %-10s %-9s %-8s %-20s %s\n", "ID", "STATUS", "AGENT", "PID", "CREATED", "PROMPT")
	for _, t := range list {
		fmt.Printf("%-14s %-10s %-9s %-8d %-20s %s\n", t.ID, t.Status, t.Agent, t.PID,
			t.CreatedAt, store.TruncateTitle(t.Prompt, 60))
	}
	return 0
}

func cancelMain(args []string) int {
	fset := flag.NewFlagSet("cancel", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	_ = fset.Parse(args)
	rest := fset.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, T("用法: rymesh cancel <task-id>"))
		return 2
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
		return 1
	}
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
		return 1
	}
	defer st.Close()
	t, err := st.GetTask(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, T("任务不存在"))
		return 1
	}
	switch t.Status {
	case store.StatusCompleted, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted:
		fmt.Println(T("任务已结束:"), t.Status)
		return 0
	case store.StatusPending:
		_ = st.FinishTask(t.ID, store.StatusCancelled, nil, "用户取消（尚未启动）", util.NowRFC3339())
		fmt.Println(T("已取消"))
		return 0
	}
	_ = st.RequestCancel(t.ID)
	killTask(t, syscall.SIGTERM)
	fmt.Println(T("已发送停止信号；状态会由后台写入"))
	return 0
}

func logsMain(args []string) int {
	fset := flag.NewFlagSet("logs", flag.ContinueOnError)
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	n := fset.Int("n", 100, "number of lines")
	_ = fset.Parse(args)
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
		return 1
	}
	path := filepath.Join(cfg.LogDir, "rymesh.log")
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, Tr(err.Error()))
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
	configPath := fset.String("config", config.DefaultConfigPath, "config path")
	_ = fset.Parse(args)
	ok := true
	fmt.Println(T("== 配置 =="))
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Println("✗", err)
		return 1
	}
	fmt.Println("✓", *configPath)
	fmt.Println(T("== 目录 =="))
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
		fmt.Println(T("✗ 网卡 %s 不存在", cfg.EasyTierIface))
		ok = false
	}
	fmt.Println(T("== 监听地址 =="))
	addrs, err := listenAddresses(cfg)
	if err != nil {
		fmt.Println("✗", err)
		ok = false
	}
	for _, a := range addrs {
		fmt.Println("  ", a)
	}
	fmt.Println("== Agent ==")
	for _, a := range agent.Agents.List(true) {
		mark := "✓"
		if !a.Available {
			mark = "✗"
			ok = false
		}
		fmt.Printf("%s %-7s %s %s\n", mark, a.Name, a.Version, a.AuthDetail)
	}
	fmt.Println("== systemd ==")
	if out, err := runSystemctl("is-active", "rymesh"); err == nil {
		fmt.Println("✓", out)
	} else {
		fmt.Println("!", out)
	}
	fmt.Println(T("== 数据库 =="))
	if st, err := store.OpenStore(cfg.DataDir); err == nil {
		st.Close()
		fmt.Println("✓", filepath.Join(cfg.DataDir, "rymesh.db"))
	} else {
		fmt.Println("✗", err)
		ok = false
	}
	if ok {
		fmt.Println()
		fmt.Println(T("所有关键检查通过"))
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
	return agent.RunTaskMain(*dir)
}
