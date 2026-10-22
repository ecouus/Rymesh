package console

// Device management on the client (hub): add a server by SSH, confirm its host
// key, deploy or upgrade the node on it, and manage keys. These endpoints act
// on this console's own config and are never relayed to a node.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// deviceView is a NodeConfig as the page sees it: never the token.
type deviceView struct {
	NodeConfig
	Token    string `json:"token,omitempty"`
	HasToken bool   `json:"has_token"`
	Trusted  bool   `json:"trusted"`
	Fprint   string `json:"fingerprint,omitempty"`
}

func viewDevice(n NodeConfig) deviceView {
	v := deviceView{NodeConfig: n, HasToken: n.Token != "", Trusted: n.HostKey != ""}
	v.NodeConfig.Token = ""
	if n.HostKey != "" {
		v.Fprint = fingerprintOf(n.HostKey)
	}
	return v
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	out := []deviceView{}
	for _, n := range s.config().Nodes {
		out = append(out, viewDevice(n))
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

type deviceInput struct {
	Name       *string `json:"name"`
	Transport  *string `json:"transport"`
	SSHHost    *string `json:"ssh_host"`
	SSHPort    *int    `json:"ssh_port"`
	SSHUser    *string `json:"ssh_user"`
	KeyPath    *string `json:"key_path"`
	KeyID      *string `json:"key_id"`
	UseAgent   *bool   `json:"use_agent"`
	RemotePort *int    `json:"remote_port"`
	URL        *string `json:"url"`
	Token      *string `json:"token"`
}

// apply merges the input into n; changing where or how to connect forgets the
// pinned host key, which must then be confirmed again.
func (in deviceInput) apply(n *NodeConfig) error {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	oldTarget := n.sshAddr()
	set(&n.Name, in.Name)
	set(&n.Transport, in.Transport)
	set(&n.SSHHost, in.SSHHost)
	set(&n.SSHUser, in.SSHUser)
	set(&n.KeyPath, in.KeyPath)
	set(&n.KeyID, in.KeyID)
	set(&n.URL, in.URL)
	set(&n.Token, in.Token)
	if in.SSHPort != nil {
		n.SSHPort = *in.SSHPort
	}
	if in.UseAgent != nil {
		n.UseAgent = *in.UseAgent
	}
	if in.RemotePort != nil {
		n.RemotePort = *in.RemotePort
	}
	if n.Name == "" {
		n.Name = n.SSHHost
	}
	switch n.Transport {
	case transportSSH:
		if n.SSHHost == "" || n.SSHUser == "" {
			return errors.New("需要主机和用户名")
		}
		if n.KeyPath == "" && n.KeyID == "" && !n.UseAgent {
			return errors.New("请选择密钥或使用 ssh-agent")
		}
		if n.SSHPort < 0 || n.SSHPort > 65535 || n.RemotePort < 0 || n.RemotePort > 65535 {
			return errors.New("端口无效")
		}
		if n.sshAddr() != oldTarget {
			n.HostKey = ""
		}
	case "", "direct":
		n.Transport = ""
		if n.URL == "" || n.Token == "" {
			return errors.New("直连设备需要地址和令牌")
		}
	default:
		return errors.New("未知的连接方式: " + n.Transport)
	}
	return nil
}

// saveNodes publishes and persists a change to the device list.
func (s *Server) saveNodes(fn func(nodes []NodeConfig) ([]NodeConfig, error)) error {
	next := s.config().clone()
	nodes, err := fn(next.Nodes)
	if err != nil {
		return err
	}
	next.Nodes = nodes
	if err := s.store.saveDevices(nodes); err != nil {
		return fmt.Errorf("保存设备失败：%w", err)
	}
	s.publishConfig(next)
	return nil
}

func (s *Server) device(id string) (NodeConfig, bool) { return s.config().findNode(id) }

func (s *Server) updateDevice(id string, fn func(*NodeConfig)) (NodeConfig, error) {
	var out NodeConfig
	err := s.saveNodes(func(nodes []NodeConfig) ([]NodeConfig, error) {
		for i := range nodes {
			if nodes[i].ID == id {
				fn(&nodes[i])
				out = nodes[i]
				return nodes, nil
			}
		}
		return nil, errors.New("设备不存在")
	})
	return out, err
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var in deviceInput
	if err := decodeJSON(w, r, &in); err != nil {
		return
	}
	n := NodeConfig{ID: "d" + strings.ToLower(randomID(7)), Transport: transportSSH}
	if err := in.apply(&n); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.saveNodes(func(nodes []NodeConfig) ([]NodeConfig, error) {
		return append(nodes, n), nil
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"device": viewDevice(n)})
}

func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	var in deviceInput
	if err := decodeJSON(w, r, &in); err != nil {
		return
	}
	var applyErr error
	n, err := s.updateDevice(r.PathValue("id"), func(n *NodeConfig) { applyErr = in.apply(n) })
	if applyErr != nil {
		writeErr(w, 400, applyErr.Error())
		return
	}
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"device": viewDevice(n)})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.saveNodes(func(nodes []NodeConfig) ([]NodeConfig, error) {
		kept := nodes[:0]
		for _, n := range nodes {
			if n.ID != id {
				kept = append(kept, n)
			}
		}
		if len(kept) == len(nodes) {
			return nil, errors.New("设备不存在")
		}
		return kept, nil
	})
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleDeviceTest walks the add-device flow one step at a time:
//
//	stage "hostkey" — first contact: show the fingerprint for confirmation
//	stage "auth"    — the server refused the key: offer to install it
//	stage "ready"   — connected; reports platform and any installed node
//	stage "error"   — with a message the page shows as is
func (s *Server) handleDeviceTest(w http.ResponseWriter, r *http.Request) {
	n, ok := s.device(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "设备不存在")
		return
	}
	if n.Transport != transportSSH {
		v := probeNode(r.Context(), n, s.transportFor(n))
		writeJSON(w, 200, map[string]any{"stage": "ready", "node": v})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if n.HostKey == "" {
		p, err := probeHostKey(ctx, n)
		if err != nil {
			writeJSON(w, 200, map[string]any{"stage": "error", "error": "无法连接 " + n.sshAddr() + "：" + err.Error()})
			return
		}
		if !p.Known {
			writeJSON(w, 200, map[string]any{"stage": "hostkey", "hostkey": p})
			return
		}
		// Already in ~/.ssh/known_hosts: the user trusted it before.
		if n, err = s.updateDevice(n.ID, func(d *NodeConfig) { d.HostKey = p.Key }); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	s.ssh.retry(n) // an explicit test always tries again
	out, err := s.ssh.run(ctx, n, `uname -s -m; B="$HOME/.rymesh/bin/rymesh"; [ -x "$B" ] || B="$HOME/.agentconsole/bin/agentconsole"; [ -x "$B" ] && "$B" version`, nil)
	if err != nil && out == "" {
		if strings.Contains(err.Error(), "unable to authenticate") {
			res := map[string]any{"stage": "auth", "error": "服务器还不认识这把密钥"}
			if pub, err := s.publicKeyOf(n); err == nil {
				res["public"] = pub
			}
			writeJSON(w, 200, res)
			return
		}
		writeJSON(w, 200, map[string]any{"stage": "error", "error": sshErrorText(err)})
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	goos, goarch := parseUname(lines[0])
	installed := ""
	if len(lines) > 1 {
		installed = strings.TrimSpace(lines[len(lines)-1])
	}
	res := map[string]any{"stage": "ready", "platform": goos + "/" + goarch, "installed": installed,
		"device": viewDevice(n)}
	if installed != "" && n.Token != "" {
		res["node"] = probeNode(ctx, n, s.transportFor(n))
	}
	writeJSON(w, 200, res)
}

// handleDeviceTrust pins the host key the user confirmed. The key is fetched
// again and must still match, so a key swapped in between is not accepted.
func (s *Server) handleDeviceTrust(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	n, ok := s.device(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "设备不存在")
		return
	}
	p, err := probeHostKey(r.Context(), n)
	if err != nil {
		writeErr(w, 502, "无法连接："+err.Error())
		return
	}
	if strings.TrimSpace(req.Key) != p.Key {
		writeErr(w, 409, "服务器的主机密钥已变化，请重新测试连接")
		return
	}
	n, err = s.updateDevice(n.ID, func(d *NodeConfig) { d.HostKey = p.Key })
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"device": viewDevice(n)})
}

