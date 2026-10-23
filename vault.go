package console

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/crypto/ssh"

	"rymesh/internal/config"
	"rymesh/internal/sshconn"
	"rymesh/internal/store"
	"rymesh/internal/util"
)

// newKeyPair makes an ed25519 key and returns it as OpenSSH PEM.
func newKeyPair(comment string) ([]byte, error) {
	_, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// ---------------------------------------------------------------------------
// Startup: devices and keys move from config.json / keys/ into the database
// ---------------------------------------------------------------------------

// initVault loads the device list from the database. Devices still in
// config.json (from before the vault) and keys generated into data/keys are
// moved in first; config.json is backed up before it is rewritten.
func initVault(st *store.Store, cfg *config.Config, cfgPath string) error {
	stored, err := st.LoadDevices()
	if err != nil {
		return err
	}
	moved := false
	if len(cfg.Nodes) > 0 {
		have := map[string]bool{}
		for _, n := range stored {
			have[n.ID] = true
		}
		for _, n := range cfg.Nodes {
			if !have[n.ID] {
				stored = append(stored, n)
			}
		}
		moved = true
	}
	dir := filepath.Join(cfg.DataDir, "keys")
	if pubs, _ := filepath.Glob(filepath.Join(dir, "*.pub")); len(pubs) > 0 {
		for _, pub := range pubs {
			priv := strings.TrimSuffix(pub, ".pub")
			b, err := os.ReadFile(priv)
			if err != nil {
				continue
			}
			k, _, err := st.AddKey(filepath.Base(priv), "generated", b)
			if err != nil {
				continue
			}
			for i := range stored {
				if stored[i].KeyPath == priv {
					stored[i].KeyPath, stored[i].KeyID = "", k.ID
				}
			}
			moved = true
		}
		// The files are now in the database; keep them aside rather than delete.
		_ = os.Rename(dir, dir+".imported-"+time.Now().Format("20060102-150405"))
	}
	if moved {
		if err := st.SaveDevices(stored); err != nil {
			return err
		}
	}
	cfg.Nodes = stored
	if len(cfg.Nodes) > 0 && cfgPath != "" {
		// Only rewrite config.json if it still lists devices.
		if raw, err := config.LoadConfig(cfgPath); err == nil && len(raw.Nodes) > 0 {
			if b, err := os.ReadFile(cfgPath); err == nil {
				_ = os.WriteFile(cfgPath+".pre-vault", b, 0o600)
			}
			return config.SaveConfigFile(cfgPath, cfg)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keys API
// ---------------------------------------------------------------------------

// detectedKey is a key pair found in ~/.ssh, offered for use or import.
type detectedKey struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Public      string   `json:"public"`
	Fingerprint string   `json:"fingerprint"`
	Encrypted   bool     `json:"encrypted"` // needs a passphrase to import
	Imported    bool     `json:"imported"`  // already in the vault
	UsedBy      []string `json:"used_by"`
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	byID := map[string]*store.VaultKey{}
	vaulted := map[string]bool{}
	for i := range keys {
		byID[keys[i].ID] = &keys[i]
		vaulted[keys[i].Fingerprint] = true
	}
	detected := []detectedKey{}
	pubs, _ := filepath.Glob(filepath.Join(util.ExpandHome("~/.ssh"), "*.pub"))
	sort.Strings(pubs)
	for _, pub := range pubs {
		priv := strings.TrimSuffix(pub, ".pub")
		b, err := os.ReadFile(priv)
		if err != nil {
			continue
		}
		d := detectedKey{Name: filepath.Base(priv), Path: priv, UsedBy: []string{}}
		if pb, err := os.ReadFile(pub); err == nil {
			d.Public = strings.TrimSpace(string(pb))
			if k, _, _, _, err := ssh.ParseAuthorizedKey(pb); err == nil {
				d.Fingerprint = ssh.FingerprintSHA256(k)
			}
		}
		if _, err := ssh.ParseRawPrivateKey(b); err != nil {
			var pp *ssh.PassphraseMissingError
			if !errors.As(err, &pp) {
				continue // not a private key
			}
			d.Encrypted = true
		}
		d.Imported = vaulted[d.Fingerprint]
		detected = append(detected, d)
	}
	for _, n := range s.config().Nodes {
		if k := byID[n.KeyID]; k != nil {
			k.UsedBy = append(k.UsedBy, n.Name)
		}
		for i := range detected {
			if n.KeyID == "" && n.KeyPath == detected[i].Path {
				detected[i].UsedBy = append(detected[i].UsedBy, n.Name)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"keys": keys, "detected": detected, "agent": os.Getenv("SSH_AUTH_SOCK") != ""})
}

func validKeyName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 60 || strings.ContainsAny(name, "\r\n\t") {
		return errors.New("密钥名称不能为空，最多 60 个字")
	}
	return nil
}

func (s *Server) handleGenerateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = s.config().DisplayName() + "-" + time.Now().Format("0102")
	} else if s.store.KeyNameTaken(req.Name) {
		writeErr(w, 409, "已有同名密钥")
		return
	}
	if err := validKeyName(req.Name); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	priv, err := newKeyPair("rymesh " + req.Name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	k, _, err := s.store.AddKey(req.Name, "generated", priv)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"key": k})
}

