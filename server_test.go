package console

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rymesh/internal/config"
	"rymesh/internal/store"
	"rymesh/internal/util"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.DataDir, "log")
	cfg.AllowedRoots = []string{cfg.DataDir}
	cfg.DefaultCWD = cfg.DataDir
	cfg.Secret = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	return cfg
}

func TestAuthTokenVerification(t *testing.T) {
	cfg := testConfig(t)
	a := newAuthManager(cfg)
	tok := a.token()
	info, ok := a.verifyToken(tok)
	if !ok || info.CSRF == "" {
		t.Fatal("valid token rejected")
	}

	parts := strings.SplitN(tok, ".", 2)
	body, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var p authPayload
	_ = json.Unmarshal(body, &p)
	p.Exp = time.Now().Add(-time.Hour).Unix()
	expiredBody, _ := json.Marshal(p)
	expired := base64.RawURLEncoding.EncodeToString(expiredBody) + "." +
		base64.RawURLEncoding.EncodeToString(a.Sign([]byte(base64.RawURLEncoding.EncodeToString(expiredBody))))
	if _, ok := a.verifyToken(expired); ok {
		t.Fatal("expired token accepted")
	}

	// Changing the payload without re-signing must fail (the old bug signed the
	// decoded JSON instead of the base64 body).
	if _, ok := a.verifyToken(parts[0] + "x." + parts[1]); ok {
		t.Fatal("tampered token accepted")
	}

	other := newAuthManager(&config.Config{Secret: base64.StdEncoding.EncodeToString([]byte("another-secret-another-secret-xx")), SessionTTLHours: 1})
	if _, ok := other.verifyToken(tok); ok {
		t.Fatal("token signed with another secret accepted")
	}
}

func TestPIDFilePathSeparatesInstances(t *testing.T) {
	a := pidFilePath([]string{"127.0.0.1:8899", "10.0.0.6:8899"})
	b := pidFilePath([]string{"127.0.0.1:8901"})
	if a == b {
		t.Fatalf("instances share pid path: %s", a)
	}
	if !strings.HasSuffix(a, "rymesh-8899.pid") || !strings.HasSuffix(b, "rymesh-8901.pid") {
		t.Fatalf("unexpected pid paths: %s %s", a, b)
	}
	if got := pidFilePath(nil); !strings.HasSuffix(got, "rymesh-default.pid") {
		t.Fatalf("fallback pid path = %s", got)
	}
}

func TestGzipMiddleware(t *testing.T) {
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			writeJSON(w, 200, map[string]string{"text": strings.Repeat("tool output ", 200)})
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: hello\ndata: {}\n\n"))
		default:
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("not really a png"))
		}
	}))

	get := func(path string, enc string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// A gzip-capable client gets a compressed JSON body that decodes back.
	rec := get("/json", "gzip, deflate")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Fatal("Content-Length must not be sent with a compressed body")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("missing Vary: Accept-Encoding")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), "tool output") {
		t.Fatalf("decoded body = %q", body)
	}
	if len(body) <= rec.Body.Len() {
		t.Fatalf("compression did not shrink body: %d vs %d", len(body), rec.Body.Len())
	}

	// Identity clients keep a plain body and no encoding header, but still
	// carry Vary so a cache never serves them a gzip body.
	plain := get("/json", "")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("identity response was compressed")
	}
	if !strings.Contains(plain.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("identity response missing Vary: Accept-Encoding")
	}
	if !strings.Contains(plain.Body.String(), "tool output") {
		t.Fatal("identity body wrong")
	}

	// SSE must never be wrapped: per-event buffering would break streaming.
	sse := get("/stream", "gzip")
	if sse.Header().Get("Content-Encoding") != "" {
		t.Fatal("SSE response was gzipped")
	}
	if !strings.Contains(sse.Body.String(), "event: hello") {
		t.Fatal("SSE body was altered")
	}

	// Binary types are passed through untouched.
	bin := get("/icon", "gzip")
	if bin.Header().Get("Content-Encoding") != "" {
		t.Fatal("image/png was gzipped")
	}
}

