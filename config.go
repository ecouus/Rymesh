package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultConfigPath = "/etc/rymesh/config.json"
	defaultDataDir    = "/var/lib/rymesh"
	defaultLogDir     = "/var/log/rymesh"
	defaultPort       = 8899
	configVersion     = 1
)

type Config struct {
	Version          int      `json:"version"`
	Port             int      `json:"port"`
	ListenAddrs      []string `json:"listen_addrs,omitempty"` // explicit addresses; empty = auto
	EasyTierIface    string   `json:"easytier_interface"`
	AllowPublicBind  bool     `json:"allow_public_bind"`
	DefaultCWD       string   `json:"default_cwd"`
	AllowedRoots     []string `json:"allowed_roots"`
	MaxConcurrent    int      `json:"max_concurrent"`
	DataDir          string   `json:"data_dir"`
	LogDir           string   `json:"log_dir"`
	Secret           string   `json:"secret"`
	SessionTTLHours  int      `json:"session_ttl_hours"`
	PermissionTTLMin int      `json:"permission_ttl_minutes"`
	TaskLogMaxBytes  int64    `json:"task_log_max_bytes"`
	ImportOnStart    bool     `json:"import_on_start"`
	// ImportHistoryLimit caps how many events a native snapshot shows in the
	// console. Native resume is unaffected. 0 = show everything.
	ImportHistoryLimit int `json:"import_history_limit"`
	// ImportExcludeRoots skips native sessions whose cwd is at or below one of
	// these directories. Without it, importing real CLI history also pulls
	// throwaway test sessions into the console.
	ImportExcludeRoots []string `json:"import_exclude_roots,omitempty"`
	// NodeName is how this machine is labelled in the device switcher
	// (default: hostname). NodeToken, when set, lets a hub call this node's
	// API with "Authorization: Bearer <token>". Nodes lists the other machines
	// this instance can reach as a hub. See nodes.go.
	NodeName string `json:"node_name,omitempty"`
	// HubOnly: this console runs no agents of its own (the phone app); the
	// device list then has no "local" entry.
	HubOnly   bool         `json:"hub_only,omitempty"`
	NodeToken string       `json:"node_token,omitempty"`
	Nodes     []NodeConfig `json:"nodes,omitempty"`
}

// clone returns a copy that shares no slices with c. Settings changes are
// copy-on-write, so every mutable slice must be copied here.
func (c *Config) clone() *Config {
	n := *c
	n.AllowedRoots = append([]string(nil), c.AllowedRoots...)
	n.ImportExcludeRoots = append([]string(nil), c.ImportExcludeRoots...)
	n.ListenAddrs = append([]string(nil), c.ListenAddrs...)
	n.Nodes = append([]NodeConfig(nil), c.Nodes...)
	return &n
}

// homeDir is the home of the user the console runs as: the default place for
// new conversations, as a terminal would open there.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		return h
	}
	return "/tmp"
}

func defaultConfig() *Config {
	home := homeDir()
	roots := []string{home}
	for _, r := range []string{"/home", "/root", "/tmp"} {
		if r != home && !strings.HasPrefix(home, r+"/") {
			roots = append(roots, r)
		}
	}
	return &Config{
		Version:          configVersion,
		Port:             defaultPort,
		EasyTierIface:    "tun0",
		DefaultCWD:       home,
		AllowedRoots:     roots,
		MaxConcurrent:    4,
		DataDir:          defaultDataDir,
		LogDir:           defaultLogDir,
		SessionTTLHours:  24 * 30,
		PermissionTTLMin: 60,
		TaskLogMaxBytes:  64 << 20,
		ImportOnStart:    true,
		// A large transcript used to be truncated with a "更早的 N 条…" notice,
		// which made the web history look incomplete. Keep the whole snapshot;
		// the UI already pages it with a 600-event initial window.
		ImportHistoryLimit: 0,
	}
}