// handleDeviceDeploy installs or upgrades the node, streaming progress as SSE.
func (s *Server) handleDeviceDeploy(w http.ResponseWriter, r *http.Request) {
	n, ok := s.device(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "设备不存在")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(200)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	updated, err := s.deployNode(ctx, n, func(msg string) { send("progress", map[string]any{"text": msg}) })
	if err != nil {
		send("failed", map[string]any{"error": err.Error()})
		return
	}
	send("done", map[string]any{"device": viewDevice(updated)})
}

// deployNode uploads the node binary for the device's platform and runs
// `node setup` there. An existing install keeps its token, so every client
// that manages the server (a Mac and a phone) can share it.
func (s *Server) deployNode(ctx context.Context, n NodeConfig, emit func(string)) (NodeConfig, error) {
	if n.Transport != transportSSH {
		return n, errors.New("只有 SSH 设备可以自动部署")
	}
	if n.HostKey == "" {
		return n, errors.New("请先测试连接并确认主机指纹")
	}
	emit("连接 " + n.SSHUser + "@" + n.sshAddr())
	out, err := s.ssh.run(ctx, n, "uname -s -m", nil)
	if err != nil {
		return n, errors.New(sshErrorText(err))
	}
	goos, goarch := parseUname(out)
	if goos == "darwin" {
		if n, ok, err := s.adoptClient(ctx, n, emit); ok {
			return n, err
		}
	}
	bin, src, err := nodeBinary(goos, goarch, s.config().DataDir)
	if err != nil {
		return n, err
	}
	emit(fmt.Sprintf("上传程序：%s/%s，%.1f MB（%s）", goos, goarch, float64(len(bin))/(1<<20), src))
	const up = `set -e; D="$HOME/.rymesh/bin"; mkdir -p "$D"; cat > "$D/rymesh.new"; ` +
		`chmod 755 "$D/rymesh.new"; mv -f "$D/rymesh.new" "$D/rymesh"; "$D/rymesh" version`
	if out, err := s.ssh.run(ctx, n, up, bin); err != nil {
		return n, fmt.Errorf("上传失败：%s", strings.TrimSpace(out+" "+err.Error()))
	}
	if out, _ := s.ssh.run(ctx, n, `[ -f /etc/rymesh/config.json ] && echo system`, nil); strings.TrimSpace(out) == "system" {
		return s.adoptSystemNode(ctx, n, emit)
	}
	if out, _ := s.ssh.run(ctx, n, `[ -f /etc/agentconsole/config.json ] && echo legacy`, nil); strings.TrimSpace(out) == "legacy" {
		emit("检测到旧版系统安装，备份并迁移服务及数据")
		const migrate = `set -e
[ "$(id -u)" = 0 ] || { echo '迁移系统级安装需要 root'; exit 1; }
BACKUP="/var/backups/rymesh-migration-$(date +%Y%m%d-%H%M%S)"
mkdir -m 700 "$BACKUP"
systemctl stop agentconsole
trap 'systemctl start agentconsole >/dev/null 2>&1 || true' EXIT
set --
for P in /etc/agentconsole /var/lib/agentconsole /var/log/agentconsole /etc/systemd/system/agentconsole.service; do
  [ ! -e "$P" ] || set -- "$@" "$P"
done
tar -czf "$BACKUP/legacy.tar.gz" "$@"
install -m 0755 "$HOME/.rymesh/bin/rymesh" /usr/local/bin/rymesh
/usr/local/bin/rymesh install
trap - EXIT`
		if out, err := s.ssh.run(ctx, n, migrate, nil); err != nil {
			return n, fmt.Errorf("迁移旧版系统安装失败：%s", strings.TrimSpace(out))
		}
		return s.adoptSystemNode(ctx, n, emit)
	}
	token := ""
	if out, err := s.ssh.run(ctx, n, `C="$HOME/.rymesh/config.json"; [ -f "$C" ] || C="$HOME/.agentconsole/config.json"; [ -f "$C" ] && "$HOME/.rymesh/bin/rymesh" node token --config "$C" 2>/dev/null`, nil); err == nil {
		token = strings.TrimSpace(out)
	}
	if token != "" {
		emit("检测到已安装的节点，沿用它的令牌并升级")
	} else if token, err = newNodeToken(); err != nil {
		return n, err
	}
	port := n.RemotePort
	if port == 0 {
		port = defaultPort
	}
	emit("安装并启动节点服务")
	cmd := fmt.Sprintf(`"$HOME/.rymesh/bin/rymesh" node setup --token %s --port %d --name %s`,
		shellQuote(token), port, shellQuote(n.Name))
	out, err = s.ssh.run(ctx, n, loginShell(cmd), nil)
	if err != nil {
		return n, fmt.Errorf("节点安装失败：%s", strings.TrimSpace(out))
	}
	var res setupResult
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			_ = json.Unmarshal([]byte(line), &res)
		}
	}
	if !res.OK {
		return n, fmt.Errorf("节点安装失败：%s", strings.TrimSpace(out))
	}
	emit(fmt.Sprintf("节点已运行（%s，端口 %d）", res.Service, res.Port))
	if res.Warning != "" {
		emit("注意：" + res.Warning)
	}
	n, err = s.updateDevice(n.ID, func(d *NodeConfig) { d.Token, d.RemotePort = token, res.Port })
	if err != nil {
		return n, err
	}
	emit("验证连接")
	for i := 0; i < 10; i++ {
		if v := probeNode(ctx, n, s.transportFor(n)); v.Online {
			emit("完成：节点版本 " + v.Version)
			return n, nil
		}
		time.Sleep(time.Second)
	}
	return n, errors.New("节点已安装，但通过 SSH 隧道访问失败")
}

