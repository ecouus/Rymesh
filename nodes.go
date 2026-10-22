package console

// Multi-device ("hub and nodes").
//
// Every instance is a node: it runs its own agents, keeps its own database and
// syncs its own native history, exactly as a single install does. An instance
// whose config lists other nodes is also a hub. The page reaches a remote node
// through /n/{node}/api/..., which the hub relays over EasyTier with that
// node's token. Agents therefore always run on the machine they belong to and
// keep running if the hub, the network or the browser goes away; the hub only
// forwards HTTP and SSE.
//
// Setup, per remote machine:
//
//	node$ rymesh node token          # prints (and creates) its token
//	hub$  rymesh node add mac --name Mac --url http://10.0.0.2:8899 --token <token>
//	hub$  systemctl restart rymesh

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// NodeConfig is one remote device. A direct device is reached at URL (a LAN
// or VPN address); an SSH device through an SSH connection to its loopback
// port (sshconn.go), with HostKey pinned on first contact.
type NodeConfig struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	Token      string `json:"token"`
	Transport  string `json:"transport,omitempty"` // "" (direct) or "ssh"
	SSHHost    string `json:"ssh_host,omitempty"`
	SSHPort    int    `json:"ssh_port,omitempty"`
	SSHUser    string `json:"ssh_user,omitempty"`
	KeyPath    string `json:"key_path,omitempty"` // a key file (~/.ssh)
	KeyID      string `json:"key_id,omitempty"`   // or a key in the vault (vault.go)
	UseAgent   bool   `json:"use_agent,omitempty"`
	HostKey    string `json:"host_key,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// localNodeID names this instance in the device list; it is never proxied.
const localNodeID = "local"

func (c *Config) nodeName() string {
	if c.NodeName != "" {
		return c.NodeName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "本机"
}

func (c *Config) findNode(id string) (NodeConfig, bool) {
	for _, n := range c.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return NodeConfig{}, false
}

// nodeTokenOK reports whether r carries this node's hub token.
func (s *Server) nodeTokenOK(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return false
	}
	match := func(tok string) bool {
		return tok != "" && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
	}
	return match(s.config().NodeToken) || match(s.reloadNodeToken())
}

// reloadNodeToken picks up a token that `rymesh node token` wrote to
// config.json while this console runs: a hub adopting a Mac app's console
// gets the token over SSH and uses it at once, without a restart. The file is
// read at most once a second, however many requests carry a wrong token.
func (s *Server) reloadNodeToken() string {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if time.Since(s.tokenRead) < time.Second {
		return ""
	}
	s.tokenRead = time.Now()
	tok := s.fileNodeToken()
	if tok == "" || tok == s.config().NodeToken {
		return ""
	}
	s.updateConfig(func(c *Config) { c.NodeToken = tok })
	s.logger.Printf("node token reloaded from %s", s.cfgPath)
	return tok
}

// fileNodeToken is the node token in config.json as it is on disk now.
func (s *Server) fileNodeToken() string {
	if s.cfgPath == "" {
		return ""
	}
	b, err := os.ReadFile(s.cfgPath)
	if err != nil {
		return ""
	}
	var f struct {
		NodeToken string `json:"node_token"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	return f.NodeToken
}

var nodeTransport = &http.Transport{
	Proxy:                 nil, // EasyTier addresses are direct; never route via an env proxy
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: 90 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   8,
}

type nodeView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Local     bool   `json:"local"`
	Online    bool   `json:"online"`
	Version   string `json:"version,omitempty"`
	URL       string `json:"url,omitempty"`
	Transport string `json:"transport,omitempty"`
	Target    string `json:"target,omitempty"` // user@host for SSH devices
	Error     string `json:"error,omitempty"`
	Checking  bool   `json:"checking,omitempty"` // not probed yet (fast listing)
}

// transportFor picks how the relay reaches a device.
func (s *Server) transportFor(n NodeConfig) http.RoundTripper {
	if n.Transport == transportSSH {
		return s.ssh.transport(n)
	}
	return nodeTransport
}

