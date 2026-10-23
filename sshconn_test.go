package console

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"rymesh/internal/config"
	"rymesh/internal/sshconn"
	"rymesh/internal/store"
)

// testSSHServer is a minimal sshd: public-key auth for one key (and for keys
// in $HOME/.ssh/authorized_keys), password auth when testPassword is set,
// direct-tcpip forwarding (what the relay uses) and exec sessions (what
// deploy uses).
type testSSHServer struct {
	addr    string
	hostKey ssh.Signer
	ln      net.Listener
	mu      sync.Mutex
	conns   []net.Conn
}

func newTestSSHServer(t *testing.T, clientPub ssh.PublicKey) *testSSHServer {
	t.Helper()
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hk)
	return startTestSSHServer(t, clientPub, signer)
}

func startTestSSHServer(t *testing.T, clientPub ssh.PublicKey, hostKey ssh.Signer) *testSSHServer {
	t.Helper()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if clientPub != nil && string(k.Marshal()) == string(clientPub.Marshal()) {
			return nil, nil
		}
		b, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "authorized_keys"))
		for len(b) > 0 {
			ak, _, _, rest, err := ssh.ParseAuthorizedKey(b)
			if err != nil {
				break
			}
			if string(ak.Marshal()) == string(k.Marshal()) {
				return nil, nil
			}
			b = rest
		}
		return nil, io.EOF
	}, PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if testPassword != "" && string(pw) == testPassword {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHServer{addr: ln.Addr().String(), hostKey: hostKey, ln: ln}
	t.Cleanup(s.close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			go s.serve(c, cfg)
		}
	}()
	return s
}

// dropAll cuts every live connection, as a network blip would.
func (s *testSSHServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *testSSHServer) close() { s.ln.Close(); s.dropAll() }

func (s *testSSHServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "direct-tcpip":
			var d struct {
				Host     string
				Port     uint32
				OrigHost string
				OrigPort uint32
			}
			_ = ssh.Unmarshal(nc.ExtraData(), &d)
			up, err := net.Dial("tcp", net.JoinHostPort(d.Host, strconv.Itoa(int(d.Port))))
			if err != nil {
				nc.Reject(ssh.ConnectionFailed, "connection refused")
				continue
			}
			ch, r, _ := nc.Accept()
			go ssh.DiscardRequests(r)
			go func() { io.Copy(ch, up); ch.CloseWrite() }()
			go func() { io.Copy(up, ch); up.Close() }()
		case "session":
			ch, r, _ := nc.Accept()
			go func() {
				for req := range r {
					if req.Type != "exec" {
						req.Reply(false, nil)
						continue
					}
					var p struct{ Command string }
					_ = ssh.Unmarshal(req.Payload, &p)
					req.Reply(true, nil)
					cmd := exec.Command("sh", "-c", p.Command)
					cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
					code := 0
					if err := cmd.Run(); err != nil {
						code = 1
					}
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{uint32(code)}))
					ch.Close()
					return
				}
			}()
		default:
			nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

// testPassword, when set, is the password the test sshd accepts.
var testPassword string

// clientKey writes an unencrypted ed25519 key the way ssh-keygen would.
func clientKey(t *testing.T) (path string, pub ssh.PublicKey) {
	t.Helper()
	b, err := newKeyPair("test")
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "id_test")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.ParsePrivateKey(b)
	return path, signer.PublicKey()
}

func sshNodeFor(t *testing.T, srv *testSSHServer, keyPath string, nodeURL string) config.NodeConfig {
	host, port, _ := net.SplitHostPort(srv.addr)
	p, _ := strconv.Atoi(port)
	_, nport, _ := net.SplitHostPort(strings.TrimPrefix(nodeURL, "http://"))
	np, _ := strconv.Atoi(nport)
	return config.NodeConfig{ID: "box", Name: "Box", Transport: config.TransportSSH, SSHHost: host, SSHPort: p,
		SSHUser: "me", KeyPath: keyPath, RemotePort: np, Token: "node-secret-token-123"}
}