// adoptClient joins the console a Mac already runs for its own user (the Mac
// app, or `rymesh client`) instead of installing a second node next to it,
// which would fight it for ~/.rymesh and the port. Nothing is uploaded or
// installed: that console's own program hands out a node token, which the
// running console picks up without a restart (nodes.go reloadNodeToken).
// ok is false when the machine has no such console.
func (s *Server) adoptClient(ctx context.Context, n NodeConfig, emit func(string)) (NodeConfig, bool, error) {
	const find = `C="$HOME/.rymesh/config.json"
B=$(cat "$HOME/.rymesh/` + clientBinFile + `" 2>/dev/null)
[ -x "$B" ] || B=/Applications/Rymesh.app/Contents/Resources/rymesh
[ -f "$C" ] && [ -x "$B" ] || exit 0
"$B" node token --config "$C" 2>/dev/null || exit 1
sed -n 's/.*"port": *\([0-9]*\).*/\1/p' "$C" | head -1`
	out, err := s.ssh.run(ctx, n, find, nil)
	f := strings.Fields(out)
	if err == nil && len(f) == 0 {
		return n, false, nil
	}
	if err != nil || len(f) != 2 {
		return n, true, fmt.Errorf("读取 Mac 上 Rymesh 的节点令牌失败：%s", strings.TrimSpace(out))
	}
	port, _ := strconv.Atoi(f[1])
	if port == 0 {
		port = defaultPort
	}
	emit("检测到 Mac 上的 Rymesh，直接接入它（不安装任何程序）")
	n, err = s.updateDevice(n.ID, func(d *NodeConfig) { d.Token, d.RemotePort = f[0], port })
	if err != nil {
		return n, true, err
	}
	emit("验证连接")
	for i := 0; i < 5; i++ {
		if v := probeNode(ctx, n, s.transportFor(n)); v.Online {
			emit("完成：Mac 上的 Rymesh 版本 " + v.Version)
			return n, true, nil
		}
		time.Sleep(time.Second)
	}
	return n, true, errors.New("已取得令牌，但连不上 Mac 上的 Rymesh：请确认 Mac 上的 Rymesh 应用正在运行")
}

