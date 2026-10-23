package store

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPEM(t *testing.T) []byte {
	t.Helper()
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(sk, "test")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func TestVaultKeysAreSealedAtRest(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	priv := testPEM(t)
	k, _, err := st.AddKey("box", "imported", priv)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	_ = st.DB.QueryRow(`SELECT private FROM ssh_keys WHERE id=?`, k.ID).Scan(&raw)
	if !bytes.HasPrefix(raw, sealPrefix) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatalf("stored in the clear: %q", raw[:40])
	}
	if got, err := st.PrivateKey(k.ID); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("privateKey = %v", err)
	}
	st.Close()

	// The key file persists: a reopened store still reads the key.
	st, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.PrivateKey(k.ID); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("after reopen: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "vault.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("vault.key: %v %v", fi, err)
	}
}

func TestVaultSealsPlainRowsFromOlderVersions(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	priv := testPEM(t)
	_, err = st.DB.Exec(`INSERT INTO ssh_keys (id, name, type, fingerprint, public, private, source, created_at) VALUES ('k1','old','ssh-ed25519','fp','pub',?,'generated',0)`, priv)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.PrivateKey("k1"); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("plain row unreadable: %v", err)
	}
	if err := st.SealStoredKeys(); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	_ = st.DB.QueryRow(`SELECT private FROM ssh_keys WHERE id='k1'`).Scan(&raw)
	if !bytes.HasPrefix(raw, sealPrefix) {
		t.Fatal("row not sealed")
	}
	if got, err := st.PrivateKey("k1"); err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("sealed row: %v", err)
	}
}

func TestVaultKeyFromApp(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := bytes.Repeat([]byte{7}, 32)
	st.SetVaultKey(key)
	k, _, err := st.AddKey("box", "imported", testPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(st.path), "vault.key")); !os.IsNotExist(err) {
		t.Fatal("a key file was written although the app gave the key")
	}
	st.SetVaultKey(bytes.Repeat([]byte{8}, 32))
	if _, err := st.PrivateKey(k.ID); err == nil {
		t.Fatal("opened with the wrong key")
	}
}