// handleNodes lists this machine and every configured node with a live check
// that the node is reachable and accepts the token. With ?fast=1 it answers
// at once from the last check instead: a first SSH connection to a distant
// server takes seconds, and the page should not sit blank meanwhile.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	off := 1
	if cfg.HubOnly {
		off = 0
	}
	out := make([]nodeView, off+len(cfg.Nodes))
	if !cfg.HubOnly {
		out[0] = nodeView{ID: localNodeID, Name: cfg.nodeName(), Local: true, Online: true, Version: version}
	}
	fast := r.URL.Query().Get("fast") == "1"
	var wg sync.WaitGroup
	for i, n := range cfg.Nodes {
		if fast {
			out[i+off] = s.lastProbe(n)
			continue
		}
		wg.Add(1)
		go func(i int, n NodeConfig) {
			defer wg.Done()
			out[i+off] = s.probe(r.Context(), n)
		}(i, n)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"nodes": out, "hub_only": cfg.HubOnly, "current": s.store.GetMeta("current_node")})
}

// probe checks a device and remembers the result for fast listings.
func (s *Server) probe(ctx context.Context, n NodeConfig) nodeView {
	v := probeNode(ctx, n, s.transportFor(n))
	s.nodeMu.Lock()
	if s.nodeSeen == nil {
		s.nodeSeen = map[string]nodeView{}
	}
	s.nodeSeen[n.ID] = v
	s.nodeMu.Unlock()
	return v
}

func (s *Server) lastProbe(n NodeConfig) nodeView {
	s.nodeMu.Lock()
	v, ok := s.nodeSeen[n.ID]
	s.nodeMu.Unlock()
	if ok {
		v.Name = n.Name // may have been renamed since
		return v
	}
	v = nodeView{ID: n.ID, Name: n.Name, URL: n.URL, Transport: n.Transport, Checking: true}
	if n.Transport == transportSSH {
		v.URL, v.Target = "", n.SSHUser+"@"+n.SSHHost
	}
	return v
}

// handleUseNode records the device the user is working with (the page's own
// storage may not survive Android stopping the app) and lifts any reconnect
// backoff, so choosing it or pressing retry always makes a fresh attempt.
func (s *Server) handleUseNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != localNodeID {
		n, ok := s.device(id)
		if !ok {
			writeErr(w, 404, "设备不存在")
			return
		}
		if n.Transport == transportSSH {
			s.ssh.retry(n)
		}
	}
	if err := s.store.SetMeta("current_node", id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleNodeSnapshot keeps the page's last view of a device (its bootstrap,
// trimmed), so the next start can draw it before the SSH connection is up.
// It lives in the database: a WebView's localStorage is written lazily and
// is often lost when Android stops the app.
func (s *Server) handleNodeSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.device(id); !ok {
		writeErr(w, 404, "设备不存在")
		return
	}
	key := "snapshot." + id
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		v := s.store.GetMeta(key)
		if v == "" {
			v = "null"
		}
		_, _ = io.WriteString(w, v)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil || !json.Valid(b) {
		writeErr(w, 400, "快照无效")
		return
	}
	if err := s.store.SetMeta(key, string(b)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// warmNodes connects to every device in the background when the console
// starts, so the SSH handshakes overlap with the page loading.
func (s *Server) warmNodes() {
	for _, n := range s.config().Nodes {
		go s.probe(context.Background(), n)
	}
}

func probeNode(ctx context.Context, n NodeConfig, rt http.RoundTripper) nodeView {
	v := nodeView{ID: n.ID, Name: n.Name, URL: n.URL, Transport: n.Transport}
	if n.Transport == transportSSH {
		v.URL, v.Target = "", n.SSHUser+"@"+n.SSHHost
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(n.baseURL(), "/")+"/api/nodes/self", nil)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		v.Error = "无法连接"
		var mm *hostKeyMismatchError
		switch {
		case errors.As(err, &mm):
			v.Error = "主机密钥不一致"
		case errors.Is(err, errHostKeyUnknown):
			v.Error = "未信任主机密钥"
		case strings.Contains(err.Error(), "节点程序未在"):
			v.Error = "节点未运行"
		case strings.Contains(err.Error(), "unable to authenticate"):
			v.Error = "SSH 认证失败"
		}
		return v
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var self struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&self)
		v.Online, v.Version = true, self.Version
	case http.StatusUnauthorized:
		v.Error = "令牌无效"
	default:
		v.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return v
}

// handleNodeSelf identifies this node to a hub; it requires the node token
// (or a login), so a successful answer also proves the token is right.
func (s *Server) handleNodeSelf(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"name": s.config().nodeName(), "version": version})
}

