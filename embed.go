package console

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"rymesh/internal/config"
	"rymesh/internal/store"
)

type Embedded struct {
	srv   *Server
	store *store.Store
	http  *http.Server
	port  int
	logf  *os.File
}

func embeddedConfig(dataDir string) (*config.Config, error) {
	c, err := config.NewConfig()
	if err != nil {
		return nil, err
	}
	c.ListenAddrs = []string{"127.0.0.1:0"}
	c.EasyTierIface = ""
	c.DataDir = filepath.Join(dataDir, "data")
	c.LogDir = filepath.Join(dataDir, "log")
	c.DefaultCWD = dataDir
	c.AllowedRoots = []string{dataDir}
	c.ImportOnStart = false
	c.NodeName = "手机"
	c.HubOnly = true
	return c, nil
}

// StartEmbedded starts (or reopens) the console stored under dataDir.
//
// vaultKey (32 bytes, kept by the app in the Android Keystore) seals the SSH
// keys in the database; without one a key file in dataDir is used.
func StartEmbedded(dataDir string, vaultKey []byte) (*Embedded, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	cfg, err := config.LoadConfig(cfgPath)
	if os.IsNotExist(err) {
		if cfg, err = embeddedConfig(dataDir); err == nil {
			err = config.SaveConfig(cfgPath, cfg)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// App storage belongs to the current package. A restored Android backup
	// must not keep absolute paths into the previous package's private files.
	cfg.DataDir = filepath.Join(dataDir, "data")
	cfg.LogDir = filepath.Join(dataDir, "log")
	cfg.DefaultCWD = dataDir
	cfg.AllowedRoots = []string{dataDir}
	cfg.HubOnly, cfg.ImportOnStart = true, false
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(cfg.LogDir, "rymesh.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var w io.Writer = io.Discard
	if err == nil {
		w = logf
	}
	logger := log.New(w, "", log.LstdFlags|log.Lmicroseconds)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st.SetVaultKey(vaultKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		st.Close()
		return nil, err
	}
	srv := newServer(cfg, cfgPath, st, logger, []string{ln.Addr().String()})
	srv.warmNodes()
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 180 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	return &Embedded{srv: srv, store: st, http: hs, port: ln.Addr().(*net.TCPAddr).Port, logf: logf}, nil
}

func (e *Embedded) Port() int { return e.port }

// LoginURL opens the console logged in; each URL works once.
func (e *Embedded) LoginURL() string {
	e.srv.ticketMu.Lock()
	t := e.srv.ticket
	e.srv.ticketMu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/api/login/ticket?t=%s", e.port, t)
}

func (e *Embedded) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = e.http.Shutdown(ctx)
	e.store.Close()
	if e.logf != nil {
		e.logf.Close()
	}
}