func TestHubRelaysThroughSSHTunnel(t *testing.T) {
	// The node: a real server, reachable only as the SSH server's loopback.
	nodeCfg := testConfig(t)
	nodeCfg.NodeToken, nodeCfg.NodeName = "node-secret-token-123", "Box"
	nst, _ := store.OpenStore(nodeCfg.DataDir)
	defer nst.Close()
	node := newServer(nodeCfg, "", nst, log.New(io.Discard, "", 0), nil)
	_ = nst.CreateSession(&store.Session{Agent: "claude", Cwd: nodeCfg.DataDir, Title: "via ssh"})
	ns := httptest.NewServer(node.Handler())
	defer ns.Close()

	keyPath, pub := clientKey(t)
	sshd := newTestSSHServer(t, pub)
	n := sshNodeFor(t, sshd, keyPath, ns.URL)

	hubCfg := testConfig(t)
	hst, _ := store.OpenStore(hubCfg.DataDir)
	defer hst.Close()
	hub := newServer(hubCfg, "", hst, log.New(io.Discard, "", 0), nil)
	_ = hub.saveNodes(func(_ []config.NodeConfig) ([]config.NodeConfig, error) { return []config.NodeConfig{n}, nil })
	tok := hub.auth.token()
	info, _ := hub.auth.verifyToken(tok)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		req.Header.Set("X-CSRF-Token", info.CSRF)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		hub.Handler().ServeHTTP(rec, req)
		return rec
	}

	// First contact: the key is unknown, so the test asks for confirmation.
	rec := do("POST", "/api/devices/box/test", "{}")
	var probe struct {
		Stage   string
		Hostkey sshconn.HostKeyProbe
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &probe)
	if probe.Stage != "hostkey" || probe.Hostkey.Fingerprint != ssh.FingerprintSHA256(sshd.hostKey.PublicKey()) {
		t.Fatalf("first test = %s", rec.Body.String())
	}
	// Nothing is relayed before the key is trusted.
	if rec := do("GET", "/n/box/api/sessions", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("untrusted relay: %d", rec.Code)
	}
	if rec := do("POST", "/api/devices/box/trust", `{"key":`+strconv.Quote(probe.Hostkey.Key)+`}`); rec.Code != 200 {
		t.Fatalf("trust: %d %s", rec.Code, rec.Body.String())
	}
	rec = do("POST", "/api/devices/box/test", "{}")
	if !strings.Contains(rec.Body.String(), `"stage":"ready"`) {
		t.Fatalf("second test = %s", rec.Body.String())
	}

	if rec := do("GET", "/n/box/api/sessions", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "via ssh") {
		t.Fatalf("relay over ssh: %d %s", rec.Code, rec.Body.String())
	}
	// A dropped connection is redialed on the next request.
	sshd.dropAll()
	time.Sleep(100 * time.Millisecond)
	if rec := do("GET", "/n/box/api/sessions", ""); rec.Code != 200 {
		t.Fatalf("after drop: %d %s", rec.Code, rec.Body.String())
	}
	// Commands run on the device (deploy's building block).
	n, _ = hub.device("box")
	out, err := hub.ssh.Run(t.Context(), n, "echo hello-$((1+1))", nil)
	if err != nil || strings.TrimSpace(out) != "hello-2" {
		t.Fatalf("run = %q %v", out, err)
	}
}

func TestSSHRejectsChangedHostKey(t *testing.T) {
	keyPath, pub := clientKey(t)
	sshd := newTestSSHServer(t, pub)
	n := sshNodeFor(t, sshd, keyPath, "http://127.0.0.1:9")
	p, err := sshconn.ProbeHostKey(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	n.HostKey = p.Key
	if _, err := sshconn.DialSSH(t.Context(), n, nil); err != nil {
		t.Fatalf("trusted key: %v", err)
	}
	// Same address, different server key: a man in the middle.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(other)
	impostor := startTestSSHServer(t, pub, signer)
	host, port, _ := net.SplitHostPort(impostor.addr)
	n.SSHHost, n.SSHPort = host, mustAtoi(port)
	_, err = sshconn.DialSSH(t.Context(), n, nil)
	var mm *sshconn.HostKeyMismatchError
	if err == nil || !errors.As(err, &mm) {
		t.Fatalf("changed host key accepted: %v", err)
	}
}

func TestSSHConfigImport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(`
Host *
  ServerAliveInterval 30
Host prod web1
  HostName 203.0.113.7
  User deploy
  Port 2222
  IdentityFile ~/.ssh/prod_ed25519
`), 0o600)
	cfg := testConfig(t)
	st, _ := store.OpenStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	rec := httptest.NewRecorder()
	srv.handleSSHHosts(rec, httptest.NewRequest("GET", "/api/ssh/hosts", nil))
	var r struct{ Hosts []sshHost }
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Hosts) != 2 || r.Hosts[0].HostName != "203.0.113.7" || r.Hosts[0].Port != 2222 ||
		r.Hosts[0].User != "deploy" || r.Hosts[1].Alias != "web1" {
		t.Fatalf("hosts = %+v", r.Hosts)
	}
}

func mustAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