func (c *Config) normalize() error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported config version %d (expected %d)", c.Version, configVersion)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	// The cookie HMAC key comes from Secret. An empty value signs with a
	// publicly known key, so anyone could mint a valid session cookie.
	if strings.TrimSpace(c.Secret) == "" {
		return errors.New("config secret is empty; refusing to sign sessions with a known key (run: rymesh install, or set a random secret)")
	}
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 1
	}
	if c.MaxConcurrent > 32 {
		c.MaxConcurrent = 32
	}
	if c.SessionTTLHours < 1 {
		c.SessionTTLHours = 24
	}
	if c.PermissionTTLMin < 1 {
		c.PermissionTTLMin = 60
	}
	if c.TaskLogMaxBytes < 1<<20 {
		c.TaskLogMaxBytes = 64 << 20
	}
	if c.EasyTierIface == "" {
		c.EasyTierIface = "tun0"
	}
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	if c.LogDir == "" {
		c.LogDir = defaultLogDir
	}
	if c.DefaultCWD == "" {
		c.DefaultCWD = homeDir()
	}
	if len(c.AllowedRoots) == 0 {
		c.AllowedRoots = []string{c.DefaultCWD}
	}
	var clean []string
	for _, r := range c.AllowedRoots {
		r = filepath.Clean(r)
		if r == "/" {
			continue
		}
		clean = append(clean, r)
	}
	c.AllowedRoots = clean
	if len(c.AllowedRoots) == 0 {
		return errors.New("allowed_roots must contain at least one non-root directory")
	}

	var excludes []string
	seenExclude := map[string]bool{}
	for _, r := range c.ImportExcludeRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return fmt.Errorf("import_exclude_roots must be absolute: %q", r)
		}
		r = filepath.Clean(r)
		if r == "/" {
			return errors.New(`import_exclude_roots must not contain "/"`)
		}
		if strings.ContainsAny(r, "*?[") {
			if _, err := filepath.Match(r, r); err != nil {
				return fmt.Errorf("invalid import_exclude_roots pattern %q: %w", r, err)
			}
		}
		if !seenExclude[r] {
			seenExclude[r] = true
			excludes = append(excludes, r)
		}
	}
	c.ImportExcludeRoots = excludes
	return nil
}

// isExcludedCWD reports whether cwd is at or below one of roots. Plain roots
// match path components, so "/tmp/ac-e2e" also covers "/tmp/ac-e2e/work" but
// not "/tmp/ac-e2e2". Roots containing wildcards are matched against cwd and
// each of its ancestors, so "/tmp/ac-*" excludes every /tmp/ac-* work tree.
func isExcludedCWD(cwd string, roots []string) bool {
	cwd = filepath.Clean(cwd)
	for _, root := range roots {
		root = filepath.Clean(root)
		if strings.ContainsAny(root, "*?[") {
			for p := cwd; ; {
				if ok, _ := filepath.Match(root, p); ok {
					return true
				}
				parent := filepath.Dir(p)
				if parent == p {
					break
				}
				p = parent
			}
			continue
		}
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveConfig(path string, c *Config) error {
	if err := c.normalize(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newConfig() (*Config, error) {
	c := defaultConfig()
	secret, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	c.Secret = base64.StdEncoding.EncodeToString(secret)
	return c, nil
}

func (c *Config) ensureDirs() error {
	for _, d := range []string{c.DataDir, c.LogDir,
		filepath.Join(c.DataDir, "tasks"),
		filepath.Join(c.DataDir, "trash"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func randomID(n int) string {
	b, err := randomBytes(n)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newUUID() string {
	b, err := randomBytes(16)
	if err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *Config) sign(data []byte) []byte {
	m := hmac.New(sha256.New, mustB64(c.Secret))
	m.Write(data)
	return m.Sum(nil)
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return []byte(s)
	}
	return b
}

// allowedCWD validates a user-supplied working directory.
func (c *Config) allowedCWD(p string) (string, error) {
	if p == "" {
		p = c.defaultCWD()
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("working directory must be absolute")
	}
	p = filepath.Clean(p)
	// Resolve symlinks so a link cannot escape an allowed root.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("working directory does not exist: %s", p)
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", p)
	}
	ok := false
	for _, root := range c.AllowedRoots {
		rr := root
		if r, err := filepath.EvalSymlinks(root); err == nil {
			rr = r
		}
		if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("working directory %s is outside allowed roots (%s)", p, strings.Join(c.AllowedRoots, ", "))
	}
	return real, nil
}

func listenAddresses(c *Config) ([]string, error) {
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

// defaultCWD is where a new conversation starts: the configured directory,
// or the home directory if that one has gone missing on this machine.
func (c *Config) defaultCWD() string {
	if st, err := os.Stat(c.DefaultCWD); err == nil && st.IsDir() {
		return c.DefaultCWD
	}
	if h := homeDir(); h != "" {
		return h
	}
	return c.DefaultCWD
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// laterTS returns whichever timestamp is later. RFC3339 strings are compared as
// instants rather than text: "+08:00" offsets and fractional-second spellings
// make lexical order disagree with time order. Unparsable values fall back to
// text order so a malformed timestamp is never dropped.
func laterTS(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea != nil || eb != nil {
		if a > b {
			return a
		}
		return b
	}
	if tb.After(ta) {
		return b
	}
	return a
}
