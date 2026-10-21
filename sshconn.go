package main

// SSH transport for devices (NodeConfig.Transport == "ssh").
//
// The client holds one SSH connection per device and reaches the node's
// loopback-only HTTP port through it (a direct-tcpip channel, as `ssh -L`
// would). The node therefore exposes nothing beyond sshd, and no VPN is
// needed. The relay in nodes.go is unchanged; only its DialContext differs.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

const transportSSH = "ssh"

// errHostKeyUnknown means the device has no pinned host key yet; the user
// must confirm the fingerprint first (handleDeviceTest).
var errHostKeyUnknown = errors.New("host key not trusted yet")

// hostKeyMismatchError is returned when a server presents a key other than
// the pinned one. It is never bypassed: that is what a man in the middle
// looks like.
type hostKeyMismatchError struct{ got string }

func (e *hostKeyMismatchError) Error() string {
	return "主机密钥与已信任的不一致（" + e.got + "），已拒绝连接"
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (n NodeConfig) sshAddr() string {
	port := n.SSHPort
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(n.SSHHost, strconv.Itoa(port))
}

func (n NodeConfig) remotePort() int {
	if n.RemotePort == 0 {
		return defaultPort
	}
	return n.RemotePort
}

// baseURL is where the relay sends requests. For SSH devices the host part is
// only a label: the dial goes through the tunnel to the node's loopback.
func (n NodeConfig) baseURL() string {
	if n.Transport == transportSSH {
		return "http://127.0.0.1:" + strconv.Itoa(n.remotePort())
	}
	return n.URL
}

// sshAuth collects the configured authentication methods: a key file
// (unencrypted; protected keys belong in ssh-agent) and/or the agent.
func sshAuth(n NodeConfig) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	cleanup := func() {}
	if n.UseAgent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, cleanup, errors.New("未找到 ssh-agent（SSH_AUTH_SOCK 为空）")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, cleanup, fmt.Errorf("连接 ssh-agent 失败：%w", err)
		}
		cleanup = func() { conn.Close() }
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}
	if n.KeyPath != "" {
		b, err := os.ReadFile(expandHome(n.KeyPath))
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("读取私钥失败：%w", err)
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			cleanup()
			var pp *ssh.PassphraseMissingError
			if errors.As(err, &pp) {
				return nil, func() {}, errors.New("私钥有密码保护，请把它加入 ssh-agent 并改用 ssh-agent 认证")
			}
			return nil, func() {}, fmt.Errorf("解析私钥失败：%w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(methods) == 0 {
		return nil, cleanup, errors.New("没有配置密钥或 ssh-agent")
	}
	return methods, cleanup, nil
}

// pinnedHostKey verifies the server against NodeConfig.HostKey only.
func pinnedHostKey(n NodeConfig) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if n.HostKey == "" {
			return errHostKeyUnknown
		}
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(n.HostKey))
		if err != nil {
			return fmt.Errorf("已保存的主机密钥无效：%w", err)
		}
		if !bytes.Equal(want.Marshal(), key.Marshal()) {
			return &hostKeyMismatchError{got: ssh.FingerprintSHA256(key)}
		}
		return nil
	}
}

// hostKeyAlgorithms asks the server for the pinned key's type, so a server
// with several host keys always presents the one that was confirmed. Before
// pinning, ed25519 comes first: it is the key users are told to compare.
func hostKeyAlgorithms(n NodeConfig) []string {
	if n.HostKey != "" {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(n.HostKey)); err == nil {
			if k.Type() == ssh.KeyAlgoRSA {
				return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
			}
			return []string{k.Type()}
		}
	}
	return []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
}

func dialSSH(ctx context.Context, n NodeConfig) (*ssh.Client, error) {
	auth, cleanup, err := sshAuth(n)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cfg := &ssh.ClientConfig{
		User: n.SSHUser, Auth: auth, HostKeyCallback: pinnedHostKey(n), Timeout: 10 * time.Second,
		HostKeyAlgorithms: hostKeyAlgorithms(n),
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", n.sshAddr())
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, n.sshAddr(), cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// HostKeyProbe is what the device test reports before a key is trusted.
type HostKeyProbe struct {
	Key         string `json:"key"`         // authorized_keys line, to pin
	Fingerprint string `json:"fingerprint"` // SHA256:...
	Known       bool   `json:"known"`       // matches ~/.ssh/known_hosts
	Mismatch    bool   `json:"mismatch"`    // known_hosts has a different key
}

// probeHostKey fetches the server's host key without authenticating, and
// checks it against the user's known_hosts.
func probeHostKey(ctx context.Context, n NodeConfig) (*HostKeyProbe, error) {
	var got ssh.PublicKey
	var remote net.Addr
	stop := errors.New("captured")
	cfg := &ssh.ClientConfig{User: n.SSHUser, Timeout: 10 * time.Second, HostKeyAlgorithms: hostKeyAlgorithms(n),
		HostKeyCallback: func(_ string, r net.Addr, key ssh.PublicKey) error { got, remote = key, r; return stop }}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", n.sshAddr())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	_, _, _, err = ssh.NewClientConn(conn, n.sshAddr(), cfg)
	if got == nil {
		return nil, err
	}
	p := &HostKeyProbe{Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(got))), Fingerprint: ssh.FingerprintSHA256(got)}
	if kh, err := knownhosts.New(expandHome("~/.ssh/known_hosts")); err == nil {
		switch err := kh(knownhosts.Normalize(n.sshAddr()), remote, got); {
		case err == nil:
			p.Known = true
		default:
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) > 0 {
				p.Mismatch = true
			}
		}
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Connection pool
// ---------------------------------------------------------------------------

type sshEntry struct {
	mu        sync.Mutex
	cfg       NodeConfig
	client    *ssh.Client
	lastErr   error
	failUntil time.Time
	backoff   time.Duration
}

type sshPool struct {
	mu         sync.Mutex
	entries    map[string]*sshEntry
	transports map[string]*http.Transport
}

func newSSHPool() *sshPool {
	return &sshPool{entries: map[string]*sshEntry{}, transports: map[string]*http.Transport{}}
}

// key identifies a device's connection settings; editing a device makes a
// fresh entry instead of reusing a connection made with old credentials.
func (n NodeConfig) sshKey() string {
	return strings.Join([]string{n.ID, n.SSHHost, strconv.Itoa(n.SSHPort), n.SSHUser,
		n.KeyPath, strconv.FormatBool(n.UseAgent), n.HostKey}, "|")
}

func (p *sshPool) entry(n NodeConfig) *sshEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := n.sshKey()
	e := p.entries[k]
	if e == nil {
		e = &sshEntry{cfg: n}
		p.entries[k] = e
	}
	return e
}