// adoptSystemNode takes over a server-wide install (`rymesh install`:
// /etc/rymesh, a systemd unit) instead of starting a second node next to
// it, which would fight it for the port. It keeps that install's config and
// port, adds a node token, and upgrades its binary in place (the previous one
// is kept as <binary>.prev). Running agent tasks survive the restart.
func (s *Server) adoptSystemNode(ctx context.Context, n NodeConfig, emit func(string)) (NodeConfig, error) {
	const cfg = "/etc/rymesh/config.json"
	emit("检测到服务器上的系统级安装（" + cfg + "），接管并升级它")
	out, err := s.ssh.run(ctx, n, `"$HOME/.rymesh/bin/rymesh" node token --config `+cfg+` 2>/dev/null`, nil)
	token := strings.TrimSpace(out)
	if err != nil || token == "" {
		return n, fmt.Errorf("读取节点令牌失败（需要能写 %s）：%s", cfg, strings.TrimSpace(out))
	}
	out, _ = s.ssh.run(ctx, n, `sed -n 's/.*"port": *\([0-9]*\).*/\1/p' `+cfg+` | head -1`, nil)
	port, _ := strconv.Atoi(strings.TrimSpace(out))
	if port == 0 {
		port = defaultPort
	}
	emit("升级系统服务的程序并重启（运行中的任务不受影响）")
	const swap = `set -e
BIN=$(systemctl show -p ExecStart --value rymesh | sed -n 's/.*path=\([^ ;]*\).*/\1/p')
[ -n "$BIN" ] || BIN=/usr/local/bin/rymesh
cp -p "$BIN" "$BIN.prev" 2>/dev/null || true
install -m 0755 "$HOME/.rymesh/bin/rymesh" "$BIN"
systemctl restart rymesh
echo "$BIN"`
	if out, err := s.ssh.run(ctx, n, swap, nil); err != nil {
		return n, fmt.Errorf("升级系统服务失败（需要 root）：%s", strings.TrimSpace(out))
	} else {
		emit("已升级 " + strings.TrimSpace(out) + "（旧程序保存为 .prev）")
	}
	n, err = s.updateDevice(n.ID, func(d *NodeConfig) { d.Token, d.RemotePort = token, port })
	if err != nil {
		return n, err
	}
	emit("验证连接")
	for i := 0; i < 15; i++ {
		if v := probeNode(ctx, n, s.transportFor(n)); v.Online {
			emit("完成：节点版本 " + v.Version)
			return n, nil
		}
		time.Sleep(time.Second)
	}
	return n, errors.New("系统服务已升级，但通过 SSH 隧道访问失败")
}

