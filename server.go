package console

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	cfg       *Config
	cfgPath   string
	store     *Store
	agents    *AgentSet
	ing       *Ingestor
	imp       *Importer
	auth      *authManager
	mux       *http.ServeMux
	logger    *log.Logger
	started   time.Time
	bindAddrs []string
	cfgMu     sync.RWMutex
	// cfgWrite serializes config changes: each one copies the current
	// config, changes it, saves and publishes it, and two of them interleaved
	// would publish one on top of a copy that lacks the other's change.
	cfgWrite  sync.Mutex
	schedMu   sync.Mutex
	launching map[string]bool
	importMu  sync.Mutex
	ssh       *sshPool
	nodeMu    sync.Mutex
	nodeSeen  map[string]nodeView // last probe of each device (nodes.go)
	ticketMu  sync.Mutex
	ticket    string // one-time local login (client.go)
	// tokenRead is when config.json was last read for a node token written
	// by `rymesh node token` while this console runs (nodes.go).
	tokenMu   sync.Mutex
	tokenRead time.Time
	// streamSem bounds concurrent SSE connections. Every stream polls the store
	// several times a second and the store has a single connection, so an
	// unbounded number of open tabs (or a reconnect loop) would starve every
	// other request. The client falls back to polling on 503.
	streamSem chan struct{}
}

func newServer(cfg *Config, cfgPath string, st *Store, logger *log.Logger, bindAddrs []string) *Server {
	// The device list lives in the database (vault.go).
	if err := initVault(st, cfg, cfgPath); err != nil {
		logger.Printf("密钥库初始化失败: %v", err)
	}
	s := &Server{
		cfg: cfg, cfgPath: cfgPath, store: st, agents: newAgentSet(),
		auth: newAuthManager(cfg), mux: http.NewServeMux(), logger: logger,
		started: time.Now(), bindAddrs: bindAddrs, launching: map[string]bool{},
		streamSem: make(chan struct{}, 32), ssh: newSSHPool(st),
	}
	s.ing = newIngestor(st, cfg.DataDir, logger)
	s.ing.onDone = func() { s.schedule() }
	s.imp = newImporter(st, logger, cfg.ImportExcludeRoots, cfg.ImportHistoryLimit)
	s.routes()
	s.rotateTicket()
	return s
}

func (s *Server) config() *Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// publishConfig atomically swaps in a new config. Readers that already hold the
// old pointer keep seeing a consistent snapshot.
func (s *Server) publishConfig(c *Config) {
	s.cfgMu.Lock()
	s.cfg = c
	s.cfgMu.Unlock()
	s.auth.setConfig(c)
}

// updateConfig applies fn to a copy of the current config and publishes it,
// returning the new config for persisting.
func (s *Server) updateConfig(fn func(*Config)) *Config {
	s.cfgWrite.Lock()
	defer s.cfgWrite.Unlock()
	next := s.config().clone()
	fn(next)
	s.publishConfig(next)
	return next
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/session", s.handleAuthState)
	// No password login: the apps and `rymesh client open` sign in with a
	// one-time ticket, and a hub reaches a node with its token.
	m.HandleFunc("GET /api/login/ticket", s.handleTicketLogin)

	m.HandleFunc("GET /api/bootstrap", s.authed(s.handleBootstrap))
	m.HandleFunc("GET /api/agents", s.authed(s.handleAgents))
	m.HandleFunc("POST /api/agents/rescan", s.authed(s.handleAgents))
	m.HandleFunc("GET /api/agents/{name}/commands", s.authed(s.handleAgentCommands))
	m.HandleFunc("GET /api/agents/{name}/models", s.authed(s.handleAgentModels))

	m.HandleFunc("GET /api/sessions", s.authed(s.handleListSessions))
	m.HandleFunc("POST /api/sessions", s.authed(s.handleCreateSession))
	m.HandleFunc("POST /api/sessions/import", s.authed(s.handleImport))
	m.HandleFunc("GET /api/sessions/{id}", s.authed(s.handleGetSession))
	m.HandleFunc("PATCH /api/sessions/{id}", s.authed(s.handlePatchSession))
	m.HandleFunc("DELETE /api/sessions/{id}", s.authed(s.handleDeleteSession))
	m.HandleFunc("GET /api/sessions/{id}/events", s.authed(s.handleEvents))
	m.HandleFunc("GET /api/sessions/{id}/events/{seq}", s.authed(s.handleEvent))
	m.HandleFunc("POST /api/sessions/{id}/messages", s.authed(s.handleCreateMessage))
	m.HandleFunc("GET /api/sessions/{id}/stream", s.authed(s.handleStream))

	m.HandleFunc("GET /api/tasks", s.authed(s.handleListTasks))
	m.HandleFunc("GET /api/tasks/{id}", s.authed(s.handleGetTask))
	m.HandleFunc("POST /api/tasks/{id}/cancel", s.authed(s.handleCancelTask))
	m.HandleFunc("POST /api/tasks/{id}/retry", s.authed(s.handleRetryTask))
	m.HandleFunc("GET /api/tasks/{id}/log", s.authed(s.handleTaskLog))

	m.HandleFunc("GET /api/permissions", s.authed(s.handleListPermissions))
	m.HandleFunc("POST /api/permissions/{id}", s.authed(s.handleResolvePermission))

	m.HandleFunc("GET /api/settings", s.authed(s.handleSettings))
	m.HandleFunc("PATCH /api/settings", s.authed(s.handlePatchSettings))
	m.HandleFunc("POST /api/cwd/check", s.authed(s.handleCwdCheck))

	// Multi-device: list devices, and relay a device's API through this hub.
	m.HandleFunc("GET /api/nodes", s.authed(s.handleNodes))
	m.HandleFunc("GET /api/nodes/self", s.authed(s.handleNodeSelf))
	m.HandleFunc("POST /api/nodes/{id}/use", s.authed(s.handleUseNode))
	m.HandleFunc("GET /api/nodes/{id}/snapshot", s.authed(s.handleNodeSnapshot))
	m.HandleFunc("PUT /api/nodes/{id}/snapshot", s.authed(s.handleNodeSnapshot))
	// Device management (this console's own device list and keys; devices.go).
	m.HandleFunc("GET /api/devices", s.authed(s.handleListDevices))
	m.HandleFunc("POST /api/devices", s.authed(s.handleCreateDevice))
	m.HandleFunc("PATCH /api/devices/{id}", s.authed(s.handlePatchDevice))
	m.HandleFunc("DELETE /api/devices/{id}", s.authed(s.handleDeleteDevice))
	m.HandleFunc("POST /api/devices/{id}/test", s.authed(s.handleDeviceTest))
	m.HandleFunc("POST /api/devices/{id}/trust", s.authed(s.handleDeviceTrust))
	m.HandleFunc("POST /api/devices/{id}/deploy", s.authed(s.handleDeviceDeploy))
	m.HandleFunc("GET /api/ssh/hosts", s.authed(s.handleSSHHosts))
	m.HandleFunc("POST /api/devices/{id}/install-key", s.authed(s.handleInstallKey))
	m.HandleFunc("GET /api/keys", s.authed(s.handleListKeys))
	m.HandleFunc("POST /api/keys/generate", s.authed(s.handleGenerateKey))
	m.HandleFunc("POST /api/keys/import", s.authed(s.handleImportKey))
	m.HandleFunc("PATCH /api/keys/{id}", s.authed(s.handlePatchKey))
	m.HandleFunc("DELETE /api/keys/{id}", s.authed(s.handleDeleteKey))
	m.HandleFunc("POST /api/vault/export", s.authed(s.handleVaultExport))
	m.HandleFunc("POST /api/vault/import", s.authed(s.handleVaultImport))
	m.HandleFunc("/n/{node}/api/{rest...}", s.authed(s.handleNodeProxy))

	// Updates act on the Codex/Claude CLIs, not on this console. "Restart"
	// re-runs the agent's live tasks so they pick up an edited config.
	m.HandleFunc("GET /api/agents/updates", s.authed(s.handleAgentUpdateCheck))
	m.HandleFunc("POST /api/agents/{name}/update", s.authed(s.handleAgentUpdate))
	m.HandleFunc("POST /api/agents/{name}/restart", s.authed(s.handleAgentRestart))

	m.Handle("/", s.staticHandler())
}