func TestEmbeddedHubOnly(t *testing.T) {
	dir := t.TempDir()
	e, err := StartEmbedded(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	jar := &cookieJar{}
	c := &http.Client{Jar: jar, Transport: nodeTransport}
	resp, err := c.Get(e.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = c.Get("http://127.0.0.1:" + strconv.Itoa(e.Port()) + "/api/nodes")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Nodes   []nodeView
		HubOnly bool `json:"hub_only"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if resp.StatusCode != 200 || !r.HubOnly || len(r.Nodes) != 0 {
		t.Fatalf("nodes: %d %+v", resp.StatusCode, r)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("config not stored in the app dir")
	}
}

// cookieJar is the minimum http.CookieJar for one host.
type cookieJar struct {
	mu sync.Mutex
	c  []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	j.c = append(j.c, cs...)
	j.mu.Unlock()
}
func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie { j.mu.Lock(); defer j.mu.Unlock(); return j.c }

func TestDialFailureNamesTheCause(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close() // nothing listens here now
	keyPath, _ := clientKey(t)
	n := config.NodeConfig{ID: "x", Transport: config.TransportSSH, SSHHost: "127.0.0.1", SSHPort: mustAtoi(port), SSHUser: "root", KeyPath: keyPath,
		HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}
	_, err := sshconn.DialSSH(t.Context(), n, nil)
	if got := dialFailure(err); !strings.Contains(got, "连接被拒绝") {
		t.Fatalf("refused: %q (%v)", got, err)
	}
	if got := dialFailure(errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]")); !strings.Contains(got, "认证失败") {
		t.Fatalf("auth: %q", got)
	}
}

// A Mac that runs the app's console is joined as it is: deploy asks that
// console's own program for a node token over SSH, installs nothing, and the
// running console accepts the token without a restart.
func TestDeployAdoptsMacClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".rymesh")
	cfgPath := filepath.Join(base, "config.json")

	macCfg := testConfig(t)
	macCfg.NodeName = "Mac"
	mst, _ := store.OpenStore(macCfg.DataDir)
	defer mst.Close()
	_ = mst.CreateSession(&store.Session{Agent: "claude", Cwd: macCfg.DataDir, Title: "on the mac"})
	mac := newServer(macCfg, cfgPath, mst, log.New(io.Discard, "", 0), nil)
	ms := httptest.NewServer(mac.Handler())
	defer ms.Close()
	_, mport, _ := net.SplitHostPort(strings.TrimPrefix(ms.URL, "http://"))

	keyPath, pub := clientKey(t)
	sshd := newTestSSHServer(t, pub)
	n := sshNodeFor(t, sshd, keyPath, ms.URL)
	n.Token, n.RemotePort = "", 0
	n.HostKey = string(ssh.MarshalAuthorizedKey(sshd.hostKey.PublicKey()))
	hub := newTestHub(t, testConfig(t), "")
	_ = hub.srv.saveNodes(func(_ []config.NodeConfig) ([]config.NodeConfig, error) { return []config.NodeConfig{n}, nil })

	// No console on the machine: not adopted, deploy goes on as usual.
	if _, ok, err := hub.srv.adoptClient(t.Context(), n, func(string) {}); ok || err != nil {
		t.Fatalf("adopted a machine without a console: %v %v", ok, err)
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(cfgPath, macCfg); err != nil {
		t.Fatal(err)
	}

	// Stand-in for the app's program: `node token` writes the token to the
	// config file and prints it, as the real command does.
	withToken := *macCfg
	withToken.NodeToken = "mac-node-token-0123456789"
	withToken.Port, _ = strconv.Atoi(mport)
	if err := config.SaveConfig(cfgPath+".next", &withToken); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "rymesh-app")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"node token\" ] || exit 2\ncp \"$4.next\" \"$4\"\necho " + withToken.NodeToken + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, clientBinFile), []byte(bin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var steps []string
	got, ok, err := hub.srv.adoptClient(t.Context(), n, func(s string) { steps = append(steps, s) })
	if !ok || err != nil {
		t.Fatalf("adopt = %v %v (%q)", ok, err, steps)
	}
	if got.Token != withToken.NodeToken || got.RemotePort != withToken.Port {
		t.Fatalf("device = token %q port %d", got.Token, got.RemotePort)
	}
	if rec := hub.do("GET", "/n/box/api/sessions", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "on the mac") {
		t.Fatalf("relay to the mac: %d %s", rec.Code, rec.Body.String())
	}
}

// A wrong token makes the node re-read its config at most once a second, and
// never accepts anything but the token in the file.
func TestNodeTokenReloadIsBounded(t *testing.T) {
	cfg := testConfig(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	st, _ := store.OpenStore(cfg.DataDir)
	defer st.Close()
	srv := newServer(cfg, cfgPath, st, log.New(io.Discard, "", 0), nil)
	req := func(tok string) bool {
		r := httptest.NewRequest("GET", "/api/sessions", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return srv.nodeTokenOK(r)
	}
	if req("") || req("guess") {
		t.Fatal("accepted a token while none is set")
	}
	next := *cfg
	next.NodeToken = "written-by-node-token"
	if err := config.SaveConfig(cfgPath, &next); err != nil {
		t.Fatal(err)
	}
	// Read less than a second ago: the file is not read again yet.
	if req("written-by-node-token") {
		t.Fatal("config re-read within a second")
	}
	srv.tokenMu.Lock()
	srv.tokenRead = time.Time{}
	srv.tokenMu.Unlock()
	if !req("written-by-node-token") || req("guess") {
		t.Fatal("token from the config file not picked up")
	}
	if srv.config().NodeToken != "written-by-node-token" {
		t.Fatal("token not published")
	}
}