func TestCompressibleType(t *testing.T) {
	for _, ct := range []string{"application/json; charset=utf-8", "text/html", "application/javascript", "image/svg+xml"} {
		if !compressibleType(ct) {
			t.Errorf("%q should be compressible", ct)
		}
	}
	for _, ct := range []string{"image/png", "application/octet-stream", "text/event-stream", ""} {
		if compressibleType(ct) {
			t.Errorf("%q should not be compressible", ct)
		}
	}
}

func TestSystemdUnitUsesInstalledPaths(t *testing.T) {
	unit := systemdUnit("/usr/local/bin/rymesh", "/etc/rymesh/config.json")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/rymesh serve --config /etc/rymesh/config.json",
		"WorkingDirectory=/usr/local/bin",
		"KillMode=process", // detached tasks must survive service restarts
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

// The scheduler must be able to start every queued task: a pending task does
// not occupy its own session, and different sessions run in parallel.
func TestPickPendingStartsQueuedTasks(t *testing.T) {
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	a := &store.Session{Agent: "codex", Cwd: "/tmp", Title: "a"}
	b := &store.Session{Agent: "claude", Cwd: "/tmp", Title: "b"}
	for _, sess := range []*store.Session{a, b} {
		if err := st.CreateSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(sess *store.Session, created string) *store.Task {
		task := &store.Task{SessionID: sess.ID, Agent: sess.Agent, Cwd: "/tmp", Prompt: "p",
			Status: store.StatusPending, CreatedAt: created}
		if err := st.CreateTask(task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	a1 := mk(a, "2026-01-01T00:00:00Z")
	b1 := mk(b, "2026-01-01T00:00:01Z")
	a2 := mk(a, "2026-01-01T00:00:02Z")

	pending, _ := st.PendingTasks(20)
	pick := srv.pickPending(pending)
	if pick == nil || pick.ID != a1.ID {
		t.Fatalf("first pick = %v, want oldest task", pick)
	}

	// Once a1 is running, a2 must wait but b1 (another session) must not.
	if err := st.MarkTaskRunning(a1.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingTasks(20)
	if pick := srv.pickPending(pending); pick == nil || pick.ID != b1.ID {
		t.Fatalf("second pick = %v, want the other session's task", pick)
	}
	// With both sessions busy, nothing is left to start.
	if err := st.MarkTaskRunning(b1.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingTasks(20)
	if pick := srv.pickPending(pending); pick != nil {
		t.Fatalf("picked %s although both sessions are running", pick.ID)
	}
	// a2 becomes eligible once a1 finishes.
	if err := st.FinishTask(a1.ID, store.StatusCompleted, nil, "", util.NowRFC3339()); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingTasks(20)
	if pick := srv.pickPending(pending); pick == nil || pick.ID != a2.ID {
		t.Fatalf("pick after completion = %v, want %s", pick, a2.ID)
	}
}

// handleTaskLog must reject a traversal id instead of joining it into a path.
func TestTaskLogRejectsTraversal(t *testing.T) {
	data := t.TempDir()
	st, err := store.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	cfg.DataDir = data
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	// A secret exactly where a traversal would land.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "events.jsonl"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The route segment is decoded before it reaches the handler, so set the
	// path value directly to exercise the guard itself.
	for _, id := range []string{
		strings.Repeat("../", 8) + strings.TrimPrefix(outside, "/"),
		strings.Repeat("..%2F", 8) + strings.TrimPrefix(outside, "/"),
		"/etc/passwd/..",
	} {
		r := httptest.NewRequest("GET", "/api/tasks/x/log", nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		srv.handleTaskLog(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status = %d, want 400", id, w.Code)
		}
		if strings.Contains(w.Body.String(), "TOP-SECRET") {
			t.Fatal("traversal read a file outside the data directory")
		}
	}
}

// authManager must validate against the config currently published by the
// server, not a stale pointer from startup (settings use copy-on-write).
func TestAppConfigSwapKeepsAuthInSync(t *testing.T) {
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	tok := srv.auth.token()
	if _, ok := srv.auth.verifyToken(tok); !ok {
		t.Fatal("fresh token rejected")
	}
	secret := base64.StdEncoding.EncodeToString([]byte("a-brand-new-secret-0123456789"))
	srv.updateConfig(func(c *config.Config) { c.Secret = secret; c.MaxConcurrent = 2 })
	if _, ok := srv.auth.verifyToken(tok); ok {
		t.Fatal("token signed with the old secret accepted after config swap")
	}
	// The rest of the config must survive the swap.
	if got := srv.config().MaxConcurrent; got != 2 {
		t.Fatalf("max_concurrent = %d", got)
	}
	if len(srv.config().AllowedRoots) == 0 {
		t.Fatal("allowed_roots lost in copy-on-write")
	}
}

// Deleting a session must also remove its task directories (events.jsonl,
// control files); otherwise the log endpoint stays readable for a session that
// no longer exists and the data dir grows forever.
func TestDeleteSessionRemovesTaskDirs(t *testing.T) {
	data := t.TempDir()
	st, err := store.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	cfg.DataDir = data
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	sess := &store.Session{Agent: "claude", Cwd: data, Title: "d"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: store.StatusCompleted}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "tasks", task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling task of another session must survive.
	other := &store.Task{SessionID: "other", Agent: "claude", Cwd: data, Prompt: "o", Status: store.StatusCompleted}
	if err := st.CreateTask(other); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(data, "tasks", other.ID)
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	csrf := srv.auth.setCookie(w)
	ck := w.Result().Cookies()[0]

	r := httptest.NewRequest("DELETE", "/api/sessions/"+sess.ID+"?confirm="+sess.ID, nil)
	r.AddCookie(ck)
	r.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("delete = %d %q", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("task dir left behind")
	}
	if _, err := os.Stat(otherDir); err != nil {
		t.Fatalf("sibling task dir removed: %v", err)
	}
}

// There is no password login: only the one-time ticket signs a browser in.
func TestNoPasswordLogin(t *testing.T) {
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(testConfig(t), "", st, log.New(io.Discard, "", 0), nil)
	for _, p := range []string{"/api/login", "/api/password", "/api/logout"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", p, strings.NewReader(`{"password":"admin"}`)))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want no such endpoint", p, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s set a cookie", p)
		}
	}
}

// "gzip;q=0" is an explicit refusal and must not produce a compressed body.
func TestAcceptsGzipHonorsQZero(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip;q=1.0", true},
		{"gzip; q=0", false},
		{"gzip;q=0.0", false},
		{"deflate, gzip;q=0", false},
		{"deflate, gzip;q=0.5", true},
		{"identity", false},
		{"", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", c.header)
		if got := acceptsGzip(r); got != c.want {
			t.Fatalf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestUnknownAPIPathReturns404(t *testing.T) {
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(testConfig(t), "", st, log.New(io.Discard, "", 0), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown API path = %d, want 404 (body %.80q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("unknown API path served the SPA: %q", ct)
	}
}

func TestClipPayloadKeepsPreviewOfLargeFields(t *testing.T) {
	big, _ := json.Marshal(map[string]any{"text": strings.Repeat("思", 4000)})
	var m map[string]any
	if err := json.Unmarshal(clipPayload("thinking", 42, big), &m); err != nil {
		t.Fatal(err)
	}
	c, ok := m["text"].(map[string]any)
	if !ok || c["__clipped"] != true || c["seq"] != float64(42) || c["field"] != "text" {
		t.Fatalf("clipped = %v", m["text"])
	}
	if p := c["preview"].(string); len([]rune(p)) != clipPreviewRunes {
		t.Fatalf("preview runes = %d", len([]rune(p)))
	}
	// Small payloads and kinds the page renders in full are left alone.
	small, _ := json.Marshal(map[string]any{"text": "short"})
	if string(clipPayload("thinking", 1, small)) != string(small) {
		t.Fatal("small payload changed")
	}
	answer, _ := json.Marshal(map[string]any{"text": strings.Repeat("a", 10000)})
	if string(clipPayload("assistant", 1, answer)) != string(answer) {
		t.Fatal("assistant text must never be clipped")
	}
}

func TestStaticAssetsRevalidateWithETag(t *testing.T) {
	st, err := store.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(testConfig(t), "", st, log.New(io.Discard, "", 0), nil)
	for _, path := range []string{"/app.js", "/"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		etag := rec.Header().Get("ETag")
		if rec.Code != 200 || etag == "" {
			t.Fatalf("%s: code %d etag %q", path, rec.Code, etag)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("%s revalidation: code %d body %d", path, rec.Code, rec.Body.Len())
		}
	}
}

// A hub relays a node's API with the node token: the node's data comes back,
// the hub's login endpoints are never forwarded, and a wrong token surfaces as
// a gateway error instead of logging the user out of the hub.
func TestHubRelaysNodeAPI(t *testing.T) {
	newSrv := func(cfg *config.Config) *Server {
		st, err := store.OpenStore(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	}
	nodeCfg := testConfig(t)
	nodeCfg.NodeToken = "node-secret"
	nodeCfg.NodeName = "Mac"
	node := newSrv(nodeCfg)
	if err := node.store.CreateSession(&store.Session{Agent: "claude", Cwd: nodeCfg.DataDir, Title: "on the mac"}); err != nil {
		t.Fatal(err)
	}
	ns := httptest.NewServer(node.Handler())
	defer ns.Close()

	hubCfg := testConfig(t)
	hubCfg.Nodes = []config.NodeConfig{{ID: "mac", Name: "Mac", URL: ns.URL, Token: "node-secret"},
		{ID: "bad", Name: "Bad", URL: ns.URL, Token: "wrong"}}
	hub := newSrv(hubCfg)
	tok := hub.auth.token()
	info, _ := hub.auth.verifyToken(tok)
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		req.Header.Set("X-CSRF-Token", info.CSRF)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		hub.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := do("GET", "/n/mac/api/sessions")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "on the mac") {
		t.Fatalf("relayed sessions: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("GET", "/n/mac/api/session"); rec.Code != 403 {
		t.Fatalf("hub-only path relayed: %d", rec.Code)
	}
	if rec := do("GET", "/n/bad/api/sessions"); rec.Code != http.StatusBadGateway {
		t.Fatalf("wrong token: %d %s", rec.Code, rec.Body.String())
	}
	// Without the hub's own login nothing is relayed.
	req := httptest.NewRequest("GET", "/n/mac/api/sessions", nil)
	rec = httptest.NewRecorder()
	hub.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated relay: %d", rec.Code)
	}
	// The device list reports the node online and the bad token as such.
	rec = do("GET", "/api/nodes")
	var nl struct{ Nodes []nodeView }
	_ = json.Unmarshal(rec.Body.Bytes(), &nl)
	if len(nl.Nodes) != 3 || !nl.Nodes[1].Online || nl.Nodes[2].Online || nl.Nodes[2].Error != "令牌无效" {
		t.Fatalf("nodes = %+v", nl.Nodes)
	}
}

// Approving from the page must leave the outcome in the transcript, so the
// card still reads as handled when the session is opened again.
func TestResolvePermissionRecordsResultEvent(t *testing.T) {
	cfg := testConfig(t)
	st, err := store.OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &store.Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: store.StatusWaitingInput}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	perm := &store.Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "Bash", Input: json.RawMessage(`{}`), Status: "pending"}
	if err := st.CreatePermission(perm); err != nil {
		t.Fatal(err)
	}
	tok := srv.auth.token()
	info, _ := srv.auth.verifyToken(tok)
	req := httptest.NewRequest("POST", "/api/permissions/"+perm.ID, strings.NewReader(`{"decision":"allow"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	req.Header.Set("X-CSRF-Token", info.CSRF)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	evs, _ := st.RecentEvents(sess.ID, 10)
	found := false
	for _, e := range evs {
		if e.Kind == "permission_result" && strings.Contains(string(e.Payload), `"allowed"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission_result event after approval: %d events", len(evs))
	}
}

// The trash keeps a deleted conversation's transcript for 30 days, by the
// deletion time in the folder name; anything else there is left alone.
func TestPruneTrash(t *testing.T) {
	data := t.TempDir()
	st, err := store.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	cfg.DataDir = data
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"20260920T110000", "20261001T000000", "keep-me"} {
		if err := os.MkdirAll(filepath.Join(data, "trash", name, "codex"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	srv.pruneTrash(now)
	left, _ := os.ReadDir(filepath.Join(data, "trash"))
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "20261001T000000,keep-me" {
		t.Fatalf("trash after prune = %s", got)
	}
}
