package console

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testHub is a client console with an authenticated request helper.
type testHub struct {
	srv *Server
	st  *Store
	do  func(method, path, body string) *httptest.ResponseRecorder
}

func newTestHub(t *testing.T, cfg *Config, cfgPath string) *testHub {
	t.Helper()
	st, err := openStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := newServer(cfg, cfgPath, st, log.New(io.Discard, "", 0), nil)
	tok := srv.auth.token()
	info, _ := srv.auth.verifyToken(tok)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		req.Header.Set("X-CSRF-Token", info.CSRF)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	return &testHub{srv: srv, st: st, do: do}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestVaultMigratesConfigDevicesAndKeyFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := testConfig(t)
	keyDir := filepath.Join(cfg.DataDir, "keys")
	_ = os.MkdirAll(keyDir, 0o700)
	priv, _ := newKeyPair("old")
	_ = os.WriteFile(filepath.Join(keyDir, "phone"), priv, 0o600)
	_ = os.WriteFile(filepath.Join(keyDir, "phone.pub"), []byte("ssh-ed25519 AAAA phone\n"), 0o644)
	cfg.Nodes = []NodeConfig{
		{ID: "dbox", Name: "Box", Transport: transportSSH, SSHHost: "h", SSHUser: "root", KeyPath: filepath.Join(keyDir, "phone"), Token: "tok"},
		{ID: "mac", Name: "Mac", URL: "http://10.0.0.2:8899", Token: "t2"},
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := saveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	h := newTestHub(t, cfg, cfgPath)

	nodes := h.srv.config().Nodes
	if len(nodes) != 2 || nodes[0].KeyID == "" || nodes[0].KeyPath != "" || nodes[1].ID != "mac" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if b, err := h.st.privateKey(nodes[0].KeyID); err != nil || string(b) != string(priv) {
		t.Fatalf("key not moved: %v", err)
	}
	if raw, _ := loadConfig(cfgPath); len(raw.Nodes) != 0 {
		t.Fatalf("config.json still lists devices")
	}
	if _, err := os.Stat(cfgPath + ".pre-vault"); err != nil {
		t.Fatal("no backup of config.json")
	}
	if _, err := os.Stat(keyDir); !os.IsNotExist(err) {
		t.Fatal("key files left in place")
	}
	if fi, _ := os.Stat(filepath.Join(cfg.DataDir, "agentconsole.db")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v", fi.Mode().Perm())
	}
	// A second start reads the same list from the database alone.
	cfg2, _ := loadConfig(cfgPath)
	cfg2.DataDir = cfg.DataDir
	h2 := newTestHub(t, cfg2, cfgPath)
	if got := h2.srv.config().Nodes; len(got) != 2 || got[0].KeyID != nodes[0].KeyID {
		t.Fatalf("reloaded nodes = %+v", got)
	}
}

func TestVaultKeyLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := testConfig(t)
	cfg.NodeName = "手机"
	h := newTestHub(t, cfg, "")
	type keyResp struct{ Key vaultKey }

	// Unnamed keys never collide (the old date-based name did, silently).
	a := decode[keyResp](t, h.do("POST", "/api/keys/generate", `{}`)).Key
	b := decode[keyResp](t, h.do("POST", "/api/keys/generate", `{}`)).Key
	if a.ID == "" || b.ID == "" || a.Name == b.Name || !strings.HasPrefix(a.Public, "ssh-ed25519 ") {
		t.Fatalf("generated %+v %+v", a, b)
	}
	if rec := h.do("POST", "/api/keys/generate", `{"name":`+strconv.Quote(a.Name)+`}`); rec.Code != 409 {
		t.Fatalf("duplicate name: %d", rec.Code)
	}
	if rec := h.do("PATCH", "/api/keys/"+a.ID, `{"name":"工作 Mac"}`); rec.Code != 200 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	dev := `{"transport":"ssh","name":"Box","ssh_host":"203.0.113.9","ssh_user":"root","key_id":"` + a.ID + `"}`
	if rec := h.do("POST", "/api/devices", dev); rec.Code != 201 {
		t.Fatalf("create device: %d %s", rec.Code, rec.Body.String())
	}
	list := decode[struct{ Keys []vaultKey }](t, h.do("GET", "/api/keys", ""))
	if len(list.Keys) != 2 || list.Keys[0].Name != "工作 Mac" || len(list.Keys[0].UsedBy) != 1 ||
		!strings.HasSuffix(list.Keys[0].Public, " agentconsole-工作-Mac") {
		t.Fatalf("keys = %+v", list.Keys)
	}
	if rec := h.do("DELETE", "/api/keys/"+a.ID, ""); rec.Code != 409 {
		t.Fatalf("deleted a key in use: %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/keys/"+b.ID, ""); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
}

func TestVaultImportsProtectedKeyFromSSHDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	_ = os.MkdirAll(sshDir, 0o700)
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(sk, "me@laptop", []byte("open sesame"))
	path := filepath.Join(sshDir, "id_ed25519")
	_ = os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	signer, _ := ssh.NewSignerFromKey(sk)
	_ = os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)

	cfg := testConfig(t)
	h := newTestHub(t, cfg, "")
	_ = h.srv.saveNodes(func([]NodeConfig) ([]NodeConfig, error) {
		return []NodeConfig{{ID: "dbox", Name: "Box", Transport: transportSSH, SSHHost: "h", SSHUser: "u", KeyPath: path}}, nil
	})
	list := decode[struct{ Detected []detectedKey }](t, h.do("GET", "/api/keys", ""))
	if len(list.Detected) != 1 || !list.Detected[0].Encrypted || list.Detected[0].Imported || len(list.Detected[0].UsedBy) != 1 {
		t.Fatalf("detected = %+v", list.Detected)
	}
	body := `{"path":` + strconv.Quote(path) + `}`
	if rec := h.do("POST", "/api/keys/import", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), "密码短语") {
		t.Fatalf("import without passphrase: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do("POST", "/api/keys/import", `{"path":"/etc/passwd"}`); rec.Code != 400 {
		t.Fatalf("imported outside ~/.ssh: %d", rec.Code)
	}
	body = `{"path":` + strconv.Quote(path) + `,"passphrase":"open sesame"}`
	rec := h.do("POST", "/api/keys/import", body)
	if rec.Code != 201 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	k := decode[struct{ Key vaultKey }](t, rec).Key
	if k.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("fingerprint %s", k.Fingerprint)
	}
	// Stored decrypted, and the device now uses the vault copy.
	b, _ := h.st.privateKey(k.ID)
	if _, err := ssh.ParsePrivateKey(b); err != nil {
		t.Fatalf("stored key: %v", err)
	}
	if n, _ := h.srv.device("dbox"); n.KeyID != k.ID || n.KeyPath != "" {
		t.Fatalf("device = %+v", n)
	}
}

func TestVaultBundleRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mac := newTestHub(t, testConfig(t), "")
	key := decode[struct{ Key vaultKey }](t, mac.do("POST", "/api/keys/generate", `{"name":"mac"}`)).Key
	_ = mac.srv.saveNodes(func([]NodeConfig) ([]NodeConfig, error) {
		return []NodeConfig{
			{ID: "dbox", Name: "Box", Transport: transportSSH, SSHHost: "h", SSHUser: "root", KeyID: key.ID,
				HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", Token: "node-tok", RemotePort: 8899},
			{ID: "dagent", Name: "Agent box", Transport: transportSSH, SSHHost: "h2", SSHUser: "root", UseAgent: true},
			{ID: "dskip", Name: "Not chosen", Transport: transportSSH, SSHHost: "h3", SSHUser: "root", KeyID: key.ID},
		}, nil
	})
	rec := mac.do("POST", "/api/vault/export", `{"devices":["dbox","dagent"]}`)
	if rec.Code != 200 {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	ex := decode[struct {
		Code, Bundle, Filename string
		Warnings               []string
	}](t, rec)
	if len(normalizeCode(ex.Code)) != 16 || len(ex.Warnings) != 1 || !strings.HasSuffix(ex.Filename, ".acbundle") {
		t.Fatalf("export = %+v", ex)
	}
	// Nothing sensitive in the clear.
	for _, secret := range []string{"node-tok", "PRIVATE KEY", "203.0.113", "Not chosen"} {
		if strings.Contains(ex.Bundle, secret) {
			t.Fatalf("bundle leaks %q", secret)
		}
	}

	phone := newTestHub(t, testConfig(t), "")
	imp := func(code string) *httptest.ResponseRecorder {
		return phone.do("POST", "/api/vault/import", `{"bundle":`+strconv.Quote(ex.Bundle)+`,"code":`+strconv.Quote(code)+`}`)
	}
	if rec := imp("2222-3333-4444-5555"); rec.Code != 400 {
		t.Fatalf("wrong code accepted: %d", rec.Code)
	}
	// Codes are forgiving about case and separators.
	rec = imp(strings.ToLower(strings.ReplaceAll(ex.Code, "-", " ")))
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	if r := decode[struct{ Added, Keys int }](t, rec); r.Added != 2 || r.Keys != 1 {
		t.Fatalf("import result %+v", r)
	}
	box, ok := phone.srv.device("dbox")
	if !ok || box.Token != "node-tok" || box.HostKey == "" || box.KeyID == "" || box.RemotePort != 8899 {
		t.Fatalf("imported device = %+v", box)
	}
	want, _ := mac.st.privateKey(key.ID)
	if got, _ := phone.st.privateKey(box.KeyID); string(got) != string(want) {
		t.Fatal("private key differs after import")
	}
	if a, _ := phone.srv.device("dagent"); a.KeyID != "" || a.UseAgent {
		t.Fatalf("agent device = %+v", a)
	}
	// Importing again updates in place: no duplicates.
	if r := decode[struct{ Added, Updated, Keys int }](t, imp(ex.Code)); r.Added != 0 || r.Updated != 2 || r.Keys != 0 {
		t.Fatalf("re-import %+v", r)
	}
	if n := len(phone.srv.config().Nodes); n != 2 {
		t.Fatalf("devices after re-import = %d", n)
	}
}

func TestInstallKeyWithPasswordOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	testPassword = "server-pw"
	defer func() { testPassword = "" }()
	sshd := newTestSSHServer(t, nil) // knows no keys yet

	h := newTestHub(t, testConfig(t), "")
	key := decode[struct{ Key vaultKey }](t, h.do("POST", "/api/keys/generate", `{"name":"phone"}`)).Key
	n := sshNodeFor(t, sshd, "", "http://127.0.0.1:9")
	n.KeyPath, n.KeyID = "", key.ID
	_ = h.srv.saveNodes(func([]NodeConfig) ([]NodeConfig, error) { return []NodeConfig{n}, nil })

	// The password is never sent before the host key is pinned.
	if rec := h.do("POST", "/api/devices/box/install-key", `{"password":"server-pw"}`); rec.Code != 400 {
		t.Fatalf("install before trust: %d", rec.Code)
	}
	probe := decode[struct{ Hostkey HostKeyProbe }](t, h.do("POST", "/api/devices/box/test", "{}"))
	h.do("POST", "/api/devices/box/trust", `{"key":`+strconv.Quote(probe.Hostkey.Key)+`}`)

	rec := h.do("POST", "/api/devices/box/test", "{}")
	auth := decode[struct{ Stage, Public string }](t, rec)
	if auth.Stage != "auth" || auth.Public != key.Public {
		t.Fatalf("test before install = %s", rec.Body.String())
	}
	if rec := h.do("POST", "/api/devices/box/install-key", `{"password":"wrong"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "密码不正确") {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 2; i++ { // idempotent
		if rec := h.do("POST", "/api/devices/box/install-key", `{"password":"server-pw"}`); rec.Code != 200 {
			t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
		}
	}
	ak, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if strings.Count(string(ak), "ssh-ed25519 ") != 1 {
		t.Fatalf("authorized_keys = %q", ak)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys mode %v", fi.Mode().Perm())
	}
	if rec := h.do("POST", "/api/devices/box/test", "{}"); !strings.Contains(rec.Body.String(), `"stage":"ready"`) {
		t.Fatalf("test after install = %s", rec.Body.String())
	}
}

func TestFastNodeListAndSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newTestHub(t, testConfig(t), "")
	_ = h.srv.saveNodes(func([]NodeConfig) ([]NodeConfig, error) {
		return []NodeConfig{{ID: "dfar", Name: "Far", Transport: transportSSH, SSHHost: "203.0.113.1", SSHUser: "root", UseAgent: true}}, nil
	})
	// The fast listing never dials: an unreachable address answers at once.
	rec := h.do("GET", "/api/nodes?fast=1", "")
	r := decode[struct{ Nodes []nodeView }](t, rec)
	if len(r.Nodes) != 2 || !r.Nodes[1].Checking || r.Nodes[1].Target != "root@203.0.113.1" {
		t.Fatalf("fast nodes = %s", rec.Body.String())
	}
	if rec := h.do("GET", "/api/nodes/dfar/snapshot", ""); rec.Body.String() != "null" {
		t.Fatalf("empty snapshot = %q", rec.Body.String())
	}
	if rec := h.do("PUT", "/api/nodes/dfar/snapshot", `{"sessions":[{"id":"a"}]}`); rec.Code != 200 {
		t.Fatalf("put: %d", rec.Code)
	}
	if rec := h.do("GET", "/api/nodes/dfar/snapshot", ""); !strings.Contains(rec.Body.String(), `"id":"a"`) {
		t.Fatalf("snapshot = %q", rec.Body.String())
	}
	if rec := h.do("PUT", "/api/nodes/nope/snapshot", `{}`); rec.Code != 404 {
		t.Fatalf("unknown device: %d", rec.Code)
	}
	// The device in use is remembered by the hub.
	if rec := h.do("POST", "/api/nodes/dfar/use", `{}`); rec.Code != 200 {
		t.Fatalf("use: %d", rec.Code)
	}
	if r := decode[struct{ Current string }](t, h.do("GET", "/api/nodes?fast=1", "")); r.Current != "dfar" {
		t.Fatalf("current = %q", r.Current)
	}
	if rec := h.do("POST", "/api/nodes/nope/use", `{}`); rec.Code != 404 {
		t.Fatalf("use unknown: %d", rec.Code)
	}
}
