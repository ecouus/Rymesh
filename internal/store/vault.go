package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"rymesh/internal/config"
	"rymesh/internal/util"
)

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

var vaultSchema = []string{`CREATE TABLE IF NOT EXISTS ssh_keys (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	type        TEXT NOT NULL,
	fingerprint TEXT NOT NULL UNIQUE,
	public      TEXT NOT NULL,
	private     BLOB NOT NULL,
	source      TEXT NOT NULL,
	created_at  INTEGER NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS devices (
	id   TEXT PRIMARY KEY,
	pos  INTEGER NOT NULL,
	data TEXT NOT NULL
)`}

// VaultKey is a stored key as the page sees it: never the private part.
type VaultKey struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Fingerprint string   `json:"fingerprint"`
	Public      string   `json:"public"`
	Source      string   `json:"source"` // generated | imported
	Created     int64    `json:"created"`
	UsedBy      []string `json:"used_by"`
}

func (s *Store) PrivateKey(id string) ([]byte, error) {
	var b []byte
	err := s.DB.QueryRow(`SELECT private FROM ssh_keys WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("密钥已被删除，请为这台设备重新选择密钥")
	}
	if err != nil {
		return nil, err
	}
	return s.openPrivate(b)
}

func (s *Store) ListKeys() ([]VaultKey, error) {
	rows, err := s.DB.Query(`SELECT id, name, type, fingerprint, public, source, created_at FROM ssh_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VaultKey{}
	for rows.Next() {
		var k VaultKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Type, &k.Fingerprint, &k.Public, &k.Source, &k.Created); err != nil {
			return nil, err
		}
		k.UsedBy = []string{}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) keyByFingerprint(fp string) (VaultKey, bool) {
	var k VaultKey
	err := s.DB.QueryRow(`SELECT id, name, type, fingerprint, public, source, created_at FROM ssh_keys WHERE fingerprint=?`, fp).
		Scan(&k.ID, &k.Name, &k.Type, &k.Fingerprint, &k.Public, &k.Source, &k.Created)
	return k, err == nil
}

func (s *Store) KeyNameTaken(name string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM ssh_keys WHERE name=?`, name).Scan(&n)
	return n > 0
}

// AddKey stores a private key (unencrypted OpenSSH PEM, sealed in the
// database by vaultseal.go). A key already in
// the vault (same fingerprint) is returned as is, with created false.
func (s *Store) AddKey(name, source string, privPEM []byte) (k VaultKey, created bool, err error) {
	signer, err := ssh.ParsePrivateKey(privPEM)
	if err != nil {
		return VaultKey{}, false, fmt.Errorf("解析私钥失败：%w", err)
	}
	pub := signer.PublicKey()
	fp := ssh.FingerprintSHA256(pub)
	if k, ok := s.keyByFingerprint(fp); ok {
		return k, false, nil
	}
	sealed, err := s.sealPrivate(privPEM)
	if err != nil {
		return VaultKey{}, false, err
	}
	name = s.uniqueKeyName(name)
	k = VaultKey{ID: "k" + strings.ToLower(util.RandomID(7)), Name: name, Type: pub.Type(), Fingerprint: fp,
		Public: AuthorizedLine(pub, name), Source: source, Created: time.Now().Unix(), UsedBy: []string{}}
	_, err = s.DB.Exec(`INSERT INTO ssh_keys (id, name, type, fingerprint, public, private, source, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		k.ID, k.Name, k.Type, k.Fingerprint, k.Public, sealed, k.Source, k.Created)
	return k, err == nil, err
}

func (s *Store) uniqueKeyName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "key"
	}
	base := name
	for i := 2; s.KeyNameTaken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func (s *Store) RenameKey(id, name string) error {
	if s.KeyNameTaken(name) {
		return errors.New("已有同名密钥")
	}
	var pub string
	if err := s.DB.QueryRow(`SELECT public FROM ssh_keys WHERE id=?`, id).Scan(&pub); err != nil {
		return errors.New("密钥不存在")
	}
	if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub)); err == nil {
		pub = AuthorizedLine(k, name)
	}
	_, err := s.DB.Exec(`UPDATE ssh_keys SET name=?, public=? WHERE id=?`, name, pub, id)
	return err
}

func (s *Store) DeleteKey(id string) error {
	res, err := s.DB.Exec(`DELETE FROM ssh_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("密钥不存在")
	}
	return nil
}

func (s *Store) LoadDevices() ([]config.NodeConfig, error) {
	rows, err := s.DB.Query(`SELECT data FROM devices ORDER BY pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.NodeConfig
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var n config.NodeConfig
		if err := json.Unmarshal([]byte(data), &n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SaveDevices replaces the stored device list.
func (s *Store) SaveDevices(nodes []config.NodeConfig) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM devices`); err != nil {
		return err
	}
	for i, n := range nodes {
		b, _ := json.Marshal(n)
		if _, err := tx.Exec(`INSERT INTO devices (id, pos, data) VALUES (?,?,?)`, n.ID, i, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuthorizedLine is the authorized_keys line for a key, with a comment that
// tells the server's owner which client it belongs to.
func AuthorizedLine(k ssh.PublicKey, name string) string {
	comment := "rymesh"
	name = strings.TrimPrefix(strings.TrimPrefix(name, comment), "-")
	if c := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return '-'
		}
		return r
	}, name); c != "" {
		comment += "-" + c
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}