// handleImportKey copies a key from ~/.ssh into the vault (decrypting it with
// the passphrase once), and points devices that used the file at it.
func (s *Server) handleImportKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path       string `json:"path"`
		Passphrase string `json:"passphrase"`
		Name       string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	dir := util.ExpandHome("~/.ssh")
	path := filepath.Clean(util.ExpandHome(req.Path))
	if filepath.Dir(path) != dir {
		writeErr(w, 400, "只能导入 ~/.ssh 里的密钥")
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 400, "读取私钥失败："+err.Error())
		return
	}
	pemBytes, err := plainKeyPEM(b, req.Passphrase)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	if err := validKeyName(name); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	k, _, err := s.store.AddKey(name, "imported", pemBytes)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.saveNodes(func(nodes []config.NodeConfig) ([]config.NodeConfig, error) {
		for i := range nodes {
			if nodes[i].KeyID == "" && filepath.Clean(util.ExpandHome(nodes[i].KeyPath)) == path {
				nodes[i].KeyPath, nodes[i].KeyID = "", k.ID
			}
		}
		return nodes, nil
	})
	writeJSON(w, 201, map[string]any{"key": k})
}

// plainKeyPEM returns the key as unencrypted OpenSSH PEM, decrypting it with
// passphrase if it is protected.
func plainKeyPEM(b []byte, passphrase string) ([]byte, error) {
	raw, err := ssh.ParseRawPrivateKey(b)
	var pp *ssh.PassphraseMissingError
	if errors.As(err, &pp) {
		if passphrase == "" {
			return nil, errPassphraseNeeded
		}
		raw, err = ssh.ParseRawPrivateKeyWithPassphrase(b, []byte(passphrase))
		if err != nil {
			return nil, errors.New("密码短语不正确")
		}
	} else if err != nil {
		return nil, fmt.Errorf("不是可用的私钥：%w", err)
	}
	if k, ok := raw.(*ed25519.PrivateKey); ok {
		raw = *k
	}
	block, err := ssh.MarshalPrivateKey(raw, "")
	if err != nil {
		return nil, fmt.Errorf("不支持的密钥类型：%w", err)
	}
	return pem.EncodeToMemory(block), nil
}

var errPassphraseNeeded = errors.New("这把私钥有密码短语保护，请输入密码短语")

func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validKeyName(req.Name); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.store.RenameKey(r.PathValue("id"), req.Name); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, n := range s.config().Nodes {
		if n.KeyID == id {
			writeErr(w, 409, "设备「"+n.Name+"」正在使用这把密钥，请先为它换一把密钥或删除设备")
			return
		}
	}
	if err := s.store.DeleteKey(id); err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// publicKeyOf returns the authorized_keys line of the key a device uses.
func (s *Server) publicKeyOf(n config.NodeConfig) (string, error) {
	var b []byte
	var err error
	switch {
	case n.KeyID != "":
		var pub string
		if err := s.store.DB.QueryRow(`SELECT public FROM ssh_keys WHERE id=?`, n.KeyID).Scan(&pub); err != nil {
			return "", errors.New("密钥已被删除，请为这台设备重新选择密钥")
		}
		return pub, nil
	case n.KeyPath != "":
		b, err = os.ReadFile(util.ExpandHome(n.KeyPath + ".pub"))
		if err == nil {
			return strings.TrimSpace(string(b)), nil
		}
		b, err = os.ReadFile(util.ExpandHome(n.KeyPath))
	default:
		return "", errors.New("这台设备使用 ssh-agent，无法自动安装公钥")
	}
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return "", fmt.Errorf("解析私钥失败：%w", err)
	}
	return store.AuthorizedLine(signer.PublicKey(), s.config().DisplayName()), nil
}

// handleInstallKey logs in once with the server password and appends the
// device's public key to ~/.ssh/authorized_keys, as ssh-copy-id does. The
// password is used for this one connection only and never stored. The host
// key must already be pinned, so the password only ever goes to that server.
func (s *Server) handleInstallKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	n, ok := s.device(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "设备不存在")
		return
	}
	if n.Transport != config.TransportSSH || n.HostKey == "" {
		writeErr(w, 400, "请先确认服务器的主机指纹")
		return
	}
	if req.Password == "" {
		writeErr(w, 400, "请输入服务器密码")
		return
	}
	pub, err := s.publicKeyOf(n)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := installAuthorizedKey(ctx, n, req.Password, pub); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// installAuthorizedKeyScript reads the key line on stdin and adds it unless