func (s *Server) Handler() http.Handler {
	return securityHeaders(gzipMiddleware(s.mux))
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, ok := s.auth.authenticate(r)
		if !ok {
			// A hub calling this node: the bearer token stands in for the
			// cookie, and CSRF does not apply to a non-browser caller.
			if s.nodeTokenOK(r) {
				h(w, r)
				return
			}
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		if !s.auth.checkCSRF(r, info) {
			writeErr(w, http.StatusForbidden, "CSRF 校验失败")
			return
		}
		h(w, r)
	}
}

func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	// Embedded files carry no modification time, so without an ETag every
	// page load re-downloaded the whole app. A content hash lets the browser
	// revalidate with a 304 instead.
	etags := map[string]string{}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(sub, p); err == nil {
				sum := sha256.Sum256(b)
				etags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
			}
		}
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Unknown API paths must 404, not return the SPA shell with 200: a
		// misspelled or removed endpoint would otherwise look like success to
		// the page (which then parses HTML as JSON).
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "接口不存在")
			return
		}
		p := strings.TrimPrefix(filepath.ToSlash(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if p == "manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		if st, err := fs.Stat(sub, p); err == nil && !st.IsDir() {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etags[p])
			fileServer.ServeHTTP(w, r)
			return
		}
		// Single-page app: unknown paths get the shell.
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etags["index.html"])
		if r.Header.Get("If-None-Match") == etags["index.html"] {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(b)
	})
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	if info, ok := s.auth.authenticate(r); ok {
		writeJSON(w, 200, map[string]any{"authenticated": true, "csrf": info.CSRF, "version": version})
		return
	}
	writeJSON(w, 200, map[string]any{"authenticated": false, "version": version})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": version, "build": buildID(),
		"started_at": s.started.UTC().Format(time.RFC3339Nano), "time": nowRFC3339(),
	})
}

// ---------------------------------------------------------------------------
// Bootstrap / agents / settings
// ---------------------------------------------------------------------------

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	agents := s.agents.List(false)
	sessions, _ := s.store.ListSessions("", "", false)
	active, _ := s.store.ListTasks([]string{StatusRunning, StatusWaitingInput, StatusPending}, 100)
	recent, _ := s.store.ListTasks(nil, 50)
	perms, _ := s.store.PendingPermissions("")
	counts, _ := s.store.TaskCounts()
	sources, _ := s.store.SourceCounts()
	csrf := ""
	if info, ok := s.auth.authenticate(r); ok {
		csrf = info.CSRF
	}
	var lastReport ImportReport
	_ = json.Unmarshal([]byte(s.store.GetMeta("last_import_report")), &lastReport)
	writeJSON(w, 200, map[string]any{
		"version": version, "started_at": s.started.UTC().Format(time.RFC3339Nano),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"agents":         agents,
		"sessions":       sessions,
		"profiles": map[string]any{
			"codex": codexProfiles, "claude": claudeProfiles,
		},
		"tasks":         mergeTasks(active, recent, 80),
		"permissions":   perms,
		"counts":        counts,
		"source_counts": sources,
		"csrf":          csrf,
		"node_name":     cfg.nodeName(),
		"import": map[string]any{
			"last_at": s.store.GetMeta("last_import_at"),
			"report":  lastReport,
		},
		"config": map[string]any{
			"default_cwd":            cfg.defaultCWD(),
			"allowed_roots":          cfg.AllowedRoots,
			"max_concurrent":         cfg.MaxConcurrent,
			"data_dir":               cfg.DataDir,
			"log_dir":                cfg.LogDir,
			"port":                   cfg.Port,
			"bind_addrs":             s.bindAddrs,
			"easytier_iface":         cfg.EasyTierIface,
			"easytier_ip":            easytierIP(s.bindAddrs),
			"db_path":                filepath.Join(cfg.DataDir, "rymesh.db"),
			"permission_ttl_minutes": cfg.PermissionTTLMin,
			"session_ttl_hours":      cfg.SessionTTLHours,
			"import_on_start":        cfg.ImportOnStart,
			"import_history_limit":   cfg.ImportHistoryLimit,
			"import_exclude_roots":   cfg.ImportExcludeRoots,
			"task_log_max_bytes":     cfg.TaskLogMaxBytes,
		},
	})
}

