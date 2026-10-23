package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"rymesh/internal/util"
)

var sealPrefix = []byte("rymesh-sealed-v1:")

// UseKeychain keeps the vault key in the macOS keychain. Tests turn it off so
// they do not leave items in the developer's keychain.
var UseKeychain = runtime.GOOS == "darwin"

const keychainService = "dev.rymesh.vault"

// SetVaultKey fixes the vault key, as the Android app does with one kept in
// its Keystore. Anything but 32 bytes is ignored.
func (s *Store) SetVaultKey(k []byte) {
	if len(k) != 32 {
		return
	}
	s.vaultMu.Lock()
	s.vaultKey = append([]byte(nil), k...)
	s.vaultMu.Unlock()
}

// vaultAEAD returns the cipher for sealed keys, creating the vault key the
// first time. A failure is not remembered: a locked keychain may open later.
func (s *Store) vaultAEAD() (cipher.AEAD, error) {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	if s.vaultKey == nil {
		dir := filepath.Dir(s.path)
		var k []byte
		var err error
		if UseKeychain {
			k, err = keychainVaultKey(dir)
		} else {
			k, err = fileVaultKey(filepath.Join(dir, "vault.key"))
		}
		if err != nil {
			return nil, err
		}
		s.vaultKey = k
	}
	block, err := aes.NewCipher(s.vaultKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Store) sealPrivate(plain []byte) ([]byte, error) {
	aead, err := s.vaultAEAD()
	if err != nil {
		return nil, fmt.Errorf("无法取得密钥库的加密密钥：%w", err)
	}
	nonce, err := util.RandomBytes(aead.NonceSize())
	if err != nil {
		return nil, err
	}
	out := append(append([]byte(nil), sealPrefix...), nonce...)
	return aead.Seal(out, nonce, plain, sealPrefix), nil
}

func (s *Store) openPrivate(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, sealPrefix) {
		return b, nil // stored before sealing
	}
	aead, err := s.vaultAEAD()
	if err != nil {
		return nil, fmt.Errorf("无法取得密钥库的加密密钥：%w", err)
	}
	b = b[len(sealPrefix):]
	if len(b) < aead.NonceSize() {
		return nil, errors.New("保存的私钥已损坏")
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], sealPrefix)
	if err != nil {
		return nil, errors.New("无法解密保存的私钥（密钥库的加密密钥已变化），请重新导入这把密钥")
	}
	return plain, nil
}

// SealStoredKeys seals rows still kept as plain PEM. Nothing is touched (and
// the keychain is not asked) when there are none.
func (s *Store) SealStoredKeys() error {
	rows, err := s.DB.Query(`SELECT id, private FROM ssh_keys`)
	if err != nil {
		return err
	}
	plain := map[string][]byte{}
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			rows.Close()
			return err
		}
		if !bytes.HasPrefix(b, sealPrefix) {
			plain[id] = b
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, b := range plain {
		sealed, err := s.sealPrivate(b)
		if err != nil {
			return err
		}
		if _, err := s.DB.Exec(`UPDATE ssh_keys SET private=? WHERE id=?`, sealed, id); err != nil {
			return err
		}
	}
	// The plain rows may linger in freed pages and the WAL until overwritten.
	if len(plain) > 0 {
		_, _ = s.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		_, _ = s.DB.Exec(`VACUUM`)
	}
	return nil
}

// fileVaultKey reads (or creates) a 32-byte key file readable only by this
// user.
func fileVaultKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s 已损坏", path)
		}
		return k, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	k, err := util.RandomBytes(32)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return fileVaultKey(path) // another process created it meanwhile
	}
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(k) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return k, nil
}

// keychainVaultKey keeps the key in the login keychain as a generic password,
// one per data directory (an isolated test instance gets its own). It goes
// through /usr/bin/security, which the item then trusts, so reading it later
// asks nothing. The secret is passed on stdin, never on a command line.
func keychainVaultKey(dataDir string) ([]byte, error) {
	account, _ := filepath.Abs(dataDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password",
		"-s", keychainService, "-a", account, "-w").Output()
	if err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(out)))
		if err != nil || len(k) != 32 {
			return nil, errors.New("钥匙串中的密钥库密钥已损坏")
		}
		return k, nil
	}
	// 44 is errSecItemNotFound; anything else (a locked keychain, no login
	// session over SSH) must not lead to a new key that orphans the old one.
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 44 {
		return nil, fmt.Errorf("无法读取钥匙串：%v", err)
	}
	k, err := util.RandomBytes(32)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -s %s -a %s -l %s -w %s\n",
		keychainService, securityQuote(account), securityQuote("Rymesh 密钥库"), hex.EncodeToString(k)))
	if out, err := cmd.CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) > 0 {
		return nil, fmt.Errorf("无法写入钥匙串：%v %s", err, strings.TrimSpace(string(out)))
	}
	return k, nil
}

// securityQuote quotes an argument for `security -i`, which splits its input
// lines like a shell does.
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
