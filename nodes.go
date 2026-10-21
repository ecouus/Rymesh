package main

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
//	node$ agentconsole node token          # prints (and creates) its token
//	hub$  agentconsole node add mac --name Mac --url http://10.0.0.2:8899 --token <token>
//	hub$  systemctl restart agentconsole

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	KeyPath    string `json:"key_path,omitempty"`
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
	tok := s.config().NodeToken
	if tok == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
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
}

// transportFor picks how the relay reaches a device.
func (s *Server) transportFor(n NodeConfig) http.RoundTripper {
	if n.Transport == transportSSH {
		return s.ssh.transport(n)
	}
	return nodeTransport
}

// handleNodes lists this machine and every configured node with a live check
// that the node is reachable and accepts the token.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	out := make([]nodeView, 1+len(cfg.Nodes))
	out[0] = nodeView{ID: localNodeID, Name: cfg.nodeName(), Local: true, Online: true, Version: version}
	var wg sync.WaitGroup
	for i, n := range cfg.Nodes {
		wg.Add(1)
		go func(i int, n NodeConfig) {
			defer wg.Done()
			out[i+1] = probeNode(r.Context(), n, s.transportFor(n))
		}(i, n)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"nodes": out})
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
// would set its own cookie on the hub's origin or change its own password.
var errNodeToken = errors.New("node rejected token")

var hubOnlyPaths = map[string]bool{
	"/api/login": true, "/api/logout": true, "/api/password": true, "/api/session": true,
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
			writeErr(w, http.StatusBadGateway, "设备「"+n.Name+"」无法连接")
		},
	}
	proxy.ServeHTTP(w, r)
}

func cliTransport(n NodeConfig) http.RoundTripper {
	if n.Transport == transportSSH {
		return newSSHPool().transport(n)
	}
	return nodeTransport
}

// ---------------------------------------------------------------------------
// CLI: agentconsole node token|add|remove|list|name
// ---------------------------------------------------------------------------

func nodeMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `用法:
  agentconsole node setup --token 令牌 [--port 8899] [--name 名称]  在本机安装/升级节点（~/.agentconsole，仅监听 127.0.0.1）
  agentconsole node token [--rotate]                       显示本机节点令牌（首次自动生成）
  agentconsole node name <名称>                             设置本机在设备列表中的名称
  agentconsole node add <id> --name 名称 --url 地址 --token 令牌  添加远程设备（在主控台上执行）
  agentconsole node remove <id>                            移除远程设备
  agentconsole node list                                   列出远程设备
所有子命令都接受 --config；修改后需重启服务生效。`)
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub == "setup" {
		return nodeSetupMain(rest) // runs on the node itself; no hub config
	}
	fset := flag.NewFlagSet("node "+sub, flag.ContinueOnError)
	configPath := fset.String("config", defaultConfigPath, "config path")
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
	save := func() int {
		if err := saveConfig(*configPath, cfg); err != nil {
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
			fmt.Fprintln(os.Stderr, "用法: agentconsole node name <名称>")
			return 2
		}
		cfg.NodeName = pos[0]
		return save()
	case "add":
		if len(pos) != 1 || *nodeURL == "" || *token == "" {
			fmt.Fprintln(os.Stderr, "用法: agentconsole node add <id> --name 名称 --url 地址 --token 令牌")
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
		v := probeNode(context.Background(), n, cliTransport(n))
		if v.Online {
			fmt.Printf("设备 %s 在线（%s）\n", n.Name, v.Version)
		} else {
			fmt.Printf("警告：设备 %s 当前不可用：%s\n", n.Name, v.Error)
		}
		return save()
	case "remove":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "用法: agentconsole node remove <id>")
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
			v := probeNode(context.Background(), n, cliTransport(n))
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