// client returns a live connection, dialing (with backoff after failures) when
// there is none.
func (p *sshPool) client(ctx context.Context, n NodeConfig) (*ssh.Client, error) {
	e := p.entry(n)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client, nil
	}
	if time.Now().Before(e.failUntil) {
		return nil, e.lastErr
	}
	c, err := dialSSH(ctx, n)
	if err != nil {
		e.lastErr = err
		if e.backoff == 0 {
			e.backoff = 2 * time.Second
		} else if e.backoff < 30*time.Second {
			e.backoff *= 2
		}
		// A wrong key or host key will not fix itself; don't hammer the server.
		e.failUntil = time.Now().Add(e.backoff)
		return nil, err
	}
	e.client, e.lastErr, e.backoff = c, nil, 0
	go p.keepalive(e, c)
	return c, nil
}

// keepalive drops the connection when the server stops answering, so the
// next request redials instead of hanging on a dead TCP stream.
func (p *sshPool) keepalive(e *sshEntry, c *ssh.Client) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	for {
		select {
		case <-done:
			p.drop(e, c)
			return
		case <-t.C:
			res := make(chan error, 1)
			go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); res <- err }()
			select {
			case err := <-res:
				if err == nil {
					continue
				}
			case <-time.After(15 * time.Second):
			}
			c.Close()
			p.drop(e, c)
			return
		}
	}
}

func (p *sshPool) drop(e *sshEntry, c *ssh.Client) {
	e.mu.Lock()
	if e.client == c {
		e.client = nil
	}
	e.mu.Unlock()
}

// dial opens a channel to the node's loopback HTTP port, redialing once if a
// pooled connection turns out to be dead.
func (p *sshPool) dial(ctx context.Context, n NodeConfig) (net.Conn, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(n.remotePort()))
	for attempt := 0; attempt < 2; attempt++ {
		c, err := p.client(ctx, n)
		if err != nil {
			return nil, err
		}
		conn, err := c.Dial("tcp", addr)
		if err == nil {
			return conn, nil
		}
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) {
			// The connection is fine; the node itself is not listening.
			return nil, fmt.Errorf("节点程序未在 %s 运行：%s", addr, oe.Message)
		}
		c.Close()
		p.drop(p.entry(n), c)
	}
	return nil, errors.New("SSH 连接中断")
}

// transport is the http.RoundTripper the relay uses for an SSH device.
func (p *sshPool) transport(n NodeConfig) http.RoundTripper {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := n.sshKey() + "|" + strconv.Itoa(n.remotePort())
	if t := p.transports[k]; t != nil {
		return t
	}
	t := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dial(ctx, n)
		},
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	p.transports[k] = t
	return t
}

// run executes a command on the device and returns its combined output.
func (p *sshPool) run(ctx context.Context, n NodeConfig, cmd string, stdin []byte) (string, error) {
	c, err := p.client(ctx, n)
	if err != nil {
		return "", err
	}
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	// stdout and stderr are copied by separate goroutines; one shared buffer
	// needs a lock or the two streams corrupt it.
	var out lockedBuffer
	sess.Stdout, sess.Stderr = &out, &out
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		err = ctx.Err()
	}
	return out.String(), err
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// Keys managed by the client (phones have no ~/.ssh)
// ---------------------------------------------------------------------------

// generateKey creates an ed25519 key pair in dir and returns the public key
// line to add to a server's authorized_keys.
func generateKey(dir, name string) (path, pub string, err error) {
	if !validID(name) {
		return "", "", errors.New("密钥名只能包含字母、数字、- 和 _")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return "", "", errors.New("已存在同名密钥")
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, "agentconsole "+name)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	spk, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk))) + " agentconsole-" + name
	if err := os.WriteFile(path+".pub", []byte(pub+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return path, pub, nil
}
