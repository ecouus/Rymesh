package console

// `agentconsole client` is the entry point on a personal machine: the console
// runs locally (loopback only), is both this machine's node and the hub for
// the user's servers, and opens in the browser without a password through a
// one-time login ticket.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const ticketFile = "login-ticket"

func clientMain(args []string) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	base, err := agentHome()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fset := flag.NewFlagSet("client", flag.ContinueOnError)
	configPath := fset.String("config", filepath.Join(base, "config.json"), "config path")
	port := fset.Int("port", defaultPort, "loopback port for a new config")
	noOpen := fset.Bool("no-open", false, "do not open the browser")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if os.IsNotExist(err) {
		if cfg, err = personalConfig(filepath.Dir(*configPath), *port); err == nil {
			if err = cfg.ensureDirs(); err == nil {
				err = saveConfig(*configPath, cfg)
			}
		}
		if err == nil {
			fmt.Println("已创建配置：", *configPath)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置无效：", err)
		return 1
	}
	switch sub {
	case "serve":
		// What the background service runs.
		return serveMain([]string{"--config", *configPath})
	case "open":
		return openConsole(cfg)
	case "", "start":
		// A service left running by an older build (the app was updated, or
		// the binary moved) is restarted onto this one.
		exe, _ := os.Executable()
		exe, _ = filepath.EvalSymlinks(exe)
		if running := runningBuild(cfg.Port); running != "" && (running != buildID() || serviceUsesOther(exe)) {
			fmt.Println("后台服务不是当前这个程序，正在切换…")
			startNodeService(exe, *configPath, filepath.Dir(*configPath))
			time.Sleep(time.Second)
		}
		if !waitHealthy(cfg.Port, 500*time.Millisecond) {
			service, warning := startNodeService(exe, *configPath, filepath.Dir(*configPath))
			if warning != "" {
				fmt.Fprintln(os.Stderr, "注意：", warning)
			}
			if !waitHealthy(cfg.Port, 15*time.Second) {
				fmt.Fprintf(os.Stderr, "控制台没有在 127.0.0.1:%d 启动，日志在 %s\n", cfg.Port, cfg.LogDir)
				return 1
			}
			fmt.Printf("控制台已在后台运行（%s），开机自动启动。\n", service)
		}
		if *noOpen {
			fmt.Printf("http://127.0.0.1:%d\n", cfg.Port)
			return 0
		}
		return openConsole(cfg)
	}
	fmt.Fprintln(os.Stderr, `用法:
  agentconsole client          启动（首次自动配置并注册为后台服务）并在浏览器中打开
  agentconsole client open     在浏览器中打开（免密）
  agentconsole client serve    前台运行（供服务管理器调用）`)
	return 2
}

var (
	buildOnce sync.Once
	buildHash string
)

// buildID fingerprints this executable, so a client can tell whether the
// service it finds running is the same build as itself.
func buildID() string {
	buildOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			buildHash = hex.EncodeToString(h.Sum(nil))[:12]
		}
	})
	return buildHash
}

// serviceUsesOther reports whether the registered service starts a different
// program than exe — the app was moved, or another copy installed it.
func serviceUsesOther(exe string) bool {
	home, _ := os.UserHomeDir()
	var file, want string
	switch runtime.GOOS {
	case "darwin":
		file = filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		want = "<string>" + xmlEsc(exe) + "</string>"
	case "linux":
		file = filepath.Join(home, ".config", "systemd", "user", nodeServiceName+".service")
		if os.Geteuid() == 0 {
			file = "/etc/systemd/system/" + nodeServiceName + ".service"
		}
		want = "ExecStart=" + exe + " "
	}
	b, err := os.ReadFile(file)
	return err == nil && !strings.Contains(string(b), want)
}

// runningBuild reports the build of the service on port, "" if none answers.
func runningBuild(port int) string {
	c := &http.Client{Timeout: 2 * time.Second, Transport: nodeTransport}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var h struct {
		Build string `json:"build"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Build == "" {
		return "unknown" // older than build fingerprints: always replace
	}
	return h.Build
}

// openConsole opens the page with the current login ticket.
func openConsole(cfg *Config) int {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, ticketFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, "控制台未运行（没有登录凭证），请先运行 agentconsole client")
		return 1
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/login/ticket?t=%s", cfg.Port, strings.TrimSpace(string(b)))
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Run(); err != nil {
		fmt.Println("请在浏览器中打开：", url)
	}
	return 0
}

// rotateTicket writes a fresh single-use login ticket that only this user can
// read; the CLI hands it to the browser so no password is needed locally.
func (s *Server) rotateTicket() {
	b, err := randomBytes(24)
	if err != nil {
		return
	}
	t := fmt.Sprintf("%x", b)
	s.ticketMu.Lock()
	s.ticket = t
	s.ticketMu.Unlock()
	_ = os.WriteFile(filepath.Join(s.config().DataDir, ticketFile), []byte(t+"\n"), 0o600)
}

func (s *Server) handleTicketLogin(w http.ResponseWriter, r *http.Request) {
	got := r.URL.Query().Get("t")
	s.ticketMu.Lock()
	want := s.ticket
	s.ticketMu.Unlock()
	if want == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.rotateTicket() // single use
	s.auth.setCookie(w)
	http.Redirect(w, r, "/", http.StatusFound)
}