// Login state belongs to the hub. These must never reach a node: the node
// would set its own cookie on the hub's origin.
var errNodeToken = errors.New("node rejected token")

var hubOnlyPaths = map[string]bool{
	"/api/login/ticket": true, "/api/session": true,
}

func (s *Server) handleNodeProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("node")
	n, ok := s.config().findNode(id)
	if !ok {
		writeErr(w, 404, "未知设备: "+id)
		return
	}
	path := "/api/" + r.PathValue("rest")
	if hubOnlyPaths[path] {
		writeErr(w, 403, "该操作只能在当前控制台执行")
		return
	}
	target, err := url.Parse(n.baseURL())
	if err != nil {
		writeErr(w, 500, "设备地址无效")
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			// The node authenticates the hub by token. The browser's cookie and
			// CSRF token are the hub's and mean nothing there.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-CSRF-Token")
			pr.Out.Header.Set("Authorization", "Bearer "+n.Token)
			// Let the hub's own gzip layer compress; a pre-compressed body
			// would otherwise be compressed twice.
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:     s.transportFor(n),
		FlushInterval: -1, // SSE: forward every event as soon as it arrives
		ModifyResponse: func(resp *http.Response) error {
			// A 401 from the node means the hub's token is wrong. Passed through,
			// the page would read it as its own login expiring and log out.
			if resp.StatusCode == http.StatusUnauthorized {
				return errNodeToken
			}
			resp.Header.Del("Set-Cookie")
			// The hub sets its own security headers; duplicates confuse nothing
			// but are noise.
			for _, h := range []string{"Content-Security-Policy", "X-Frame-Options",
				"X-Content-Type-Options", "Referrer-Policy", "Permissions-Policy"} {
				resp.Header.Del(h)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the browser went away (a closed stream)
			}
			s.logger.Printf("node %s: %v", id, err)
			if errors.Is(err, errNodeToken) {
				writeErr(w, http.StatusBadGateway, "设备「"+n.Name+"」拒绝了令牌，请检查 node_token")
				return
			}
			writeErr(w, http.StatusBadGateway, dialFailure(err))
		},
	}
	proxy.ServeHTTP(w, r)
}

// dialFailure says why a device could not be reached, in words that point at
// what to check.
func dialFailure(err error) string {
	msg := err.Error()
	var ne net.Error
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		return "SSH 认证失败：服务器不接受这把密钥"
	case strings.Contains(msg, "节点程序未在"):
		return msg
	case strings.Contains(msg, "connection refused"):
		return "连接被拒绝：服务器的 SSH 端口没有开放，或端口填错了"
	case strings.Contains(msg, "no such host"):
		return "找不到这个主机名，请检查地址"
	case strings.Contains(msg, "network is unreachable"), strings.Contains(msg, "no route to host"):
		return "网络不可达：请检查网络连接"
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(msg, "i/o timeout"):
		return "连接超时：服务器没有响应，请检查地址和网络"
	case strings.Contains(msg, "handshake failed: EOF"):
		return "服务器在 SSH 握手时断开：地址或端口可能不对，或被网络代理拦截"
	}
	return sshErrorText(err)
}

func cliTransport(n NodeConfig, keys keySource) http.RoundTripper {
	if n.Transport == transportSSH {
		return newSSHPool(keys).transport(n)
	}
	return nodeTransport
}

// ---------------------------------------------------------------------------
// CLI: rymesh node token|add|remove|list|name
// ---------------------------------------------------------------------------

func nodeMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `用法:
  rymesh node setup --token 令牌 [--port 8899] [--name 名称]  在本机安装/升级节点（~/.rymesh，仅监听 127.0.0.1）
  rymesh node token [--rotate]                       显示本机节点令牌（首次自动生成）
  rymesh node name <名称>                             设置本机在设备列表中的名称
  rymesh node add <id> --name 名称 --url 地址 --token 令牌  添加远程设备（在主控台上执行）
  rymesh node remove <id>                            移除远程设备
  rymesh node list                                   列出远程设备
所有子命令都接受 --config；修改后需重启服务生效。`)
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub == "setup" {
		return nodeSetupMain(rest) // runs on the node itself; no hub config
	}
	fset := flag.NewFlagSet("node "+sub, flag.ContinueOnError)
	configPath := fset.String("config", systemConfigPath(), "config path")
	rotate := fset.Bool("rotate", false, "generate a new token")
	name := fset.String("name", "", "display name")
	nodeURL := fset.String("url", "", "node base URL, e.g. http://10.0.0.2:8899")
	token := fset.String("token", "", "node token")
	// Positional arguments come first ("add mac --url ..."); flag parsing
	// stops at the first non-flag, so split them off before parsing.
	var pos []string
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	if err := fset.Parse(rest); err != nil {
		return 2
	}
	pos = append(pos, fset.Args()...)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return 1
	}
	// Devices live in the database (vault.go); token and name only touch
	// config.json, so they work while the service has the database open.
	var st *Store
	if sub == "add" || sub == "remove" || sub == "list" {
		if st, err = openStore(cfg.DataDir); err != nil {
			fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
			return 1
		}
		defer st.Close()
		if err := initVault(st, cfg, *configPath); err != nil {
			fmt.Fprintln(os.Stderr, "读取设备失败:", err)
			return 1
		}
	}
	save := func() int {
		var err error
		if st != nil {
			if err = st.saveDevices(cfg.Nodes); err == nil {
				err = saveConfigFile(*configPath, cfg)
			}
		} else {
			err = saveConfig(*configPath, cfg)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "保存配置失败:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "已保存；重启服务后生效。")
		return 0
	}
	switch sub {
	case "token":
		if cfg.NodeToken == "" || *rotate {
			b, err := randomBytes(32)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			cfg.NodeToken = base64.RawURLEncoding.EncodeToString(b)
			if code := save(); code != 0 {
				return code
			}
		}
		fmt.Println(cfg.NodeToken)
		return 0
	case "name":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "用法: rymesh node name <名称>")
			return 2
		}
		cfg.NodeName = pos[0]
		return save()
	case "add":
		if len(pos) != 1 || *nodeURL == "" || *token == "" {
			fmt.Fprintln(os.Stderr, "用法: rymesh node add <id> --name 名称 --url 地址 --token 令牌")
			return 2
		}
		id := pos[0]
		if !validID(id) || id == localNodeID {
			fmt.Fprintln(os.Stderr, "设备 id 只能包含字母、数字、- 和 _，且不能是 local")
			return 2
		}
		u, err := url.Parse(*nodeURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintln(os.Stderr, "地址必须形如 http://10.0.0.2:8899")
			return 2
		}
		n := NodeConfig{ID: id, Name: *name, URL: strings.TrimRight(*nodeURL, "/"), Token: *token}
		if n.Name == "" {
			n.Name = id
		}
		replaced := false
		for i := range cfg.Nodes {
			if cfg.Nodes[i].ID == id {
				cfg.Nodes[i], replaced = n, true
			}
		}
		if !replaced {
			cfg.Nodes = append(cfg.Nodes, n)
		}
		v := probeNode(context.Background(), n, cliTransport(n, st))
		if v.Online {
			fmt.Printf("设备 %s 在线（%s）\n", n.Name, v.Version)
		} else {
			fmt.Printf("警告：设备 %s 当前不可用：%s\n", n.Name, v.Error)
		}
		return save()
	case "remove":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "用法: rymesh node remove <id>")
			return 2
		}
		kept := cfg.Nodes[:0]
		for _, n := range cfg.Nodes {
			if n.ID != pos[0] {
				kept = append(kept, n)
			}
		}
		if len(kept) == len(cfg.Nodes) {
			fmt.Fprintln(os.Stderr, "没有这个设备:", pos[0])
			return 1
		}
		cfg.Nodes = kept
		return save()
	case "list":
		fmt.Printf("本机  %s\n", cfg.nodeName())
		for _, n := range cfg.Nodes {
			v := probeNode(context.Background(), n, cliTransport(n, st))
			state := "在线 " + v.Version
			if !v.Online {
				state = "不可用：" + v.Error
			}
			fmt.Printf("%-10s %-12s %-28s %s\n", n.ID, n.Name, n.URL, state)
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "未知子命令:", sub)
	return 2
}
