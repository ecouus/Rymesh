package main

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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer is a minimal sshd: public-key auth for one key, direct-tcpip
// forwarding (what the relay uses) and exec sessions (what deploy uses).
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
		if string(k.Marshal()) == string(clientPub.Marshal()) {
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

// clientKey writes an unencrypted ed25519 key the way ssh-keygen would.
func clientKey(t *testing.T) (path string, pub ssh.PublicKey) {
	t.Helper()
	path, line, err := generateKey(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, _ = ssh.ParseAuthorizedKey([]byte(line))
	return path, pub
}

func sshNodeFor(t *testing.T, srv *testSSHServer, keyPath string, nodeURL string) NodeConfig {
	host, port, _ := net.SplitHostPort(srv.addr)
	p, _ := strconv.Atoi(port)
	_, nport, _ := net.SplitHostPort(strings.TrimPrefix(nodeURL, "http://"))
	np, _ := strconv.Atoi(nport)
	return NodeConfig{ID: "box", Name: "Box", Transport: transportSSH, SSHHost: host, SSHPort: p,
		SSHUser: "me", KeyPath: keyPath, RemotePort: np, Token: "node-secret-token-123"}
}

func TestHubRelaysThroughSSHTunnel(t *testing.T) {
	// The node: a real server, reachable only as the SSH server's loopback.
	nodeCfg := testConfig(t)
	nodeCfg.NodeToken, nodeCfg.NodeName = "node-secret-token-123", "Box"
	nst, _ := openStore(nodeCfg.DataDir)
	defer nst.Close()
	node := newServer(nodeCfg, "", nst, log.New(io.Discard, "", 0), nil)
	_ = nst.CreateSession(&Session{Agent: "claude", Cwd: nodeCfg.DataDir, Title: "via ssh"})
	ns := httptest.NewServer(node.Handler())
	defer ns.Close()

	keyPath, pub := clientKey(t)
	sshd := newTestSSHServer(t, pub)
	n := sshNodeFor(t, sshd, keyPath, ns.URL)

	hubCfg := testConfig(t)
	hst, _ := openStore(hubCfg.DataDir)
	defer hst.Close()
	hub := newServer(hubCfg, "", hst, log.New(io.Discard, "", 0), nil)
	_ = hub.saveNodes(func(_ []NodeConfig) ([]NodeConfig, error) { return []NodeConfig{n}, nil })
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
		Hostkey HostKeyProbe
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
	out, err := hub.ssh.run(t.Context(), n, "echo hello-$((1+1))", nil)
	if err != nil || strings.TrimSpace(out) != "hello-2" {
		t.Fatalf("run = %q %v", out, err)
	}
}

func TestSSHRejectsChangedHostKey(t *testing.T) {
	keyPath, pub := clientKey(t)
	sshd := newTestSSHServer(t, pub)
	n := sshNodeFor(t, sshd, keyPath, "http://127.0.0.1:9")
	p, err := probeHostKey(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	n.HostKey = p.Key
	if _, err := dialSSH(t.Context(), n); err != nil {
		t.Fatalf("trusted key: %v", err)
	}
	// Same address, different server key: a man in the middle.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(other)
	impostor := startTestSSHServer(t, pub, signer)
	host, port, _ := net.SplitHostPort(impostor.addr)
	n.SSHHost, n.SSHPort = host, mustAtoi(port)
	_, err = dialSSH(t.Context(), n)
	var mm *hostKeyMismatchError
	if err == nil || !errors.As(err, &mm) {
		t.Fatalf("changed host key accepted: %v", err)
	}
}

func TestSSHConfigImportAndKeyGeneration(t *testing.T) {
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
	st, _ := openStore(cfg.DataDir)
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
	path, pub, err := generateKey(srv.keyDir(), "phone")
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("generate = %q %v", pub, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %v", st.Mode().Perm())
	}
	if _, _, err := generateKey(srv.keyDir(), "phone"); err == nil {
		t.Fatal("overwrote an existing key")
	}
}

func mustAtoi(s string) int { n, _ := strconv.Atoi(s); return n }
