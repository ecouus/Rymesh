package console

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyDirectoryMergesStagedBinary(t *testing.T) {
	root := t.TempDir()
	old, next := filepath.Join(root, ".agentconsole"), filepath.Join(root, ".rymesh")
	for _, p := range []string{filepath.Join(old, "data"), filepath.Join(old, "bin"), filepath.Join(next, "bin")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{filepath.Join(old, "config.json"): "existing config", filepath.Join(old, "data", "events.jsonl"): "existing events", filepath.Join(old, "bin", "agentconsole"): "old binary", filepath.Join(next, "bin", "rymesh"): "uploaded binary"} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := moveLegacyDir(old, next)
	if err != nil || !moved {
		t.Fatalf("move: %v %v", moved, err)
	}
	for p, want := range map[string]string{filepath.Join(next, "config.json"): "existing config", filepath.Join(old, "data", "events.jsonl"): "existing events", filepath.Join(next, "bin", "rymesh"): "uploaded binary"} {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want {
			t.Fatalf("preserve %s: %q %v", p, b, err)
		}
	}
	if link, err := os.Readlink(old); err != nil || link != next {
		t.Fatalf("compatibility link: %s %v", link, err)
	}
	if moved, err := moveLegacyDir(old, next); err != nil || moved {
		t.Fatalf("repeat: %v %v", moved, err)
	}
}

func TestLegacyDirectoryConflictDoesNotMoveFiles(t *testing.T) {
	root := t.TempDir()
	old, next := filepath.Join(root, "old"), filepath.Join(root, "new")
	for _, p := range []string{old, next} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{filepath.Join(old, "a-first"): "keep", filepath.Join(old, "config.json"): "old", filepath.Join(next, "config.json"): "new"} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := moveLegacyDir(old, next); err == nil {
		t.Fatal("conflicting data accepted")
	}
	if b, err := os.ReadFile(filepath.Join(old, "a-first")); err != nil || string(b) != "keep" {
		t.Fatalf("partially moved: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(next, "config.json")); string(b) != "new" {
		t.Fatal("existing config overwritten")
	}
}

func TestLegacyDatabasePreservesVaultAndSessions(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{Agent: "claude", Cwd: dir, Title: "Keep my conversation"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("migration-marker", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := st.saveDevices([]NodeConfig{{ID: "keep-device", Name: "Original device", Token: "unchanged-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	oldDB := filepath.Join(dir, legacyName+".db")
	if err := os.Rename(filepath.Join(dir, "rymesh.db"), oldDB); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyFiles(dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDB); !os.IsNotExist(err) {
		t.Fatalf("old database: %v", err)
	}
	st, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.GetSession(sess.ID); err != nil || got.Title != sess.Title {
		t.Fatalf("session: %+v %v", got, err)
	}
	if got := st.GetMeta("migration-marker"); got != "keep" {
		t.Fatalf("meta: %s", got)
	}
	devices, err := st.loadDevices()
	if err != nil || len(devices) != 1 || devices[0].Token != "unchanged-token" {
		t.Fatalf("devices: %+v %v", devices, err)
	}
	if err := migrateLegacyFiles(dir, ""); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyBundleImport(t *testing.T) {
	code := "2222-3333-4444-5555"
	payload := bundlePayload{Devices: []NodeConfig{{ID: "old-device", Name: "Old phone", Token: "preserved"}}, Keys: []bundleKey{{ID: "old-key", Private: "preserved-key"}}}
	// Construct an actual old bundle, including its authenticated format marker.
	data, err := sealBundle(payload, code, "old phone")
	if err != nil {
		t.Fatal(err)
	}
	var f bundleFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	salt, _ := base64.StdEncoding.DecodeString(f.Salt)
	nonce, _ := base64.StdEncoding.DecodeString(f.Nonce)
	aead, err := bundleAEAD(code, salt, f.N)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := base64.StdEncoding.DecodeString(f.Data)
	plain, err := aead.Open(nil, nonce, sealed, []byte(f.Format))
	if err != nil {
		t.Fatal(err)
	}
	f.Format = legacyName + "-bundle"
	f.Data = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, []byte(f.Format)))
	data, _ = json.Marshal(f)
	got, err := openBundle(data, code)
	if err != nil || len(got.Devices) != 1 || got.Devices[0].Token != "preserved" || got.Keys[0].Private != "preserved-key" {
		t.Fatalf("old bundle: %+v %v", got, err)
	}
	if _, err := openBundle(data, "3333-4444-5555-6666"); err == nil {
		t.Fatal("wrong code accepted")
	}
	f.Format = "untrusted-bundle"
	data, _ = json.Marshal(f)
	if _, err := openBundle(data, code); err == nil || !strings.Contains(err.Error(), "不是") {
		t.Fatal("unknown format accepted")
	}
}

func TestEmbeddedRestoreUsesCurrentPackageStorage(t *testing.T) {
	root := t.TempDir()
	oldPackage := filepath.Join(root, "old-package", "files")
	current := filepath.Join(root, "new-package", "files")
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := embeddedConfig(oldPackage)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(filepath.Join(current, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(current, "data")
	st, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta("restore-marker", "existing phone data"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(data, "rymesh.db"), filepath.Join(data, legacyName+".db")); err != nil {
		t.Fatal(err)
	}
	embedded, err := StartEmbedded(current)
	if err != nil {
		t.Fatal(err)
	}
	defer embedded.Stop()
	if embedded.store.GetMeta("restore-marker") != "existing phone data" {
		t.Fatal("restored data lost")
	}
	saved, err := loadConfig(filepath.Join(current, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.DataDir != data || saved.LogDir != filepath.Join(current, "log") || saved.DefaultCWD != current {
		t.Fatal("restored config kept old package paths")
	}
	if _, err := os.Stat(oldPackage); !os.IsNotExist(err) {
		t.Fatalf("old package directory used: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "rymesh.db")); err != nil {
		t.Fatal(err)
	}
}