// it is already there; it also fixes the permissions sshd insists on.
const installAuthorizedKeyScript = `umask 077; K=$(cat); mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh" && ` +
	`touch "$HOME/.ssh/authorized_keys" && chmod 600 "$HOME/.ssh/authorized_keys" && ` +
	`{ grep -qxF "$K" "$HOME/.ssh/authorized_keys" || printf '%s\n' "$K" >> "$HOME/.ssh/authorized_keys"; }`

func installAuthorizedKey(ctx context.Context, n config.NodeConfig, password, pub string) error {
	answer := func(_, _ string, qs []string, _ []bool) ([]string, error) {
		out := make([]string, len(qs))
		for i := range qs {
			out[i] = password
		}
		return out, nil
	}
	cfg := &ssh.ClientConfig{
		User:              n.SSHUser,
		Auth:              []ssh.AuthMethod{ssh.Password(password), ssh.KeyboardInteractive(answer)},
		HostKeyCallback:   sshconn.PinnedHostKey(n),
		HostKeyAlgorithms: sshconn.HostKeyAlgorithms(n),
		Timeout:           10 * time.Second,
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", n.SSHAddr())
	if err != nil {
		return fmt.Errorf("无法连接 %s：%w", n.SSHAddr(), err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, n.SSHAddr(), cfg)
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			return errors.New("密码不正确，或服务器不允许密码登录")
		}
		return errors.New(sshErrorText(err))
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(pub + "\n")
	if out, err := sess.CombinedOutput(installAuthorizedKeyScript); err != nil {
		return fmt.Errorf("写入 authorized_keys 失败：%s", strings.TrimSpace(string(out)+" "+err.Error()))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Export / import: an encrypted bundle moves devices (and their keys) from
// one client to another, e.g. from a Mac to a phone.
// ---------------------------------------------------------------------------

const bundleFormat = "rymesh-bundle"

// bundleFile is what is written to disk; everything sensitive is in Data,
// sealed with AES-256-GCM under a key derived from the code by scrypt.
type bundleFile struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	Created string `json:"created"`
	From    string `json:"from"`
	Devices int    `json:"devices"`
	N       int    `json:"scrypt_n"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
}

type bundleKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private string `json:"private"`
}

type bundlePayload struct {
	Devices []config.NodeConfig `json:"devices"`
	Keys    []bundleKey         `json:"keys"`
}

// codeAlphabet has 32 symbols without look-alikes (no 0/O, 1/I).
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newBundleCode is 16 random symbols (80 bits), shown as XXXX-XXXX-XXXX-XXXX.
// It is generated, never chosen, so a copy of the file cannot be opened by
// guessing.
func newBundleCode() (string, error) {
	b, err := util.RandomBytes(16)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return sb.String(), nil
}

func normalizeCode(code string) string {
	code = strings.ToUpper(code)
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(codeAlphabet, r) {
			return r
		}
		return -1
	}, code)
}

// bundleScryptN is the scrypt cost of export files (r=8: about 32 MiB).
const bundleScryptN = 1 << 15

func bundleAEAD(code string, salt []byte, n int) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(normalizeCode(code)), salt, n, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealBundle(p bundlePayload, code, from string) ([]byte, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	salt, err := util.RandomBytes(16)
	if err != nil {
		return nil, err
	}
	const n = bundleScryptN
	aead, err := bundleAEAD(code, salt, n)
	if err != nil {
		return nil, err
	}
	nonce, err := util.RandomBytes(aead.NonceSize())
	if err != nil {
		return nil, err
	}
	f := bundleFile{Format: bundleFormat, Version: 1, Created: time.Now().Format(time.RFC3339), From: from,
		Devices: len(p.Devices), N: n, Salt: base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce)}
	f.Data = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, []byte(f.Format)))
	return json.MarshalIndent(f, "", "  ")
}

func openBundle(data []byte, code string) (*bundlePayload, error) {
	var f bundleFile
	if err := json.Unmarshal(data, &f); err != nil || f.Format != bundleFormat {
		return nil, errors.New("不是 Rymesh 导出的文件")
	}
	if f.Version != 1 {
		return nil, errors.New("文件版本过新，请先升级 Rymesh")
	}
	if len(normalizeCode(code)) != 16 {
		return nil, errors.New("解密码应为 16 位，形如 XXXX-XXXX-XXXX-XXXX")
	}
	// N is read before the code can be checked, and scrypt's memory grows
	// with it (1<<20 is about 1 GiB): a damaged file must not be able to
	// exhaust a phone. Every export has used this one value.
	if f.N != bundleScryptN {
		return nil, errors.New("文件已损坏")
	}
	salt, err1 := base64.StdEncoding.DecodeString(f.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(f.Nonce)
	sealed, err3 := base64.StdEncoding.DecodeString(f.Data)
	// Checked before the costly key derivation, like N.
	if err1 != nil || err2 != nil || err3 != nil || len(salt) != 16 || len(nonce) != 12 {
		return nil, errors.New("文件已损坏")
	}
	aead, err := bundleAEAD(code, salt, f.N)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, sealed, []byte(f.Format))
	if err != nil {
		return nil, errors.New("解密码不正确，或文件已损坏")
	}
	var p bundlePayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("文件已损坏")
	}
	return &p, nil
}

// handleVaultExport seals the chosen devices, their pinned host keys, node tokens
// and private keys into a bundle. Keys referenced from ~/.ssh are read and
// included; a device that authenticates through ssh-agent goes without one.
func (s *Server) handleVaultExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Devices []string `json:"devices"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	want := map[string]bool{}
	for _, id := range req.Devices {
		want[id] = true
	}
	var p bundlePayload
	var warnings []string
	keyIDs := map[string]string{} // vault id or file path -> bundle key id
	for _, n := range s.config().Nodes {
		if !want[n.ID] {
			continue
		}
		var pemBytes []byte
		var name, ref string
		switch {
		case n.KeyID != "":
			ref = n.KeyID
			if b, err := s.store.PrivateKey(n.KeyID); err == nil {
				pemBytes = b
				if keys, err := s.store.ListKeys(); err == nil {
					for _, k := range keys {
						if k.ID == n.KeyID {
							name = k.Name
						}
					}
				}
			}
		case n.KeyPath != "":
			ref = n.KeyPath
			name = filepath.Base(n.KeyPath)
			if b, err := os.ReadFile(util.ExpandHome(n.KeyPath)); err == nil {
				if pb, err := plainKeyPEM(b, ""); err == nil {
					pemBytes = pb
				} else if errors.Is(err, errPassphraseNeeded) {
					warnings = append(warnings, n.Name+"：私钥有密码短语，请先在「密钥」里导入它再导出")
				}
			}
		default:
			warnings = append(warnings, n.Name+"：使用 ssh-agent，导入后需要为它另选密钥")
		}
		out := n
		out.KeyPath, out.KeyID, out.UseAgent = "", "", false
		if pemBytes != nil {
			id, ok := keyIDs[ref]
			if !ok {
				id = fmt.Sprintf("b%d", len(p.Keys)+1)
				keyIDs[ref] = id
				p.Keys = append(p.Keys, bundleKey{ID: id, Name: name, Private: string(pemBytes)})
			}
			out.KeyID = id
		}
		p.Devices = append(p.Devices, out)
	}
	if len(p.Devices) == 0 {
		writeErr(w, 400, "请至少选择一台设备")
		return
	}
	code, err := newBundleCode()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	data, err := sealBundle(p, code, s.config().DisplayName())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, 200, map[string]any{"code": code, "bundle": string(data),
		"filename": "rymesh-" + time.Now().Format("20060102-1504") + ".rymesh", "warnings": warnings})
}

// handleVaultImport opens a bundle and merges it: keys already present are
// reused, a device with the same id (exported from here or imported before)
// is replaced, others are added.
func (s *Server) handleVaultImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bundle string `json:"bundle"`
		Code   string `json:"code"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, err := openBundle([]byte(req.Bundle), req.Code)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	keyMap := map[string]string{}
	newKeys := 0
	for _, k := range p.Keys {
		vk, created, err := s.store.AddKey(k.Name, "imported", []byte(k.Private))
		if err != nil {
			writeErr(w, 400, "导入密钥「"+k.Name+"」失败："+err.Error())
			return
		}
		if created {
			newKeys++
		}
		keyMap[k.ID] = vk.ID
	}
	added, updated := 0, 0
	err = s.saveNodes(func(nodes []config.NodeConfig) ([]config.NodeConfig, error) {
		for _, d := range p.Devices {
			d.KeyID = keyMap[d.KeyID]
			if d.ID == "" || d.ID == config.LocalNodeID {
				d.ID = "d" + strings.ToLower(util.RandomID(7))
			}
			replaced := false
			for i := range nodes {
				if nodes[i].ID == d.ID {
					nodes[i], replaced = d, true
				}
			}
			if replaced {
				updated++
			} else {
				nodes = append(nodes, d)
				added++
			}
		}
		return nodes, nil
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"added": added, "updated": updated, "keys": newKeys})
}
