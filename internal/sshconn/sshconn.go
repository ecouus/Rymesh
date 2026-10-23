package sshconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"rymesh/internal/config"
	"rymesh/internal/util"
)

// ErrHostKeyUnknown means the device has no pinned host key yet; the user
// must confirm the fingerprint first (handleDeviceTest).
var ErrHostKeyUnknown = errors.New("host key not trusted yet")

// HostKeyMismatchError is returned when a server presents a key other than
// the pinned one. It is never bypassed: that is what a man in the middle
// looks like.
type HostKeyMismatchError struct{ got string }

func (e *HostKeyMismatchError) Error() string {
	return "主机密钥与已信任的不一致（" + e.got + "），已拒绝连接"
}

// sshAuth collects the configured authentication methods: a vault key, a key
// file (unencrypted; protected keys are imported into the vault or kept in
// ssh-agent) and/or the agent.
func sshAuth(n config.NodeConfig, keys KeySource) ([]ssh.AuthMethod, func(), error) {
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
	if n.KeyID != "" {
		if keys == nil {
			cleanup()
			return nil, func() {}, errors.New("密钥库不可用")
		}
		b, err := keys.PrivateKey(n.KeyID)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("解析私钥失败：%w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	} else if n.KeyPath != "" {
		b, err := os.ReadFile(util.ExpandHome(n.KeyPath))
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("读取私钥失败：%w", err)
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			cleanup()
			var pp *ssh.PassphraseMissingError
			if errors.As(err, &pp) {
				return nil, func() {}, errors.New("私钥有密码短语保护：请在「密钥」里导入它（输入一次密码短语），或改用 ssh-agent")
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

// PinnedHostKey verifies the server against NodeConfig.HostKey only.
func PinnedHostKey(n config.NodeConfig) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if n.HostKey == "" {
			return ErrHostKeyUnknown
		}
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(n.HostKey))
		if err != nil {
			return fmt.Errorf("已保存的主机密钥无效：%w", err)
		}
		if !bytes.Equal(want.Marshal(), key.Marshal()) {
			return &HostKeyMismatchError{got: ssh.FingerprintSHA256(key)}
		}
		return nil
	}
}

// HostKeyAlgorithms asks the server for the pinned key's type, so a server
// with several host keys always presents the one that was confirmed. Before
// pinning, ed25519 comes first: it is the key users are told to compare.
func HostKeyAlgorithms(n config.NodeConfig) []string {
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

func DialSSH(ctx context.Context, n config.NodeConfig, keys KeySource) (*ssh.Client, error) {
	auth, cleanup, err := sshAuth(n, keys)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cfg := &ssh.ClientConfig{
		User: n.SSHUser, Auth: auth, HostKeyCallback: PinnedHostKey(n), Timeout: 10 * time.Second,
		HostKeyAlgorithms: HostKeyAlgorithms(n),
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", n.SSHAddr())
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, n.SSHAddr(), cfg)
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

// ProbeHostKey fetches the server's host key without authenticating, and
// checks it against the user's known_hosts.
func ProbeHostKey(ctx context.Context, n config.NodeConfig) (*HostKeyProbe, error) {
	var got ssh.PublicKey
	var remote net.Addr
	stop := errors.New("captured")
	cfg := &ssh.ClientConfig{User: n.SSHUser, Timeout: 10 * time.Second, HostKeyAlgorithms: HostKeyAlgorithms(n),
		HostKeyCallback: func(_ string, r net.Addr, key ssh.PublicKey) error { got, remote = key, r; return stop }}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", n.SSHAddr())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	_, _, _, err = ssh.NewClientConn(conn, n.SSHAddr(), cfg)
	if got == nil {
		return nil, err
	}
	p := &HostKeyProbe{Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(got))), Fingerprint: ssh.FingerprintSHA256(got)}
	if kh, err := knownhosts.New(util.ExpandHome("~/.ssh/known_hosts")); err == nil {
		switch err := kh(knownhosts.Normalize(n.SSHAddr()), remote, got); {
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
	cfg       config.NodeConfig
	client    *ssh.Client
	lastErr   error
	failUntil time.Time
	backoff   time.Duration
}

type Pool struct {
	mu         sync.Mutex
	entries    map[string]*sshEntry
	transports map[string]*http.Transport
	keys       KeySource
}

func NewPool(keys KeySource) *Pool {
	return &Pool{entries: map[string]*sshEntry{}, transports: map[string]*http.Transport{}, keys: keys}
}

func (p *Pool) entry(n config.NodeConfig) *sshEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := n.SSHKey()
	e := p.entries[k]
	if e == nil {
		e = &sshEntry{cfg: n}
		p.entries[k] = e
	}
	return e
}

// client returns a live connection, dialing (with backoff after failures) when
// there is none.
func (p *Pool) client(ctx context.Context, n config.NodeConfig) (*ssh.Client, error) {
	e := p.entry(n)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client, nil
	}
	if time.Now().Before(e.failUntil) {
		return nil, e.lastErr
	}
	c, err := DialSSH(ctx, n, p.keys)
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

// Retry lifts the backoff after a failure: the user just fixed something
// (installed the key, trusted the host) and asked to connect again.
func (p *Pool) Retry(n config.NodeConfig) {
	e := p.entry(n)
	e.mu.Lock()
	e.failUntil, e.backoff = time.Time{}, 0
	e.mu.Unlock()
}

// keepalive drops the connection when the server stops answering, so the
// next request redials instead of hanging on a dead TCP stream.
func (p *Pool) keepalive(e *sshEntry, c *ssh.Client) {
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

func (p *Pool) drop(e *sshEntry, c *ssh.Client) {
	e.mu.Lock()
	if e.client == c {
		e.client = nil
	}
	e.mu.Unlock()
}

// dial opens a channel to the node's loopback HTTP port, redialing once if a
// pooled connection turns out to be dead.
func (p *Pool) dial(ctx context.Context, n config.NodeConfig) (net.Conn, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(n.EffectiveRemotePort()))
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

// Transport is the http.RoundTripper the relay uses for an SSH device.
func (p *Pool) Transport(n config.NodeConfig) http.RoundTripper {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := n.SSHKey() + "|" + strconv.Itoa(n.EffectiveRemotePort())
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

// Run executes a command on the device and returns its combined output.
func (p *Pool) Run(ctx context.Context, n config.NodeConfig, cmd string, stdin []byte) (string, error) {
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

// KeySource resolves NodeConfig.KeyID to a private key (the Store).
type KeySource interface {
	PrivateKey(id string) ([]byte, error)
}