func mergeTasks(active, recent []*Task, limit int) []*Task {
	seen := map[string]bool{}
	var out []*Task
	for _, t := range active {
		if !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	for _, t := range recent {
		if !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func easytierIP(addrs []string) string {
	for _, a := range addrs {
		host, _, err := splitHostPort(a)
		if err != nil {
			continue
		}
		ip := parseIP(host)
		if ip != "" && host != "127.0.0.1" && host != "::1" {
			return ip
		}
	}
	return ""
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	force := r.Method == http.MethodPost
	writeJSON(w, 200, map[string]any{"agents": s.agents.List(force)})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	counts, _ := s.store.TaskCounts()
	sources, _ := s.store.SourceCounts()
	writeJSON(w, 200, map[string]any{
		"version": version, "started_at": s.started.UTC().Format(time.RFC3339Nano),
		"agents": s.agents.List(false),
		"config": map[string]any{
			"default_cwd": cfg.defaultCWD(), "allowed_roots": cfg.AllowedRoots,
			"max_concurrent": cfg.MaxConcurrent, "data_dir": cfg.DataDir,
			"log_dir": cfg.LogDir, "port": cfg.Port, "bind_addrs": s.bindAddrs,
			"easytier_iface": cfg.EasyTierIface, "easytier_ip": easytierIP(s.bindAddrs),
			"db_path":                filepath.Join(cfg.DataDir, "rymesh.db"),
			"permission_ttl_minutes": cfg.PermissionTTLMin,
			"session_ttl_hours":      cfg.SessionTTLHours,
			"import_on_start":        cfg.ImportOnStart,
			"import_history_limit":   cfg.ImportHistoryLimit,
			"import_exclude_roots":   cfg.ImportExcludeRoots,
			"task_log_max_bytes":     cfg.TaskLogMaxBytes,
		},
		"counts": counts, "source_counts": sources,
		"status": serviceStatus(),
	})
}

func serviceStatus() map[string]any {
	pid := os.Getpid()
	boot := currentBootID()
	return map[string]any{"pid": pid, "boot_id": boot, "service": "rymesh",
		"unit": "/etc/systemd/system/rymesh.service"}
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultCWD    *string `json:"default_cwd"`
		MaxConcurrent *int    `json:"max_concurrent"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	// config() hands out the shared *Config, which request handlers, the
	// scheduler and the ingestor also read. Copy-on-write keeps those readers
	// race-free while authManager (which holds the same pointer) is updated
	// through publishConfig below.
	s.cfgWrite.Lock()
	defer s.cfgWrite.Unlock()
	cfg := *s.config().clone()
	// `rymesh node token` may have written a token this console has not read
	// yet; saving without it would take it back out of the file.
	if tok := s.fileNodeToken(); tok != "" {
		cfg.NodeToken = tok
	}
	if req.DefaultCWD != nil {
		cwd, err := cfg.allowedCWD(*req.DefaultCWD)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		cfg.DefaultCWD = cwd
	}
	if req.MaxConcurrent != nil {
		n := *req.MaxConcurrent
		if n < 1 || n > 32 {
			writeErr(w, 400, "并发上限必须在 1..32 之间")
			return
		}
		cfg.MaxConcurrent = n
	}
	if err := saveConfigFile(s.cfgPath, &cfg); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	s.publishConfig(&cfg)
	s.schedule()
	writeJSON(w, 200, map[string]any{"ok": true, "config": map[string]any{
		"default_cwd": cfg.DefaultCWD, "max_concurrent": cfg.MaxConcurrent}})
}

// handleAgentUpdateCheck reports the installed and latest published version of
// each agent CLI. The query goes to npm, so it is a network call.
func (s *Server) handleAgentUpdateCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"updates": checkAgentUpdates(s.agents.List(false))})
}

// handleAgentUpdate installs the latest CLI release, then re-detects the agent
// so the page shows the new version. Running tasks keep their old process until
// they finish; new tasks pick up the new binary.
func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := agentCLISpecs[name]; !ok {
		writeErr(w, 404, "未知 Agent")
		return
	}
	if !agentUpdateMu.TryLock() {
		writeErr(w, 409, "正在更新，请稍候")
		return
	}
	defer agentUpdateMu.Unlock()

	info, _ := s.agents.Get(name)
	method, command, output, err := installAgentUpdate(name, info)
	var updated AgentInfo
	for _, a := range s.agents.List(true) {
		if a.Name == name {
			updated = a
		}
	}
	if err != nil {
		writeErr(w, 500, "更新失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "method": method, "command": command,
		"version": agentVersion(updated.Version), "agent": updated,
		"output": tailText(output, 2000),
	})
}

// handleAgentRestart re-detects the agent and restarts its live tasks. There is
// no long-lived agent process to bounce: each task is a new CLI process, and
// only a task that is already running still holds the old config. Those tasks
// are stopped and re-queued with the same prompt, so the replacement processes
// read the edited config/settings. Queued tasks need no restart — they will
// start after the change anyway.
func (s *Server) handleAgentRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := agentCLISpecs[name]; !ok {
		writeErr(w, 404, "未知 Agent")
		return
	}
	// Force a rescan: the point of the button is also to reload the model and
	// auth details the settings page displays.
	s.agents.List(true)
	info, _ := s.agents.Get(name)

	tasks, err := s.store.ListTasks([]string{StatusRunning, StatusWaitingInput}, 0)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var live []*Task
	for _, t := range tasks {
		if t.Agent == name {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "restarted": 0, "agent": info})
		return
	}
	ids := make([]string, 0, len(live))
	for _, t := range live {
		ids = append(ids, t.ID)
	}
	go s.restartAgentTasks(live)
	writeJSON(w, 202, map[string]any{
		"ok": true, "restarted": len(live), "tasks": ids, "agent": info,
	})
}

// restartAgentTasks stops the given tasks, waits for their processes to exit,
// then queues the same prompts again. The scheduler serializes tasks per
// session, so even a late exit cannot let two runs overlap.
func (s *Server) restartAgentTasks(tasks []*Task) {
	for _, t := range tasks {
		s.stopTask(t)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, t := range tasks {
			cur, err := s.store.GetTask(t.ID)
			if err != nil || !taskTerminal(cur.Status) {
				done = false
				break
			}
		}
		if done {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, t := range tasks {
		cur, err := s.store.GetTask(t.ID)
		if err != nil {
			continue
		}
		if !taskTerminal(cur.Status) {
			// The normal cancel escalation did not win in time; stop claiming
			// the task is live before replacing it.
			killTask(cur, syscall.SIGKILL)
			_ = s.store.ExpirePermissionsForTask(cur.ID)
			_ = s.store.FinishTask(cur.ID, StatusCancelled, nil, "重启 Agent 时强制终止", nowRFC3339())
		}
		if _, err := s.enqueueRetry(cur, "重启 Agent 后自动重新运行"); err != nil {
			s.logger.Printf("restart %s: requeue task %s: %v", cur.Agent, cur.ID, err)
		}
	}
	s.schedule()
}

func taskTerminal(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	includeHidden := q.Get("all") == "1"
	list, err := s.store.ListSessions(q.Get("agent"), q.Get("q"), includeHidden)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": list})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent    string           `json:"agent"`
		Cwd      string           `json:"cwd"`
		Title    string           `json:"title"`
		Settings *SessionSettings `json:"settings"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	agent, ok := s.agents.Get(req.Agent)
	if !ok || !agent.Available {
		writeErr(w, 400, "Agent 不可用: "+req.Agent)
		return
	}
	cfg := s.config()
	cwd, err := cfg.allowedCWD(req.Cwd)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	title := truncateTitle(req.Title, 120)
	if title == "" {
		title = "新会话 · " + agent.Label
	}
	sess := &Session{Agent: req.Agent, Cwd: cwd, Title: title, Source: "web"}
	if err := s.store.CreateSession(sess); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if req.Settings != nil {
		_, _ = s.store.UpdateSessionSettings(sess.ID, func(st *SessionSettings) { *st = *req.Settings })
	}
	writeJSON(w, 201, map[string]any{"session": sess})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	// Any session can be continued in the CLI behind the console's back.
	// Catch up before reporting stats, otherwise the page would show a
	// transcript that never reaches the latest answer. Sync is a stat when
	// nothing changed, so this costs nothing on repeated opens.
	if sess.NativeID != "" {
		if err := s.imp.SyncSession(id); err != nil {
			s.logger.Printf("同步原生会话 %s 失败: %v", id, err)
		} else if fresh, err := s.store.GetSession(id); err == nil {
			sess = fresh
		}
	}
	tasks, _ := s.store.TasksForSession(id, 100)
	events, lastSeq, err := s.store.SessionStats(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	perms, _ := s.store.PendingPermissions(id)
	writeJSON(w, 200, map[string]any{
		"session": sess, "tasks": tasks, "event_count": events,
		"last_seq": lastSeq, "permissions": perms, "settings": s.store.GetSessionSettings(id),
	})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title    *string          `json:"title"`
		Hidden   *bool            `json:"hidden"`
		Settings *SessionSettings `json:"settings"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	// The model picker sets these directly, as the CLI's /model picker does.
	if req.Settings != nil {
		st, err := s.store.UpdateSessionSettings(id, func(st *SessionSettings) { *st = *req.Settings })
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if req.Title == nil && req.Hidden == nil {
			writeJSON(w, 200, map[string]any{"ok": true, "settings": st})
			return
		}
	}
	if req.Title != nil {
		t := truncateTitle(*req.Title, 200)
		if t == "" {
			t = "未命名会话"
		}
		req.Title = &t
	}
	if err := s.store.UpdateSession(id, req.Title, req.Hidden); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("confirm") != id {
		writeErr(w, 400, "删除对话需要二次确认（confirm 必须等于会话 ID）")
		return
	}
	sess, err := s.store.GetSession(id)
	if err != nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	active, _ := s.store.SessionHasActiveTask(id)
	if active {
		writeErr(w, 409, "该会话仍有任务在运行，不能删除原生数据")
		return
	}
	trashed, err := s.trashNative(sess)
	if err != nil {
		writeErr(w, 500, "移动原生会话失败: "+err.Error())
		return
	}
	// Task directories hold events.jsonl, control files and child.pid. They are
	// not rows, so deleting the session has to remove them explicitly; leftovers
	// kept the log endpoint readable for a session that no longer exists.
	tasks, _ := s.store.TasksForSession(id, 1000)
	if err := s.store.DeleteSessionRow(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, t := range tasks {
		if !validID(t.ID) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.config().DataDir, "tasks", t.ID)); err != nil {
			s.logger.Printf("删除任务目录 %s 失败: %v", t.ID, err)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "native_trashed": trashed})
}

func (s *Server) trashNative(sess *Session) (string, error) {
	home, _ := os.UserHomeDir()
	var match string
	if sess.Agent == "claude" {
		root := filepath.Join(home, ".claude", "projects")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == sess.NativeID+".jsonl" {
				match = path
				return io.EOF
			}
			return nil
		})
	} else if sess.Agent == "codex" && sess.NativeID != "" {
		root := filepath.Join(home, ".codex", "sessions")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			// Rollout names are rollout-<time>-<uuid>.jsonl. A substring match
			// could hit a different session whose id merely contains this one.
			if err == nil && !d.IsDir() && uuidFromFilename(d.Name()) == sess.NativeID {
				match = path
				return io.EOF
			}
			return nil
		})
	}
	if match == "" {
		return "", nil
	}
	trashDir := filepath.Join(s.config().DataDir, "trash",
		time.Now().UTC().Format("20060102T150405"), sess.Agent)
	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(trashDir, filepath.Base(match))
	if err := os.Rename(match, dst); err != nil {
		// Cross-device fallback.
		if err := copyFile(match, dst); err != nil {
			return "", err
		}
		_ = os.Remove(match)
	}
	return dst, nil
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.runImport())
}

func (s *Server) runImport() ImportReport {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	rep := s.imp.ImportAll()
	b, _ := json.Marshal(rep)
	_ = s.store.SetMeta("last_import_report", string(b))
	_ = s.store.SetMeta("last_import_at", rep.FinishedAt)
	return rep
}

// autoImport keeps conversations started in a terminal showing up without a
// manual step. The importer skips unchanged transcripts, so an idle pass is
// little more than a directory walk.
func (s *Server) autoImport(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		rep := s.runImport()
		if rep.Imported > 0 || rep.Updated > 0 || rep.Errors > 0 {
			s.logger.Printf("原生会话同步: imported=%d updated=%d errors=%d",
				rep.Imported, rep.Updated, rep.Errors)
		}
	}
}

// handleEvent returns one event unclipped: the page asks for it when the user
// expands a block whose list payload was only a preview.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeErr(w, 400, "无效的事件序号")
		return
	}
	e, err := s.store.GetEvent(r.PathValue("id"), seq)
	if err != nil {
		writeErr(w, 404, "事件不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"event": e})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	// turns selects a page by conversation turns instead of raw event count;
	// limit then acts as the event cap for that page.
	turns, _ := strconv.Atoi(q.Get("turns"))
	// Every page syncs the native transcript first: the page fetches session
	// detail and the first events page in parallel, so whichever arrives first
	// must make the result correct. The "older" path needs it too, otherwise
	// history appended by the CLI was missing until some other request synced.
	if sess, err := s.store.GetSession(id); err == nil && sess.NativeID != "" {
		if err := s.imp.SyncSession(id); err != nil {
			s.logger.Printf("同步原生会话 %s 失败: %v", id, err)
		}
	}
	if v := q.Get("before"); v != "" {
		before, _ := strconv.ParseInt(v, 10, 64)
		if turns > 0 {
			win, err := s.store.EventsBeforeTurns(id, before, turns, limit)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"events": clipEvents(win.Events), "has_more": win.HasMore})
			return
		}
		evs, err := s.store.EventsBefore(id, before, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"events": clipEvents(evs), "has_more": len(evs) >= limit})
		return
	}
	if q.Get("tail") == "1" || q.Get("after") == "" {
		if turns > 0 {
			win, err := s.store.RecentTurns(id, turns, limit)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			last := int64(0)
			if len(win.Events) > 0 {
				last = win.Events[len(win.Events)-1].Seq
			}
			writeJSON(w, 200, map[string]any{"events": clipEvents(win.Events), "last_seq": last,
				"tail": true, "has_more": win.HasMore, "limit": limit})
			return
		}
		evs, err := s.store.RecentEvents(id, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		last := int64(0)
		if len(evs) > 0 {
			last = evs[len(evs)-1].Seq
		}
		writeJSON(w, 200, map[string]any{"events": clipEvents(evs), "last_seq": last, "tail": true, "limit": limit})
		return
	}
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	evs, err := s.store.EventsAfter(id, after, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	last := after
	if len(evs) > 0 {
		last = evs[len(evs)-1].Seq
	}
	writeJSON(w, 200, map[string]any{"events": clipEvents(evs), "last_seq": last, "has_more": len(evs) >= limit})
}

func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Text      string `json:"text"`
		Profile   string `json:"profile"`
		RequestID string `json:"request_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeErr(w, 400, "消息不能为空")
		return
	}
	if len(req.Text) > 256<<10 {
		writeErr(w, 413, "消息过长（上限 256 KiB）")
		return
	}
	sess, err := s.store.GetSession(id)
	if err != nil {
		writeErr(w, 404, "会话不存在")
		return
	}
	// Pull in turns made in a terminal first, so they are ordered before the
	// turn this message starts rather than after it.
	if err := s.imp.SyncSession(id); err != nil {
		s.logger.Printf("同步原生会话 %s 失败: %v", id, err)
	}
	if s.handleLocalCommand(w, sess, req.Text) {
		return
	}
	if req.Profile == "" {
		req.Profile = defaultProfileFor(sess.Agent)
	}
	if !validProfile(sess.Agent, req.Profile) {
		writeErr(w, 400, "未知执行配置: "+req.Profile)
		return
	}
	agent, ok := s.agents.Get(sess.Agent)
	if !ok || !agent.Available {
		writeErr(w, 400, "Agent 不可用: "+sess.Agent)
		return
	}
	// Imported sessions take their cwd from the native transcript, which was
	// never validated against allowed_roots. Re-check it here, otherwise a
	// transcript recorded in /etc or /root would run the agent there.
	if _, err := s.config().allowedCWD(sess.Cwd); err != nil {
		writeErr(w, 400, "会话目录不可用: "+err.Error())
		return
	}
	if req.RequestID == "" {
		req.RequestID = randomID(16)
	}
	if old, err := s.store.GetTaskByRequestID(req.RequestID); err == nil && old != nil {
		writeJSON(w, 200, map[string]any{"task": old, "duplicate": true})
		return
	}
	t := &Task{
		SessionID: id, Agent: sess.Agent, NativeID: sess.NativeID, Cwd: sess.Cwd,
		Prompt: req.Text, Status: StatusPending, Profile: req.Profile,
		RequestID: req.RequestID,
	}
	if err := s.store.CreateTask(t); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ub, _ := json.Marshal(map[string]any{"text": req.Text, "profile": req.Profile})
	_, _ = s.store.InsertEvent(&Event{SessionID: id, TaskID: t.ID, TaskSeq: 0, Sub: 0,
		TS: nowRFC3339(), Kind: "user", Payload: ub})
	if sess.Title == "" || strings.HasPrefix(sess.Title, "新会话") {
		title := truncateTitle(req.Text, 72)
		_ = s.store.UpdateSession(id, &title, nil)
	}
	_ = s.store.TouchSession(id)
	s.schedule()
	t, _ = s.store.GetTask(t.ID)
	pos, ahead, _ := s.store.QueueWait(t.ID)
	t.QueuePosition, t.QueueAhead = pos, ahead
	writeJSON(w, 202, map[string]any{
		"task": t, "queue_position": pos, "queue_ahead": ahead})
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	statuses := taskStatusFilter(r.URL.Query().Get("status"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.store.ListTasks(statuses, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// The task list shows "队列第 N 位"; without this the field was only ever
	// set on the POST response and never rendered.
	for _, t := range tasks {
		if t.Status != StatusPending {
			continue
		}
		if pos, ahead, err := s.store.QueueWait(t.ID); err == nil {
			t.QueuePosition, t.QueueAhead = pos, ahead
		}
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks})
}

func taskStatusFilter(v string) []string {
	switch v {
	case "active":
		return []string{StatusRunning, StatusWaitingInput, StatusPending}
	case "waiting":
		return []string{StatusWaitingInput}
	case "running":
		return []string{StatusRunning}
	case "done", "completed":
		return []string{StatusCompleted}
	case "failed":
		return []string{StatusFailed}
	case "interrupted":
		return []string{StatusInterrupted}
	case "cancelled":
		return []string{StatusCancelled}
	case "finished":
		return []string{StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted}
	}
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.store.GetTask(id)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	sess, _ := s.store.GetSession(t.SessionID)
	perms, _ := s.store.PendingPermissions(t.SessionID)
	if pos, ahead, err := s.store.QueueWait(t.ID); err == nil {
		t.QueuePosition, t.QueueAhead = pos, ahead
	}
	writeJSON(w, 200, map[string]any{"task": t, "session": sess, "permissions": perms})
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.store.GetTask(id)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	if taskTerminal(t.Status) {
		writeErr(w, 409, "任务已结束")
		return
	}
	outcome, status := s.stopTask(t)
	if outcome == cancelSignalled {
		writeJSON(w, 202, map[string]any{"ok": true, "status": "cancelling"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": status})
}

type cancelOutcome int

const (
	// cancelSignalled means a process was told to stop and its exit is watched.
	cancelSignalled cancelOutcome = iota
	// cancelFinished means the task was still queued (or finished meanwhile);
	// no process needs stopping.
	cancelFinished
)

// stopTask cancels a pending or running task and makes sure its process dies.
// It is shared by the HTTP cancel endpoint and the per-agent restart.
func (s *Server) stopTask(t *Task) (cancelOutcome, string) {
	if t.Status == StatusPending {
		// The scheduler may launch this task between our read and this write.
		// Cancel conditionally; if it already started, fall through to the
		// running-task path so the process is actually signalled.
		ok, err := s.store.FinishTaskIfPending(t.ID, StatusCancelled, "用户取消（尚未启动）")
		if err == nil && ok {
			_ = s.store.ExpirePermissionsForTask(t.ID)
			return cancelFinished, StatusCancelled
		}
		if t2, err := s.store.GetTask(t.ID); err == nil {
			t = t2
		}
		if taskTerminal(t.Status) {
			return cancelFinished, t.Status
		}
	}
	_ = s.store.RequestCancel(t.ID)
	// Re-read before signalling: the PID is recorded just after the process
	// starts, so the copy read above can still be 0.
	if cur, err := s.store.GetTask(t.ID); err == nil {
		t = cur
	}
	killTask(t, syscall.SIGTERM)
	go func(task *Task) {
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			cur, err := s.store.GetTask(task.ID)
			if err != nil {
				return
			}
			if taskTerminal(cur.Status) {
				return
			}
			// Refresh the PID so the escalation below targets the live process
			// even when the first read happened before launch recorded it.
			task = cur
			time.Sleep(300 * time.Millisecond)
		}
		killTask(task, syscall.SIGKILL)
		_ = s.store.ExpirePermissionsForTask(task.ID)
		_ = s.store.FinishTask(task.ID, StatusCancelled, nil, "取消超时，已强制终止", nowRFC3339())
		s.schedule()
	}(t)
	return cancelSignalled, "cancelling"
}

// killTask signals a task's runner and its process group. The recorded PID
// is only trusted while it still belongs to this task's runner: once the
// runner is gone the number may name an unrelated process.
func killTask(t *Task, sig syscall.Signal) {
	pid := t.PID
	if pid <= 1 || !isTaskRunner(pid, t.ID) {
		return
	}
	pgid := t.PGID
	if pgid <= 1 {
		pgid = pid
	}
	_ = syscall.Kill(-pgid, sig)
	_ = syscall.Kill(pid, sig)
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.store.GetTask(id)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	switch old.Status {
	case StatusFailed, StatusInterrupted, StatusCancelled:
	default:
		writeErr(w, 409, "只有失败、中断或已取消的任务可以重新运行")
		return
	}
	t, err := s.enqueueRetry(old, "用户手动重新运行任务")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.schedule()
	t, _ = s.store.GetTask(t.ID)
	writeJSON(w, 202, map[string]any{"task": t})
}

// enqueueRetry creates a fresh task with the same prompt as old. The original
// task may have run in a directory that is no longer allowed (an imported
// transcript, or a root removed from allowed_roots since), so the same checks
// apply. The caller schedules the queue.
func (s *Server) enqueueRetry(old *Task, note string) (*Task, error) {
	if _, err := s.config().allowedCWD(old.Cwd); err != nil {
		return nil, fmt.Errorf("会话目录不可用: %w", err)
	}
	if agent, ok := s.agents.Get(old.Agent); !ok || !agent.Available {
		return nil, fmt.Errorf("Agent 不可用: %s", old.Agent)
	}
	profile := old.Profile
	if !validProfile(old.Agent, profile) {
		profile = defaultProfileFor(old.Agent)
	}
	t := &Task{
		SessionID: old.SessionID, Agent: old.Agent, NativeID: old.NativeID,
		Cwd: old.Cwd, Prompt: old.Prompt, Status: StatusPending, Profile: profile,
		RequestID: randomID(16),
	}
	if err := s.store.CreateTask(t); err != nil {
		return nil, err
	}
	nb, _ := json.Marshal(map[string]any{"text": note + "（原任务 " + old.ID + "）"})
	_, _ = s.store.InsertEvent(&Event{SessionID: t.SessionID, TaskID: t.ID, TaskSeq: 0,
		TS: nowRFC3339(), Kind: "notice", Payload: nb})
	ub, _ := json.Marshal(map[string]any{"text": old.Prompt, "retry_of": old.ID})
	_, _ = s.store.InsertEvent(&Event{SessionID: t.SessionID, TaskID: t.ID, TaskSeq: 0, Sub: 1,
		TS: nowRFC3339(), Kind: "user", Payload: ub})
	return t, nil
}

func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The id reaches filepath.Join, so it must be a bare identifier: otherwise
	// a crafted segment could read an events.jsonl outside the data directory.
	if !validID(id) {
		writeErr(w, 400, "任务 ID 无效")
		return
	}
	path := filepath.Join(s.config().DataDir, "tasks", id, "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "日志不存在")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, id+".events.jsonl", st.ModTime(), f)
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

func (s *Server) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	perms, err := s.store.PendingPermissions(r.URL.Query().Get("session_id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"permissions": perms})
}

func (s *Server) handleResolvePermission(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req ctlResponse
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, err := s.store.GetPermission(id)
	if err != nil {
		writeErr(w, 404, "审批请求不存在")
		return
	}
	if p.Status != "pending" {
		writeErr(w, 409, "审批请求已处理")
		return
	}
	t, err := s.store.GetTask(p.TaskID)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	if t.Status != StatusRunning && t.Status != StatusWaitingInput {
		writeErr(w, 409, "任务已不在等待审批")
		return
	}
	var detail ctlRequest
	_ = json.Unmarshal(p.Detail, &detail)
	label, optDetail := "", ""
	switch p.Kind {
	case kindQuestion:
		if req.Decision != decisionAnswer && req.Decision != decisionSkip {
			writeErr(w, 400, "问题只能回答或跳过")
			return
		}
		if req.Decision == decisionAnswer {
			if len(req.Answers) == 0 {
				writeErr(w, 400, "请先回答问题")
				return
			}
			label = "已回答"
			optDetail = describeAnswers(detail.Questions, req.Answers)
		} else {
			label = "已跳过"
		}
	default:
		if len(detail.Options) == 0 {
			// Requests from tasks started before interactions existed offer
			// only allow and deny.
			detail.Options = []ctlOption{{ID: decisionAllow, Label: "允许"}, {ID: decisionDeny, Label: "拒绝"}}
		}
		o, ok := detail.option(req.Decision)
		if !ok {
			writeErr(w, 400, "无效的选项: "+req.Decision)
			return
		}
		label, optDetail = o.Label, o.Detail
	}
	status := statusFor(req.Decision)
	dir := filepath.Join(s.config().DataDir, "tasks", p.TaskID, "control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Claim the request before answering it: two devices answering at once
	// must not both write an answer, or the agent could act on one while the
	// page records the other.
	claimed, err := s.store.ResolvePermission(p.ID, status, strings.TrimSpace(req.Decision+" "+req.Message))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !claimed {
		writeErr(w, 409, "审批请求已处理")
		return
	}
	if err := atomicWriteJSON(filepath.Join(dir, "resp-"+p.ID+".json"), req); err != nil {
		_ = s.store.ReopenPermission(p.ID)
		writeErr(w, 500, err.Error())
		return
	}
	// Record the outcome in the transcript now. The ingestor only writes this
	// event for permissions that are still pending when the agent's done file
	// appears, which is never the case after this handler, so a reopened
	// session showed every approved card as still waiting. Same key as the
	// ingestor's event, so whichever lands second is ignored.
	if b, err := json.Marshal(map[string]any{"permission_id": p.ID, "decision": status,
		"choice": req.Decision, "label": label, "detail": optDetail, "message": req.Message}); err == nil {
		_, _ = s.store.InsertEvent(&Event{SessionID: t.SessionID, TaskID: t.ID, TaskSeq: 1 << 42,
			Sub: permSub(p.ID), TS: nowRFC3339(), Kind: "permission_result", Payload: b})
	}
	if n, _ := s.store.PendingPermissionCount(p.TaskID); n == 0 {
		_ = s.store.SetTaskWaiting(p.TaskID, false)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": status, "label": label, "detail": optDetail})
}

// ---------------------------------------------------------------------------
// CWD check
// ---------------------------------------------------------------------------

func (s *Server) handleCwdCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	cfg := s.config()
	cwd, err := cfg.allowedCWD(req.Path)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	active, _ := s.store.ActiveTasksOnCwd(cwd)
	gitRoot := ""
	if out, err := runBrief(3*time.Second, "git", "-C", cwd, "rev-parse", "--show-toplevel"); err == nil {
		gitRoot = firstLine(out)
	}
	warning := ""
	if active > 0 {
		warning = fmt.Sprintf("该目录已有 %d 个任务在运行，同时修改可能冲突；建议使用独立目录或 git worktree。", active)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "cwd": cwd, "git_root": gitRoot,
		"active_tasks": active, "warning": warning})
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

func (s *Server) schedule() {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	cfg := s.config()
	for {
		active, err := s.store.CountActiveTasks()
		if err != nil || active >= cfg.MaxConcurrent {
			return
		}
		pending, err := s.store.RunnablePendingTasks(200)
		if err != nil {
			return
		}
		pick := s.pickPending(pending)
		if pick == nil {
			return
		}
		s.launching[pick.ID] = true
		if err := s.launch(pick); err != nil {
			delete(s.launching, pick.ID)
			if errors.Is(err, errTaskAlreadyClaimed) {
				continue // another scheduler owns it
			}
			s.logger.Printf("launch task %s: %v", pick.ID, err)
			_ = s.store.FinishTask(pick.ID, StatusFailed, nil, err.Error(), nowRFC3339())
		}
	}
}

// pickPending returns the oldest queued task whose session is free, or nil.
// Tasks of different sessions and different agents run in parallel; only work
// already running on the same session makes a task wait.
//
// A pending task must not count as occupying its own session: the previous
// check did, so every queued task blocked itself and nothing ever left the
// queue ("排队中 · 前面的任务结束后自动执行" was permanent).
// errTaskAlreadyClaimed means another scheduler won the atomic claim; it is
// not a failure and must not mark the task failed.
var errTaskAlreadyClaimed = errors.New("task already claimed by another scheduler")

func (s *Server) pickPending(pending []*Task) *Task {
	for _, t := range pending {
		if s.launching[t.ID] {
			continue
		}
		busy, err := s.store.SessionHasRunningTask(t.SessionID)
		if err != nil || busy {
			continue
		}
		return t
	}
	return nil
}

func (s *Server) launch(t *Task) error {
	cfg := s.config()
	sess, err := s.store.GetSession(t.SessionID)
	if err != nil {
		return err
	}
	taskDir := filepath.Join(cfg.DataDir, "tasks", t.ID)
	if err := os.MkdirAll(filepath.Join(taskDir, "control"), 0o700); err != nil {
		return err
	}
	nativeID := sess.NativeID
	isNew := nativeID == ""
	if isNew && t.Agent == "claude" {
		nativeID = newUUID()
	}
	settings := s.store.GetSessionSettings(sess.ID)
	spec := TaskSpec{
		TaskID: t.ID, SessionID: t.SessionID, Agent: t.Agent,
		NativeID: nativeID, IsNew: isNew, Cwd: sess.Cwd, Prompt: t.Prompt,
		Profile: t.Profile, MaxLogBytes: cfg.TaskLogMaxBytes,
		PermTimeout: cfg.PermissionTTLMin * 60, RunnerID: randomID(9),
		Model: settings.Model, Effort: settings.Effort,
	}
	if exe, err := os.Executable(); err == nil {
		spec.Executable = exe
	}
	if err := atomicWriteJSON(filepath.Join(taskDir, "task.json"), spec); err != nil {
		return err
	}
	boot := currentBootID()
	// Claim before starting anything: a concurrent scheduler (old process still
	// shutting down during a restart) must not launch the same task twice.
	claimed, err := s.store.ClaimPendingTask(t.ID, boot)
	if err != nil {
		return err
	}
	if !claimed {
		return errTaskAlreadyClaimed
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "run-task", "--task-dir", taskDir)
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.Dir = "/"
	cmd.Env = childEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	// Record the runner without resetting started_at (the claim already set it).
	_ = s.store.SetTaskProcess(t.ID, pid, pid, boot)
	_ = s.store.SetTaskNativeID(t.ID, nativeID)
	s.ing.Start(t.ID)
	go func() {
		_ = cmd.Wait()
		// launching is guarded by schedMu; deleting it unlocked was a data race
		// with concurrent schedule() calls reading the same map.
		s.schedMu.Lock()
		delete(s.launching, t.ID)
		s.schedMu.Unlock()
		s.schedule()
	}()
	s.logger.Printf("task %s launched: agent=%s pid=%d session=%s", t.ID, t.Agent, pid, t.SessionID)
	return nil
}

func childEnv() []string {
	env := os.Environ()
	haveHome, havePath := false, false
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			haveHome = true
		}
		if strings.HasPrefix(e, "PATH=") {
			havePath = true
		}
	}
	if !haveHome {
		env = append(env, "HOME=/root")
	}
	if !havePath {
		env = append(env, "PATH=/root/.local/bin:/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	// The server has no WriteTimeout (SSE needs it), so a client that sends
	// headers and then stalls would hold a handler and the single DB connection
	// forever. Request bodies are small; a short deadline is enough.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return err
	}
	return nil
}

func splitHostPort(a string) (string, string, error) {
	i := strings.LastIndex(a, ":")
	if i < 0 {
		return "", "", errors.New("no port")
	}
	host := strings.Trim(a[:i], "[]")
	return host, a[i+1:], nil
}

func parseIP(host string) string {
	if host == "" {
		return ""
	}
	return host
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var _ = context.Background
var _ = sql.ErrNoRows