func newNodeToken() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// nodeBinary finds the node program for a platform: this binary when the
// platform matches, else a `make dist` build next to it or in the data dir.
func nodeBinary(goos, goarch, dataDir string) ([]byte, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		b, err := os.ReadFile(exe)
		return b, "本程序", err
	}
	name := "rymesh_" + goos + "_" + goarch
	wd, _ := os.Getwd()
	for _, dir := range []string{filepath.Join(filepath.Dir(exe), "dist"), filepath.Join(filepath.Dir(exe), "..", "dist"),
		filepath.Join(wd, "dist"), filepath.Join(dataDir, "dist")} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return b, filepath.Join(dir, name), nil
		}
	}
	return nil, "", fmt.Errorf("没有 %s/%s 平台的程序：请在源码目录运行 make dist，或把 %s 放到 %s",
		goos, goarch, name, filepath.Join(dataDir, "dist"))
}

func parseUname(s string) (goos, goarch string) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) < 2 {
		return "unknown", "unknown"
	}
	goos = strings.ToLower(f[0])
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		goarch = f[1]
	}
	return goos, goarch
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// loginShell runs cmd in the user's login shell, so PATH includes where they
// installed codex and claude (nvm, ~/.local/bin, Homebrew...).
func loginShell(cmd string) string {
	return `exec "${SHELL:-/bin/sh}" -lc ` + shellQuote(cmd)
}

func sshErrorText(err error) string {
	var mm *hostKeyMismatchError
	switch {
	case errors.As(err, &mm):
		return mm.Error()
	case errors.Is(err, errHostKeyUnknown):
		return "尚未确认主机指纹"
	case strings.Contains(err.Error(), "unable to authenticate"):
		return "SSH 认证失败：服务器不接受这个密钥（请把公钥加入服务器的 ~/.ssh/authorized_keys）"
	}
	return err.Error()
}

func fingerprintOf(authorizedKey string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// ---------------------------------------------------------------------------
// ~/.ssh/config import and keys
// ---------------------------------------------------------------------------

type sshHost struct {
	Alias        string `json:"alias"`
	HostName     string `json:"host"`
	User         string `json:"user"`
	Port         int    `json:"port"`
	IdentityFile string `json:"key_path"`
}

// handleSSHHosts lists concrete hosts from ~/.ssh/config for one-tap import.
func (s *Server) handleSSHHosts(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(expandHome("~/.ssh/config"))
	if err != nil {
		writeJSON(w, 200, map[string]any{"hosts": []sshHost{}})
		return
	}
	defer f.Close()
	var hosts []sshHost
	var cur []int // indices into hosts: a Host line can name several aliases
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, _ := strings.Cut(strings.Join(strings.Fields(strings.ReplaceAll(line, "=", " ")), " "), " ")
		switch strings.ToLower(key) {
		case "host":
			cur = nil
			for _, a := range strings.Fields(val) {
				if strings.ContainsAny(a, "*?!") {
					continue
				}
				hosts = append(hosts, sshHost{Alias: a, HostName: a, Port: 22})
				cur = append(cur, len(hosts)-1)
			}
		case "match":
			cur = nil
		case "hostname", "user", "port", "identityfile":
			for _, i := range cur {
				h := &hosts[i]
				switch strings.ToLower(key) {
				case "hostname":
					h.HostName = val
				case "user":
					h.User = val
				case "port":
					h.Port, _ = strconv.Atoi(val)
				case "identityfile":
					if h.IdentityFile == "" {
						h.IdentityFile = val
					}
				}
			}
		}
	}
	if hosts == nil {
		hosts = []sshHost{}
	}
	writeJSON(w, 200, map[string]any{"hosts": hosts})
}
