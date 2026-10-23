package console

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"rymesh/internal/config"
	"rymesh/internal/sshconn"
	"rymesh/internal/store"
)

// testHub is a client console with an authenticated request helper.
type testHub struct {
	srv *Server
	st  *store.Store
	do  func(method, path, body string) *httptest.ResponseRecorder
}

func newTestHub(t *testing.T, cfg *config.Config, cfgPath string) *testHub {
	t.Helper()
	st, err := store.OpenStore(cfg.DataDir)
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
	cfg.Nodes = []config.NodeConfig{
		{ID: "dbox", Name: "Box", Transport: config.TransportSSH, SSHHost: "h", SSHUser: "root", KeyPath: filepath.Join(keyDir, "phone"), Token: "tok"},
		{ID: "mac", Name: "Mac", URL: "http://10.0.0.2:8899", Token: "t2"},
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	h := newTestHub(t, cfg, cfgPath)

	nodes := h.srv.config().Nodes
	if len(nodes) != 2 || nodes[0].KeyID == "" || nodes[0].KeyPath != "" || nodes[1].ID != "mac" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if b, err := h.st.PrivateKey(nodes[0].KeyID); err != nil || string(b) != string(priv) {
		t.Fatalf("key not moved: %v", err)
	}
	if raw, _ := config.LoadConfig(cfgPath); len(raw.Nodes) != 0 {
		t.Fatalf("config.json still lists devices")
	}
	if _, err := os.Stat(cfgPath + ".pre-vault"); err != nil {
		t.Fatal("no backup of config.json")
	}
	if _, err := os.Stat(keyDir); !os.IsNotExist(err) {
		t.Fatal("key files left in place")
	}
	if fi, _ := os.Stat(filepath.Join(cfg.DataDir, "rymesh.db")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v", fi.Mode().Perm())
	}
	// A second start reads the same list from the database alone.
	cfg2, _ := config.LoadConfig(cfgPath)
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
	type keyResp struct{ Key store.VaultKey }

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
	list := decode[struct{ Keys []store.VaultKey }](t, h.do("GET", "/api/keys", ""))
	if len(list.Keys) != 2 || list.Keys[0].Name != "工作 Mac" || len(list.Keys[0].UsedBy) != 1 ||
		!strings.HasSuffix(list.Keys[0].Public, " rymesh-工作-Mac") {
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
	_ = h.srv.saveNodes(func([]config.NodeConfig) ([]config.NodeConfig, error) {
		return []config.NodeConfig{{ID: "dbox", Name: "Box", Transport: config.TransportSSH, SSHHost: "h", SSHUser: "u", KeyPath: path}}, nil
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
	k := decode[struct{ Key store.VaultKey }](t, rec).Key
	if k.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("fingerprint %s", k.Fingerprint)
	}
	// Stored decrypted, and the device now uses the vault copy.
	b, _ := h.st.PrivateKey(k.ID)
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
	key := decode[struct{ Key store.VaultKey }](t, mac.do("POST", "/api/keys/generate", `{"name":"mac"}`)).Key
	_ = mac.srv.saveNodes(func([]config.NodeConfig) ([]config.NodeConfig, error) {
		return []config.NodeConfig{
			{ID: "dbox", Name: "Box", Transport: config.TransportSSH, SSHHost: "h", SSHUser: "root", KeyID: key.ID,
				HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", Token: "node-tok", RemotePort: 8899},
			{ID: "dagent", Name: "Agent box", Transport: config.TransportSSH, SSHHost: "h2", SSHUser: "root", UseAgent: true},
			{ID: "dskip", Name: "Not chosen", Transport: config.TransportSSH, SSHHost: "h3", SSHUser: "root", KeyID: key.ID},
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
	if len(normalizeCode(ex.Code)) != 16 || len(ex.Warnings) != 1 || !strings.HasSuffix(ex.Filename, ".rymesh") {
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
	want, _ := mac.st.PrivateKey(key.ID)
	if got, _ := phone.st.PrivateKey(box.KeyID); string(got) != string(want) {
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
	key := decode[struct{ Key store.VaultKey }](t, h.do("POST", "/api/keys/generate", `{"name":"phone"}`)).Key
	n := sshNodeFor(t, sshd, "", "http://127.0.0.1:9")
	n.KeyPath, n.KeyID = "", key.ID
	_ = h.srv.saveNodes(func([]config.NodeConfig) ([]config.NodeConfig, error) { return []config.NodeConfig{n}, nil })

	// The password is never sent before the host key is pinned.
	if rec := h.do("POST", "/api/devices/box/install-key", `{"password":"server-pw"}`); rec.Code != 400 {
		t.Fatalf("install before trust: %d", rec.Code)
	}
	probe := decode[struct{ Hostkey sshconn.HostKeyProbe }](t, h.do("POST", "/api/devices/box/test", "{}"))
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
	_ = h.srv.saveNodes(func([]config.NodeConfig) ([]config.NodeConfig, error) {
		return []config.NodeConfig{{ID: "dfar", Name: "Far", Transport: config.TransportSSH, SSHHost: "203.0.113.1", SSHUser: "root", UseAgent: true}}, nil
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

// A server that only takes a password: the device keeps it sealed, never
// shows it, logs in with it, asks again when it is wrong, and forgets it with
// the device.
func TestPasswordOnlyDevice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	testPassword = "server-pw"
	defer func() { testPassword = "" }()
	sshd := newTestSSHServer(t, nil) // takes no keys
	h := newTestHub(t, testConfig(t), "")
	host, port, _ := net.SplitHostPort(sshd.addr)

	if rec := h.do("POST", "/api/devices", `{"transport":"ssh","ssh_host":"`+host+`","ssh_port":`+port+`,"ssh_user":"me","password_auth":true}`); rec.Code != 400 {
		t.Fatalf("password device without a password: %d", rec.Code)
	}
	rec := h.do("POST", "/api/devices", `{"transport":"ssh","name":"Pw","ssh_host":"`+host+`","ssh_port":`+port+`,"ssh_user":"me","password":"server-pw"}`)
	if rec.Code != 201 || strings.Contains(rec.Body.String(), "server-pw") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	dev := decode[struct{ Device deviceView }](t, rec).Device
	if !dev.PasswordAuth {
		t.Fatalf("device = %+v", dev)
	}
	path := "/api/devices/" + dev.ID
	// Sealed at rest, and not in the device rows.
	var sealed []byte
	_ = h.st.DB.QueryRow(`SELECT sealed FROM device_passwords WHERE device_id=?`, dev.ID).Scan(&sealed)
	var rows string
	_ = h.st.DB.QueryRow(`SELECT group_concat(data) FROM devices`).Scan(&rows)
	if len(sealed) == 0 || strings.Contains(string(sealed), "server-pw") || strings.Contains(rows, "server-pw") {
		t.Fatalf("password stored in the clear: sealed=%q rows=%q", sealed, rows)
	}
	if list := h.do("GET", "/api/devices", ""); strings.Contains(list.Body.String(), "server-pw") {
		t.Fatal("device list shows the password")
	}

	probe := decode[struct{ Hostkey sshconn.HostKeyProbe }](t, h.do("POST", path+"/test", "{}"))
	h.do("POST", path+"/trust", `{"key":`+strconv.Quote(probe.Hostkey.Key)+`}`)
	if rec := h.do("POST", path+"/test", "{}"); !strings.Contains(rec.Body.String(), `"stage":"ready"`) {
		t.Fatalf("test with the password = %s", rec.Body.String())
	}

	h.do("PATCH", path, `{"password":"wrong"}`)
	rec = h.do("POST", path+"/test", "{}")
	if r := decode[struct {
		Stage, Error string
		PasswordAuth bool `json:"password_auth"`
	}](t, rec); r.Stage != "auth" || !r.PasswordAuth || !strings.Contains(r.Error, "密码") {
		t.Fatalf("test with a wrong password = %s", rec.Body.String())
	}
	h.do("PATCH", path, `{"password":"server-pw"}`)
	if rec := h.do("POST", path+"/test", "{}"); !strings.Contains(rec.Body.String(), `"stage":"ready"`) {
		t.Fatalf("test after correcting = %s", rec.Body.String())
	}

	h.do("DELETE", path, "{}")
	if _, err := h.st.DevicePassword(dev.ID); err == nil {
		t.Fatal("password kept after the device was deleted")
	}
}

// Passwords travel in the sealed bundle (version 2) and in the QR code. The
// QR code is sealed with the pairing code the phone shows: the export screen
// gets only the picture, never a code or the file.
func TestBundleAndQRCarryPasswords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mac := newTestHub(t, testConfig(t), "")
	key := decode[struct{ Key store.VaultKey }](t, mac.do("POST", "/api/keys/generate", `{"name":"mac"}`)).Key
	_ = mac.srv.saveNodes(func([]config.NodeConfig) ([]config.NodeConfig, error) {
		return []config.NodeConfig{
			{ID: "dkey", Name: "Key box", Transport: config.TransportSSH, SSHHost: "h", SSHUser: "root", KeyID: key.ID, Token: "tok-a"},
			{ID: "dpw", Name: "Pw box", Transport: config.TransportSSH, SSHHost: "h2", SSHUser: "root", PasswordAuth: true, Token: "tok-b"},
		}, nil
	})
	if err := mac.st.SetDevicePassword("dpw", "s3cret-pw"); err != nil {
		t.Fatal(err)
	}

	// The file.
	rec := mac.do("POST", "/api/vault/export", `{"devices":["dkey","dpw"]}`)
	ex := decode[struct{ Code, Bundle string }](t, rec)
	if rec.Code != 200 || !strings.Contains(ex.Bundle, `"version": 2`) {
		t.Fatalf("export: %d %.300s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"s3cret-pw", "tok-a", "PRIVATE KEY"} {
		if strings.Contains(ex.Bundle, secret) {
			t.Fatalf("bundle leaks %q", secret)
		}
	}
	phone := newTestHub(t, testConfig(t), "")
	rec = phone.do("POST", "/api/vault/import", `{"bundle":`+strconv.Quote(ex.Bundle)+`,"code":`+strconv.Quote(ex.Code)+`}`)
	if rec.Code != 200 {
		t.Fatalf("import file: %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := phone.srv.device("dpw"); !d.PasswordAuth {
		t.Fatalf("imported device = %+v", d)
	}
	if pw, _ := phone.st.DevicePassword("dpw"); pw != "s3cret-pw" {
		t.Fatalf("imported password = %q", pw)
	}

	// The QR code, sealed with the phone's pairing code.
	const pairing = "WXYZ-2345-6789-ABCD"
	if rec := mac.do("POST", "/api/vault/export", `{"devices":["dkey"],"qr":true}`); rec.Code != 400 {
		t.Fatalf("qr export without a pairing code: %d", rec.Code)
	}
	rec = mac.do("POST", "/api/vault/export", `{"devices":["dkey","dpw"],"qr":true,"code":"`+pairing+`"}`)
	qx := decode[map[string]any](t, rec)
	if svg, _ := qx["qr_svg"].(string); rec.Code != 200 || !strings.HasPrefix(svg, "<svg") {
		t.Fatalf("qr export: %d %.300s", rec.Code, rec.Body.String())
	}
	for _, k := range []string{"code", "bundle"} {
		if _, ok := qx[k]; ok {
			t.Fatalf("qr export returns %q", k)
		}
	}
	for _, secret := range []string{"s3cret-pw", "tok-a", "PRIVATE KEY", pairing} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("qr export response leaks %q", secret)
		}
	}
	// What the phone's scanner reads is the sealed text the picture holds.
	p, err := openBundle([]byte(ex.Bundle), ex.Code)
	if err != nil {
		t.Fatal(err)
	}
	text, err := sealBundleQR(*p, pairing)
	if err != nil || !strings.HasPrefix(text, qrPrefix) || strings.Trim(text[len(qrPrefix):], base45Alphabet) != "" {
		t.Fatalf("qr text %q %v", text, err)
	}
	tablet := newTestHub(t, testConfig(t), "")
	if rec := tablet.do("POST", "/api/vault/import", `{"qr":`+strconv.Quote(text)+`,"code":"2222-3333-4444-5555"}`); rec.Code != 400 {
		t.Fatalf("qr with another pairing code: %d", rec.Code)
	}
	rec = tablet.do("POST", "/api/vault/import", `{"qr":`+strconv.Quote(text)+`,"code":"`+pairing+`"}`)
	if r := decode[struct{ Added, Keys int }](t, rec); rec.Code != 200 || r.Added != 2 || r.Keys != 1 {
		t.Fatalf("import qr: %d %s", rec.Code, rec.Body.String())
	}
	if pw, _ := tablet.st.DevicePassword("dpw"); pw != "s3cret-pw" {
		t.Fatalf("password from the qr = %q", pw)
	}
	want, _ := mac.st.PrivateKey(key.ID)
	if d, _ := tablet.srv.device("dkey"); d.Token != "tok-a" {
		t.Fatalf("device from the qr = %+v", d)
	} else if got, _ := tablet.st.PrivateKey(d.KeyID); string(got) != string(want) {
		t.Fatal("private key differs after the qr import")
	}
}

// A handful of servers fit in one code; too many are refused with a reason
// rather than drawn as a code no camera reads.
func TestQRCapacity(t *testing.T) {
	payload := func(n int) bundlePayload {
		var p bundlePayload
		for i := 0; i < n; i++ {
			b, err := newKeyPair(fmt.Sprintf("k%d", i))
			if err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("b%d", i)
			p.Keys = append(p.Keys, bundleKey{ID: id, Name: id, Private: string(b)})
			p.Devices = append(p.Devices, config.NodeConfig{ID: fmt.Sprintf("d%07d", i), Name: fmt.Sprintf("server-%d", i),
				Transport: config.TransportSSH, SSHHost: "203.0.113.10", SSHPort: 22, SSHUser: "root", KeyID: id, RemotePort: 8899,
				HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", Token: strings.Repeat("t", 43)})
		}
		return p
	}
	for _, c := range []struct {
		n    int
		fits bool
	}{{3, true}, {8, true}, {40, false}} {
		text, err := sealBundleQR(payload(c.n), "2222-3333-4444-5555")
		if err != nil {
			t.Fatal(err)
		}
		_, err = qrSVG(text)
		if (err == nil) != c.fits {
			t.Fatalf("%d devices (%d chars): fits=%v, want %v (%v)", c.n, len(text), err == nil, c.fits, err)
		}
	}
}

func TestBase45(t *testing.T) {
	// RFC 9285 section 4.
	for in, want := range map[string]string{"AB": "BB8", "Hello!!": "%69 VD92EX0", "base-45": "UJCLQE7W581", "ietf!": "QED8WEX0"} {
		if got := base45Encode([]byte(in)); got != want {
			t.Fatalf("encode %q = %q, want %q", in, got, want)
		}
		if got, err := base45Decode(want); err != nil || string(got) != in {
			t.Fatalf("decode %q = %q %v", want, got, err)
		}
	}
	for n := 0; n < 40; n++ {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i * 37)
		}
		if got, err := base45Decode(base45Encode(b)); err != nil || string(got) != string(b) {
			t.Fatalf("round trip of %d bytes: %v", n, err)
		}
	}
	for _, bad := range []string{"A", "GGW", "ZZZZ", "abc"} {
		if _, err := base45Decode(bad); err == nil {
			t.Fatalf("decode %q accepted", bad)
		}
	}
}
