package console

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.DataDir, "log")
	cfg.AllowedRoots = []string{cfg.DataDir}
	cfg.DefaultCWD = cfg.DataDir
	cfg.Secret = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	cfg.PasswordHash = hashPassword("correct horse battery staple")
	return cfg
}

func TestPasswordHashing(t *testing.T) {
	h := hashPassword("s3cret-passphrase")
	if !verifyPassword(h, "s3cret-passphrase") {
		t.Fatal("correct password rejected")
	}
	if verifyPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if verifyPassword("garbage", "x") {
		t.Fatal("malformed hash accepted")
	}
	if hashPassword("a") == hashPassword("a") {
		t.Fatal("hashes must use a random salt")
	}
}

func TestAllowedCWD(t *testing.T) {
	// allowedCWD resolves symlinks; on macOS the temp dir lives behind one
	// (/var -> /private/var), so compare against the resolved root.
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DefaultCWD: root, AllowedRoots: []string{root}}

	got, err := cfg.allowedCWD(filepath.Join(root, "sub"))
	if err != nil || got != filepath.Join(root, "sub") {
		t.Fatalf("inside root: got %q err %v", got, err)
	}
	if _, err := cfg.allowedCWD(outside); err == nil {
		t.Fatal("outside root accepted")
	}
	if _, err := cfg.allowedCWD("/etc"); err == nil {
		t.Fatal("/etc accepted")
	}
	// A symlink inside the root pointing outside must be rejected.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := cfg.allowedCWD(link); err == nil {
			t.Fatal("symlink escape accepted")
		}
	}
	if _, err := cfg.allowedCWD(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if _, err := cfg.allowedCWD("relative/path"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestConfigVersionIsEnforced(t *testing.T) {
	cfg := defaultConfig()
	cfg.Version = 2
	err := cfg.normalize()
	if err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Fatalf("expected version error, got %v", err)
	}
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
		base64.RawURLEncoding.EncodeToString(a.sign([]byte(base64.RawURLEncoding.EncodeToString(expiredBody))))
	if _, ok := a.verifyToken(expired); ok {
		t.Fatal("expired token accepted")
	}

	// Changing the payload without re-signing must fail (the old bug signed the
	// decoded JSON instead of the base64 body).
	if _, ok := a.verifyToken(parts[0] + "x." + parts[1]); ok {
		t.Fatal("tampered token accepted")
	}

	other := newAuthManager(&Config{Secret: base64.StdEncoding.EncodeToString([]byte("another-secret-another-secret-xx")), SessionTTLHours: 1})
	if _, ok := other.verifyToken(tok); ok {
		t.Fatal("token signed with another secret accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	cfg := testConfig(t)
	a := newAuthManager(cfg)
	ip := "192.0.2.10"
	for i := 0; i < 5; i++ {
		if ok, _ := a.loginAllowed(ip); !ok {
			t.Fatalf("locked too early at attempt %d", i+1)
		}
		a.loginFailed(ip)
	}
	if ok, wait := a.loginAllowed(ip); ok || wait <= 0 {
		t.Fatal("expected lockout after five failures")
	}
	if ok, _ := a.loginAllowed("192.0.2.11"); !ok {
		t.Fatal("lockout leaked to another IP")
	}
	a.loginOK(ip)
	if ok, _ := a.loginAllowed(ip); !ok {
		t.Fatal("successful login did not clear lockout")
	}
}

func TestDefaultPasswordRequiresChange(t *testing.T) {
	cfg, pw, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if pw != defaultPassword || !cfg.MustChangePassword {
		t.Fatalf("new config: pw=%q must_change=%v", pw, cfg.MustChangePassword)
	}
	if !verifyPassword(cfg.PasswordHash, defaultPassword) {
		t.Fatal("default password hash does not verify")
	}
	a := newAuthManager(cfg)
	if !a.mustChangePassword() {
		t.Fatal("auth manager did not report required change")
	}
	a.changePassword(hashPassword("x"), false)
	if a.mustChangePassword() {
		t.Fatal("password change did not clear the flag")
	}
	if !a.verifyPassword("x") {
		t.Fatal("changed password rejected")
	}
}

func TestDeleteSessionRowCleansDependents(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "delete me", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusCompleted}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: 1,
		TS: nowRFC3339(), Kind: "assistant", Payload: json.RawMessage(`{"text":"hi"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSessionRow(sess.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "tasks", "events", "permissions"} {
		var n int
		col := "session_id"
		if table == "sessions" {
			col = "id"
		}
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+`=?`, sess.ID).Scan(&n); err != nil {
			t.Fatalf("%s count: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s still has %d rows", table, n)
		}
	}
}

func TestStoreEventPagingAndIdempotency(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		payload, _ := json.Marshal(map[string]any{"text": i})
		added, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: int64(i),
			TS: nowRFC3339(), Kind: "assistant", Payload: payload})
		if err != nil || !added {
			t.Fatalf("insert %d: added=%v err=%v", i, added, err)
		}
	}
	// Same (task, task_seq, sub) must be ignored for idempotent re-ingestion.
	if added, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: 5,
		TS: nowRFC3339(), Kind: "assistant", Payload: json.RawMessage(`{"text":"dup"}`)}); err != nil || added {
		t.Fatalf("duplicate insert added=%v err=%v", added, err)
	}

	last, err := st.LastEventSeq(sess.ID)
	if err != nil || last != 10 {
		t.Fatalf("last seq = %d err %v", last, err)
	}
	after, err := st.EventsAfter(sess.ID, 6, 100)
	if err != nil || len(after) != 4 || after[0].Seq != 7 || after[3].Seq != 10 {
		t.Fatalf("after: %+v err %v", seqs(after), err)
	}
	before, err := st.EventsBefore(sess.ID, 7, 3)
	if err != nil || len(before) != 3 || before[0].Seq != 4 || before[2].Seq != 6 {
		t.Fatalf("before: %+v err %v", seqs(before), err)
	}
	recent, err := st.RecentEvents(sess.ID, 3)
	if err != nil || len(recent) != 3 || recent[0].Seq != 8 || recent[2].Seq != 10 {
		t.Fatalf("recent: %+v err %v", seqs(recent), err)
	}
}

func seqs(evs []*Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

// seedTurns writes `turns` conversations of `perTurn` events each, starting
// every turn with a user event.
func seedTurns(t *testing.T, st *Store, turns, perTurn int) *Session {
	t.Helper()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	seq := 0
	for turn := 0; turn < turns; turn++ {
		for i := 0; i < perTurn; i++ {
			seq++
			kind := "assistant"
			if i == 0 {
				kind = "user"
			}
			payload, _ := json.Marshal(map[string]any{"text": seq})
			if _, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: int64(seq),
				TS: nowRFC3339(), Kind: kind, Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sess
}

func TestRecentTurnsPagesByTurn(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := seedTurns(t, st, 20, 3) // 60 events, user events at 1,4,7,...

	win, err := st.RecentTurns(sess.ID, 5, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Events) != 15 {
		t.Fatalf("5 turns = %d events, want 15", len(win.Events))
	}
	if win.Events[0].Kind != "user" {
		t.Fatalf("page must start on a user event, got %s", win.Events[0].Kind)
	}
	if got := win.Events[len(win.Events)-1].Seq; got != 60 {
		t.Fatalf("page must end at the newest event, got seq %d", got)
	}
	if !win.HasMore {
		t.Fatal("older events exist, HasMore should be true")
	}

	// Asking for more turns than exist returns everything, still ending newest.
	all, err := st.RecentTurns(sess.ID, 50, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 60 || all.Events[0].Seq != 1 || all.Events[59].Seq != 60 {
		t.Fatalf("whole session: %d events %v", len(all.Events), seqs(all.Events)[:3])
	}
	if all.HasMore {
		t.Fatal("nothing older than the first event, HasMore should be false")
	}
}

func TestRecentTurnsEventCapNeverSplitsATurn(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Two short turns, then one 20-event turn. The 20-event turn alone exceeds
	// the cap, so the cap must yield: a half turn is what turn paging prevents.
	sess := seedTurns(t, st, 2, 1)
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p2", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		payload, _ := json.Marshal(map[string]any{"text": i})
		kind := "tool"
		if i == 0 {
			kind = "user"
		}
		if _, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: int64(i + 1),
			TS: nowRFC3339(), Kind: kind, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	win, err := st.RecentTurns(sess.ID, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if win.Events[0].Kind != "user" {
		t.Fatalf("page must start on a user event, got %s", win.Events[0].Kind)
	}
	if len(win.Events) != 20 {
		t.Fatalf("long turn was split: %d events, want the whole 20", len(win.Events))
	}
	if win.Events[len(win.Events)-1].Kind == "user" {
		t.Fatal("capped page lost the newest events")
	}
	if !win.HasMore {
		t.Fatal("older turns exist")
	}
}

func TestEventsBeforeTurnsWalksBackwardsByTurn(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := seedTurns(t, st, 20, 3) // seq 1..60, user at 1,4,7,...,58

	first, err := st.RecentTurns(sess.ID, 5, 400)
	if err != nil {
		t.Fatal(err)
	}
	oldest := first.Events[0].Seq // 46
	second, err := st.EventsBeforeTurns(sess.ID, oldest, 5, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 15 || second.Events[0].Seq != 31 || second.Events[14].Seq != 45 {
		t.Fatalf("previous page = %v", seqs(second.Events))
	}
	if second.Events[0].Kind != "user" {
		t.Fatal("previous page must start on a user event")
	}
	if !second.HasMore {
		t.Fatal("more turns remain")
	}

	// Walking back to the very beginning must clear HasMore.
	cur := second
	for i := 0; i < 10 && cur.HasMore; i++ {
		cur, err = st.EventsBeforeTurns(sess.ID, cur.Events[0].Seq, 5, 400)
		if err != nil {
			t.Fatal(err)
		}
	}
	if cur.HasMore {
		t.Fatal("reached the start but HasMore is still true")
	}
	if cur.Events[0].Seq != 1 {
		t.Fatalf("last page should reach seq 1, got %d", cur.Events[0].Seq)
	}
}

// A session with fewer turns left than a full page must load the rest in one
// go, starting at the session's first event, instead of a cap-sized slice that
// can begin in the middle of the opening turn.
func TestRecentTurnsShortSessionReturnsWholeConversation(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := seedTurns(t, st, 3, 200) // 600 events, far above the cap

	win, err := st.RecentTurns(sess.ID, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Events) != 600 {
		t.Fatalf("whole conversation = %d events, want 600", len(win.Events))
	}
	if win.Events[0].Seq != 1 || win.Events[0].Kind != "user" {
		t.Fatalf("page must start at the first event, got seq %d kind %s",
			win.Events[0].Seq, win.Events[0].Kind)
	}
	if win.HasMore {
		t.Fatal("nothing older than the first event, HasMore should be false")
	}
}

func TestEventsBeforeTurnsShortTailStartsAtSessionStart(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := seedTurns(t, st, 12, 3) // seq 1..36, user at 1,4,...,34

	first, err := st.RecentTurns(sess.ID, 10, 400)
	if err != nil {
		t.Fatal(err)
	}
	if first.Events[0].Seq != 7 {
		t.Fatalf("first page should start at seq 7, got %d", first.Events[0].Seq)
	}
	rest, err := st.EventsBeforeTurns(sess.ID, first.Events[0].Seq, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Events) != 6 || rest.Events[0].Seq != 1 || rest.Events[0].Kind != "user" {
		t.Fatalf("remaining tail = %v, want seqs 1..6 starting on user", seqs(rest.Events))
	}
	if rest.HasMore {
		t.Fatal("tail reached the first event, HasMore should be false")
	}
}

func TestEventsBeforeTurnsHandlesMidTurnCursor(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := seedTurns(t, st, 10, 4) // user at 1,5,9,...,37

	// Cursor in the middle of the last turn: the page must still begin on a
	// user event and never include the cursor itself.
	win, err := st.EventsBeforeTurns(sess.ID, 40, 2, 400)
	if err != nil {
		t.Fatal(err)
	}
	if win.Events[0].Kind != "user" {
		t.Fatalf("starts on %s", win.Events[0].Kind)
	}
	for _, e := range win.Events {
		if e.Seq >= 40 {
			t.Fatalf("cursor event leaked into page: %d", e.Seq)
		}
	}
}

func TestStoreTaskLifecycle(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "codex", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	t1 := &Task{SessionID: sess.ID, Agent: "codex", Cwd: "/tmp", Prompt: "one", RequestID: "req-1"}
	if err := st.CreateTask(t1); err != nil {
		t.Fatal(err)
	}
	// Request IDs are idempotent.
	if _, err := st.GetTaskByRequestID("req-1"); err != nil {
		t.Fatal("request id lookup failed")
	}
	active, err := st.CountActiveTasks()
	if err != nil || active != 0 {
		t.Fatalf("pending task counted active: %d %v", active, err)
	}
	if err := st.MarkTaskRunning(t1.ID, 123, 123, "boot"); err != nil {
		t.Fatal(err)
	}
	active, _ = st.CountActiveTasks()
	if active != 1 {
		t.Fatalf("running count = %d", active)
	}
	code := 0
	if err := st.FinishTask(t1.ID, StatusCompleted, &code, "", nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(t1.ID)
	if err != nil || got.Status != StatusCompleted || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("finished task = %+v err %v", got, err)
	}
}

func TestReconcileClassifiesLostTasksAsInterrupted(t *testing.T) {
	dataDir := t.TempDir()
	st, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	// A task from a previous boot whose runner cannot exist any more.
	lost := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p",
		Status: StatusRunning, BootID: "boot-id-from-a-previous-boot", PID: 999999}
	if err := st.CreateTask(lost); err != nil {
		t.Fatal(err)
	}
	// A pending task must stay pending; reconcile never starts or replays it.
	pending := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "q", Status: StatusPending}
	if err := st.CreateTask(pending); err != nil {
		t.Fatal(err)
	}
	ing := newIngestor(st, dataDir, log.New(io.Discard, "", 0))
	ing.Reconcile()

	got, err := st.GetTask(lost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInterrupted || !strings.Contains(got.Error, "服务器已重启") {
		t.Fatalf("lost task = status %q error %q", got.Status, got.Error)
	}
	gotP, _ := st.GetTask(pending.ID)
	if gotP.Status != StatusPending {
		t.Fatalf("pending task changed to %q", gotP.Status)
	}
	// No replacement task may be created automatically.
	tasks, _ := st.ListTasks(nil, 100)
	if len(tasks) != 2 {
		t.Fatalf("automatic retry created tasks: %d", len(tasks))
	}
}

func TestIngestorConsumesOversizedLines(t *testing.T) {
	dataDir := t.TempDir()
	st, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	ing := newIngestor(st, dataDir, log.New(io.Discard, "", 0))
	dir := ing.taskDir(task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(v map[string]any) string {
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	// A line above the per-pass batch budget must still be ingested, and one
	// above the hard line limit must be skipped, not wedge the reader.
	content := line(map[string]any{"seq": 1, "stream": "notice", "text": "before"}) +
		line(map[string]any{"seq": 2, "stream": "text", "text": strings.Repeat("a", 3<<20)}) +
		line(map[string]any{"seq": 3, "stream": "text", "text": strings.Repeat("b", ingestMaxLineBytes)}) +
		line(map[string]any{"seq": 4, "stream": "notice", "text": "after"}) +
		line(map[string]any{"seq": 5, "stream": "exit", "exit": map[string]any{
			"code": 0, "reason": "exited", "status": StatusCompleted}})
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var offset int64
	sawExit := false
	for i := 0; i < 20; i++ {
		advanced, exited := ing.readOnce(task.ID, &offset)
		sawExit = sawExit || exited
		if !advanced {
			break
		}
	}
	if offset != int64(len(content)) || !sawExit {
		t.Fatalf("offset %d of %d, sawExit %v", offset, len(content), sawExit)
	}
	evs, err := st.EventsAfter(sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range evs {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		texts = append(texts, e.Kind+":"+truncateTitle(p.Text, 12))
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"notice:before", "text:aaa", "notice:一条事件过大", "notice:after"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	got, _ := st.GetTask(task.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("task status %q", got.Status)
	}
}

func TestReadEventLineWaitsForNewline(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("complete\npartial"), 16)
	line, n, ok := readEventLine(br, 1<<20)
	if !ok || string(line) != "complete\n" || n != 9 {
		t.Fatalf("first line = %q %d %v", line, n, ok)
	}
	if _, _, ok := readEventLine(br, 1<<20); ok {
		t.Fatal("partial line reported as complete")
	}
}

func TestBuildAgentCommand(t *testing.T) {
	old := agents
	agents = &AgentSet{at: time.Now(), agents: []AgentInfo{
		{Name: "codex", Binary: "/bin/echo", Available: true},
		{Name: "claude", Binary: "/bin/echo", Available: true},
	}}
	defer func() { agents = old }()

	// Codex always runs app-server; the mode travels in the JSON-RPC requests.
	spec := TaskSpec{Agent: "codex", Cwd: "/tmp/work", IsNew: true, Profile: "read-only"}
	cmd, err := buildAgentCommand(spec, "/tmp/control")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(cmd.Args[1:], " "); args != "app-server" {
		t.Fatalf("codex args = %q, want app-server", args)
	}
	for profile, want := range map[string][3]string{
		"read-only":          {"read-only", "on-request", "default"},
		"workspace-write":    {"workspace-write", "on-request", "default"},
		"plan":               {"read-only", "on-request", "plan"},
		"danger-full-access": {"danger-full-access", "never", "default"},
		"workspace":          {"read-only", "on-request", "default"}, // unknown falls back
	} {
		sb, ap, mode := codexModes(profile)
		if [3]string{sb, ap, mode} != want {
			t.Fatalf("codexModes(%s) = %s %s %s", profile, sb, ap, mode)
		}
	}

	cspec := TaskSpec{Agent: "claude", Cwd: "/tmp/work", IsNew: true, NativeID: "11111111-2222-4333-8444-555555555555",
		PermTimeout: 60, Profile: "manual"}
	cmd, err = buildAgentCommand(cspec, "/tmp/control")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--input-format stream-json", "--output-format stream-json",
		"--permission-prompt-tool stdio", "--permission-mode default",
		"--session-id 11111111-2222-4333-8444-555555555555"} {
		if !strings.Contains(args, want) {
			t.Fatalf("claude new args %q missing %q", args, want)
		}
	}
	if strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("manual mode must not bypass permissions: %q", args)
	}

	// Every installed Claude mode is forwarded as-is.
	for _, mode := range []string{"acceptEdits", "plan", "auto", "dontAsk"} {
		cspec.Profile = mode
		cmd, _ = buildAgentCommand(cspec, "/tmp/control")
		modeArgs := strings.Join(cmd.Args, " ")
		if !strings.Contains(modeArgs, "--permission-mode "+mode) {
			t.Fatalf("claude mode %s not forwarded: %q", mode, modeArgs)
		}
		if strings.Contains(modeArgs, "--dangerously-skip-permissions") {
			t.Fatalf("claude mode %s unexpectedly bypasses permissions", mode)
		}
	}

	cspec.Profile = "bypassPermissions"
	cmd, _ = buildAgentCommand(cspec, "/tmp/control")
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--permission-mode bypassPermissions") ||
		!strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("claude bypass mode incomplete: %q", args)
	}
	cspec.Profile = "manual"
	cspec.IsNew = false
	cmd, _ = buildAgentCommand(cspec, "/tmp/control")
	args = strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--resume 11111111-2222-4333-8444-555555555555") ||
		strings.Contains(args, "--session-id") {
		t.Fatalf("claude resume args wrong: %q", args)
	}
}

func TestExtractNativeID(t *testing.T) {
	cases := []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"thread_id": "t1"}, "t1"},
		{map[string]any{"session_id": "s1"}, "s1"},
		{map[string]any{"subtype": "init", "session_id": "s2"}, "s2"},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := extractNativeID(c.raw); got != c.want {
			t.Fatalf("extractNativeID(%v) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestNormalizeEvents(t *testing.T) {
	claude := map[string]any{
		"type": "assistant",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
	}
	evs := normalizeClaude(claude)
	if len(evs) != 2 || evs[0].Kind != "assistant" || evs[1].Kind != "tool" {
		t.Fatalf("claude normalize = %+v", evs)
	}

	codex := map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "command_execution", "id": "c1", "status": "completed",
			"command": "echo hi", "aggregated_output": "hi", "exit_code": 0},
	}
	evs = normalizeCodex(codex)
	if len(evs) != 1 || evs[0].Kind != "tool" {
		t.Fatalf("codex normalize = %+v", evs)
	}
	payload := evs[0].Payload.(map[string]any)
	if payload["name"] != "shell" {
		t.Fatalf("codex tool payload = %+v", payload)
	}
}

func TestImportExcludeRootsNormalize(t *testing.T) {
	cfg := defaultConfig()
	cfg.Secret = "c2VjcmV0"
	cfg.PasswordHash = hashPassword("x")
	cfg.ImportExcludeRoots = []string{"/tmp/ac-*", "/tmp/ac-*", "/tmp/ac-*/"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ImportExcludeRoots) != 1 || cfg.ImportExcludeRoots[0] != "/tmp/ac-*" {
		t.Fatalf("normalized excludes = %v", cfg.ImportExcludeRoots)
	}
	cfg.ImportExcludeRoots = []string{"relative/path"}
	if err := cfg.normalize(); err == nil {
		t.Fatal("relative exclude root accepted")
	}
	cfg.ImportExcludeRoots = []string{"/"}
	if err := cfg.normalize(); err == nil {
		t.Fatal("root directory accepted as exclude root")
	}
}

func TestIsExcludedCWD(t *testing.T) {
	roots := []string{"/tmp/ac-*"}
	for _, cwd := range []string{"/tmp/ac-e2e", "/tmp/ac-e2e/work", "/tmp/ac-prod/work"} {
		if !isExcludedCWD(cwd, roots) {
			t.Fatalf("%q should be excluded", cwd)
		}
	}
	for _, cwd := range []string{"/tmp/accounting", "/tmp/ac", "/tmp/acct"} {
		if isExcludedCWD(cwd, roots) {
			t.Fatalf("%q should not be excluded", cwd)
		}
	}
	if !isExcludedCWD("/tmp/ac-e2e/work", []string{"/tmp/ac-e2e"}) {
		t.Fatal("plain root should cover descendants")
	}
	if isExcludedCWD("/tmp/ac-e2e2", []string{"/tmp/ac-e2e"}) {
		t.Fatal("plain root must match path components only")
	}
}

func TestImporterRescanSkipsUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		line, _ := json.Marshal(map[string]any{
			"timestamp": "2026-01-01T00:00:00Z", "sessionId": strings.TrimSuffix(name, ".jsonl"),
			"cwd": home, "type": "user", "message": map[string]any{"content": text},
		})
		if err := os.WriteFile(filepath.Join(dir, name), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("11111111-2222-4333-8444-555555555555.jsonl", "hello")
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("first scan = %+v", rep)
	}
	// Nothing changed: the rescan must not re-import anything.
	if rep := im.ImportAll(); rep.Imported != 0 || rep.Updated != 0 || rep.Skipped != 1 {
		t.Fatalf("idle rescan = %+v", rep)
	}
	// A conversation started in a terminal after the first scan is picked up.
	write("66666666-7777-4888-9999-000000000000.jsonl", "new one")
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("rescan after new file = %+v", rep)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 2 {
		t.Fatalf("sessions = %d", len(got))
	}
}

func TestImporterSkipsExcludedCWD(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "test-work")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"timestamp": "2026-01-01T00:00:00Z",
		"sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd":       work,
		"type":      "user",
		"message":   map[string]any{"content": "hello"},
	})
	file := filepath.Join(home, ".claude", "projects", "proj", "s.jsonl")
	if err := os.WriteFile(file, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := log.New(io.Discard, "", 0)

	im := &Importer{store: st, home: home, log: logger}
	rep := im.ImportAll()
	if rep.Excluded != 0 || rep.Imported != 1 {
		t.Fatalf("unexcluded import = %+v", rep)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 1 {
		t.Fatalf("sessions after import = %d", len(got))
	}

	st2, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	im2 := &Importer{store: st2, home: home, log: logger, excludeRoots: []string{work}}
	rep = im2.ImportAll()
	if rep.Excluded != 1 || rep.Imported != 0 {
		t.Fatalf("excluded import = %+v", rep)
	}
	if got, _ := st2.ListSessions("", "", true); len(got) != 0 {
		t.Fatalf("excluded session was imported: %d", len(got))
	}
}

func TestPIDFilePathSeparatesInstances(t *testing.T) {
	a := pidFilePath([]string{"127.0.0.1:8899", "10.0.0.6:8899"})
	b := pidFilePath([]string{"127.0.0.1:8901"})
	if a == b {
		t.Fatalf("instances share pid path: %s", a)
	}
	if !strings.HasSuffix(a, "agentconsole-8899.pid") || !strings.HasSuffix(b, "agentconsole-8901.pid") {
		t.Fatalf("unexpected pid paths: %s %s", a, b)
	}
	if got := pidFilePath(nil); !strings.HasSuffix(got, "agentconsole-default.pid") {
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
	unit := systemdUnit("/usr/local/bin/agentconsole", "/etc/agentconsole/config.json")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/agentconsole serve --config /etc/agentconsole/config.json",
		"WorkingDirectory=/usr/local/bin",
		"KillMode=process", // detached tasks must survive service restarts
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

// writeClaudeLine appends one Claude transcript record.
func appendClaudeRecords(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range records {
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func claudeRecord(ts, typ, uuid string, content any) map[string]any {
	if s, ok := content.(string); ok && typ != "user" {
		// Assistant records carry typed content blocks, never a bare string.
		content = []any{map[string]any{"type": "text", "text": s}}
	}
	return map[string]any{
		"timestamp": ts, "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": os.Getenv("TMPDIR"), "type": typ, "uuid": uuid,
		"message": map[string]any{"content": content},
	}
}

// A session continued in the CLI after the startup snapshot must pick up the
// new tail when it is opened, otherwise the last answer never appears.
func TestSyncSessionAppendsNewNativeEvents(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}

	rec := func(ts, typ, uuid, text string) map[string]any {
		r := claudeRecord(ts, typ, uuid, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:00Z", "user", "u1", "first question"),
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "first answer"),
	)
	if rep := im.ImportAll(); rep.Imported != 1 {
		t.Fatalf("import = %+v", rep)
	}
	sess, err := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := st.SessionStats(sess.ID)
	if before != 2 {
		t.Fatalf("events before = %d", before)
	}

	// Continue the conversation natively, as a CLI session would.
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:01:00Z", "user", "u2", "second question"),
		rec("2026-01-01T00:01:10Z", "assistant", "a2", "second answer"),
	)
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	after, _, _ := st.SessionStats(sess.ID)
	if after != 4 {
		t.Fatalf("events after sync = %d, want 4", after)
	}
	got, err := st.RecentEvents(sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	last := string(got[len(got)-1].Payload)
	if !strings.Contains(last, "second answer") {
		t.Fatalf("last event = %s", last)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.LastMessageAt != "2026-01-01T00:01:10Z" {
		t.Fatalf("last_message_at = %q", fresh.LastMessageAt)
	}

	// A second sync with no new data must not duplicate anything.
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := st.SessionStats(sess.ID); again != 4 {
		t.Fatalf("events after idle sync = %d, want 4", again)
	}
}

// A console-created session continued with `claude --resume` in a terminal
// must show the terminal turns, but not repeat the turns the console ran.
func TestSyncSessionPicksUpTerminalTurnsOfWebSession(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "11111111-2222-4333-8444-555555555555.jsonl")
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}

	sess := &Session{Agent: "claude", NativeID: "11111111-2222-4333-8444-555555555555",
		Cwd: home, Title: "新会话 · Claude Code", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	// The console ran the first turn; its events came from the task log.
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: home, Prompt: "web question",
		Status: StatusCompleted, StartedAt: "2026-01-01T00:00:00Z", EndedAt: "2026-01-01T00:00:30Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"web question", "web answer"} {
		p, _ := json.Marshal(map[string]any{"text": txt})
		if _, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: int64(i + 1),
			TS: "2026-01-01T00:00:1" + strconv.Itoa(i) + "Z", Kind: "text", Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	// The native transcript holds that console turn plus a terminal turn.
	appendClaudeRecords(t, path,
		claudeRecord("2026-01-01T00:00:10Z", "user", "u1", "web question"),
		claudeRecord("2026-01-01T00:00:20Z", "assistant", "a1", "web answer"),
		claudeRecord("2026-01-01T01:00:00Z", "user", "u2", "terminal question"),
		claudeRecord("2026-01-01T01:00:10Z", "assistant", "a2", "terminal answer"),
	)
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	var all []string
	for _, e := range evs {
		all = append(all, string(e.Payload))
	}
	joined := strings.Join(all, "|")
	if len(evs) != 4 || strings.Count(joined, "web answer") != 1 || !strings.Contains(joined, "terminal answer") {
		t.Fatalf("events after sync (%d): %s", len(evs), joined)
	}
	// Idle sync and the periodic rescan must not duplicate anything.
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	im.ImportAll()
	if n, _, _ := st.SessionStats(sess.ID); n != 4 {
		t.Fatalf("events after resync = %d, want 4", n)
	}
	if got, _ := st.ListSessions("", "", true); len(got) != 1 {
		t.Fatalf("rescan created a duplicate session: %d", len(got))
	}
}

func TestSyncSessionSkipsPartialLine(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "hello")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// A writer that has flushed only half a record must not corrupt the cursor.
	full, _ := json.Marshal(func() map[string]any {
		x := claudeRecord("2026-01-01T00:02:00Z", "assistant", "a2", "half written")
		x["cwd"] = work
		return x
	}())
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if _, err := f.Write(full[:len(full)/2]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 1 {
		t.Fatalf("partial line was imported: %d", n)
	}

	// Completing the record lets the next sync pick it up exactly once.
	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if _, err := f.Write(append(full[len(full)/2:], '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("completed line not imported: %d", n)
	}
}

// Console-run turns are already in the session from the task log; the same
// records in the native transcript must not be appended a second time.
func TestSyncSessionSkipsConsoleTaskWindows(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "start")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// A finished console task covering 00:01–00:02.
	task := &Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console turn", Status: StatusCompleted, CreatedAt: "2026-01-01T00:01:00Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE tasks SET started_at=?, ended_at=? WHERE id=?`,
		"2026-01-01T00:01:00Z", "2026-01-01T00:02:00Z", task.ID); err != nil {
		t.Fatal(err)
	}

	ins := map[string]any{"timestamp": "2026-01-01T00:01:30Z", "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": work, "type": "assistant", "uuid": "a-console",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "console answer"}}}}
	after := map[string]any{"timestamp": "2026-01-01T00:03:00Z", "sessionId": "11111111-2222-4333-8444-555555555555",
		"cwd": work, "type": "assistant", "uuid": "a-cli",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "cli answer"}}}}
	appendClaudeRecords(t, path, ins, after)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	var texts []string
	for _, e := range evs {
		texts = append(texts, string(e.Payload))
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, "console answer") {
		t.Fatal("console turn was imported twice")
	}
	if !strings.Contains(joined, "cli answer") {
		t.Fatal("native-only turn was not imported")
	}
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("events = %d, want 2 (start + cli answer)", n)
	}
}

// A user rename must survive later syncs; an auto title may be upgraded.
func TestSyncSessionPreservesUserTitle(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "auto title source")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	userTitle := "我的自定义标题"
	if err := st.UpdateSession(sess.ID, &userTitle, nil); err != nil {
		t.Fatal(err)
	}

	appendClaudeRecords(t, path, map[string]any{"timestamp": "2026-01-01T00:05:00Z",
		"sessionId": "11111111-2222-4333-8444-555555555555", "cwd": work,
		"type": "ai-title", "aiTitle": "Generated title"})
	appendClaudeRecords(t, path, func() map[string]any {
		x := claudeRecord("2026-01-01T00:06:00Z", "assistant", "a2", "later")
		x["cwd"] = work
		return x
	}())
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.Title != userTitle {
		t.Fatalf("title = %q, want %q", fresh.Title, userTitle)
	}
}

// Legacy snapshots were taken once at startup and carry no cursor. Opening such
// a session must backfill everything the native file gained since.
func TestSyncSessionBackfillsLegacySnapshot(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	// Claude names transcripts after the session id, which is how a legacy
	// snapshot without a stored path is located again.
	path := filepath.Join(proj, "11111111-2222-4333-8444-555555555555.jsonl")
	rec := func(ts, typ, uuid, text string) map[string]any {
		r := claudeRecord(ts, typ, uuid, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "question"))

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	// Simulate the pre-fix snapshot: events imported, but no cursor recorded.
	if _, err := st.db.Exec(`UPDATE sessions SET meta='{}' WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "answer"),
		rec("2026-01-01T00:00:09Z", "assistant", "a2", "latest answer"),
	)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if last := string(evs[len(evs)-1].Payload); !strings.Contains(last, "latest answer") {
		t.Fatalf("last = %s", last)
	}
	fresh, _ := st.GetSession(sess.ID)
	if fresh.LastMessageAt != "2026-01-01T00:00:09Z" {
		t.Fatalf("last_message_at = %q", fresh.LastMessageAt)
	}
	// The cursor is now recorded, so the next open is a no-op.
	if stt := parseImportState(st.GetSessionMeta(sess.ID)); stt.Offset <= 0 {
		t.Fatalf("cursor not persisted: %+v", stt)
	}
}

// A native file that shrank (rewritten/compacted) invalidates the cursor and
// must be re-read in full instead of waiting for bytes past the old offset.
func TestSyncSessionReimportsAfterTruncation(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "keep me")
	r["cwd"] = work
	appendClaudeRecords(t, path, r,
		func() map[string]any {
			x := claudeRecord("2026-01-01T00:00:05Z", "assistant", "a1", strings.Repeat("old ", 200))
			x["cwd"] = work
			return x
		}())

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")
	if n, _, _ := st.SessionStats(sess.ID); n != 2 {
		t.Fatalf("events = %d", n)
	}

	// Rewrite the transcript shorter with different content.
	var b strings.Builder
	for _, rec := range []map[string]any{
		r,
		func() map[string]any {
			x := claudeRecord("2026-01-01T00:00:07Z", "assistant", "a2", "new")
			x["cwd"] = work
			return x
		}(),
	} {
		raw, _ := json.Marshal(rec)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	if len(evs) != 2 {
		t.Fatalf("events after rewrite = %d, want 2", len(evs))
	}
	if last := string(evs[len(evs)-1].Payload); !strings.Contains(last, "\"new\"") {
		t.Fatalf("last = %s", last)
	}
}

// Re-importing must never renumber a session that also holds console-run turns:
// their events would jump after the imported history.
func TestReimportFullKeepsMixedSession(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "native")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	task := &Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console", Status: StatusCompleted, CreatedAt: "2026-01-01T00:10:00Z"}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO events (session_id, task_id, task_seq, ts, kind, payload)
		VALUES (?, ?, 1, ?, 'user', ?)`, sess.ID, task.ID, "2026-01-01T00:10:00Z",
		`{"text":"console turn"}`); err != nil {
		t.Fatal(err)
	}
	before, _, _ := st.SessionStats(sess.ID)

	// Force the full-reimport path.
	if _, err := st.db.Exec(`UPDATE sessions SET meta='{}' WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	after, _, _ := st.SessionStats(sess.ID)
	if after != before {
		t.Fatalf("mixed session was rewritten: %d -> %d", before, after)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	found := false
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "console turn") {
			found = true
		}
	}
	if !found {
		t.Fatal("console turn lost")
	}
}

// A console task that is still running has no ended_at; its native records are
// already arriving from the task log and must not be appended by a sync.
func TestSyncSessionSkipsRunningTaskWindow(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	r := claudeRecord("2026-01-01T00:00:00Z", "user", "u1", "start")
	r["cwd"] = work
	appendClaudeRecords(t, path, r)

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", "11111111-2222-4333-8444-555555555555")

	task := &Task{ID: "t1", SessionID: sess.ID, Agent: "claude", Cwd: work,
		Prompt: "console", Status: StatusRunning, CreatedAt: nowRFC3339()}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	live := map[string]any{"timestamp": time.Now().UTC().Add(2 * time.Second).Format("2006-01-02T15:04:05.000Z"),
		"sessionId": "11111111-2222-4333-8444-555555555555", "cwd": work,
		"type": "assistant", "uuid": "a-live",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "live console turn"}}}}
	appendClaudeRecords(t, path, live)

	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.RecentEvents(sess.ID, 20)
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "live console turn") {
			t.Fatal("running console task was imported a second time")
		}
	}
}

// A lone pending task used to report "前面还有 1 个任务" because the query
// counted the task itself; nothing else was ever in the list.
func TestQueuePositionCountsOnlyWorkAhead(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "codex", Cwd: "/tmp", Title: "q"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	first := &Task{SessionID: sess.ID, Agent: "codex", Cwd: "/tmp", Prompt: "one",
		Status: StatusPending, CreatedAt: "2026-01-01T00:00:00Z"}
	second := &Task{SessionID: sess.ID, Agent: "codex", Cwd: "/tmp", Prompt: "two",
		Status: StatusPending, CreatedAt: "2026-01-01T00:00:01Z"}
	for _, task := range []*Task{first, second} {
		if err := st.CreateTask(task); err != nil {
			t.Fatal(err)
		}
	}
	pos, err := st.QueuePosition(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Fatalf("first queue position = %d, want 1", pos)
	}
	if pos, _ := st.QueuePosition(second.ID); pos != 2 {
		t.Fatalf("second queue position = %d, want 2", pos)
	}

	// A running task on the session moves the queue, and a different session
	// never contributes to it.
	if err := st.MarkTaskRunning(first.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	if pos, _ := st.QueuePosition(second.ID); pos != 2 {
		t.Fatalf("with one running task, position = %d, want 2", pos)
	}
	other := &Session{Agent: "claude", Cwd: "/tmp", Title: "other"}
	if err := st.CreateSession(other); err != nil {
		t.Fatal(err)
	}
	foreign := &Task{SessionID: other.ID, Agent: "claude", Cwd: "/tmp", Prompt: "x",
		Status: StatusPending, CreatedAt: "2026-01-01T00:00:00Z"}
	if err := st.CreateTask(foreign); err != nil {
		t.Fatal(err)
	}
	if pos, _ := st.QueuePosition(second.ID); pos != 2 {
		t.Fatalf("another session changed the position to %d", pos)
	}
	// A task that is not waiting has no position.
	if pos, _ := st.QueuePosition(first.ID); pos != 0 {
		t.Fatalf("running task position = %d, want 0", pos)
	}
}

// A pending task must not count as occupying its own session. That made every
// queued task block itself, so "前面的任务结束后自动执行" never happened.
func TestSessionRunningCheckIgnoresPendingSelf(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "codex", Cwd: "/tmp", Title: "s"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	pending := &Task{SessionID: sess.ID, Agent: "codex", Cwd: "/tmp", Prompt: "p",
		Status: StatusPending}
	if err := st.CreateTask(pending); err != nil {
		t.Fatal(err)
	}
	running, err := st.SessionHasRunningTask(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("pending task counted as running; scheduler would never start it")
	}
	// It still blocks deletion, which must not orphan queued work.
	if active, _ := st.SessionHasActiveTask(sess.ID); !active {
		t.Fatal("pending task must keep the session from being deleted")
	}
	if err := st.MarkTaskRunning(pending.ID, 1, 1, "boot"); err != nil {
		t.Fatal(err)
	}
	if running, _ := st.SessionHasRunningTask(sess.ID); !running {
		t.Fatal("running task not reported")
	}
}

// The scheduler must be able to start every queued task: a pending task does
// not occupy its own session, and different sessions run in parallel.
func TestPickPendingStartsQueuedTasks(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	a := &Session{Agent: "codex", Cwd: "/tmp", Title: "a"}
	b := &Session{Agent: "claude", Cwd: "/tmp", Title: "b"}
	for _, sess := range []*Session{a, b} {
		if err := st.CreateSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(sess *Session, created string) *Task {
		task := &Task{SessionID: sess.ID, Agent: sess.Agent, Cwd: "/tmp", Prompt: "p",
			Status: StatusPending, CreatedAt: created}
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
	if err := st.FinishTask(a1.ID, StatusCompleted, nil, "", nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingTasks(20)
	if pick := srv.pickPending(pending); pick == nil || pick.ID != a2.ID {
		t.Fatalf("pick after completion = %v, want %s", pick, a2.ID)
	}
}

// An identifier that becomes a path component must never be able to escape the
// data directory ("../" or an absolute path read another events.jsonl).
func TestValidIDRejectsPathComponents(t *testing.T) {
	good := []string{"V5NPljRYl82AWam0", "abc-DEF_123", "a", "c3d1e3ab-e4b9-4d8e-b0ff-b5b46ce98e36"}
	for _, s := range good {
		if !validID(s) {
			t.Fatalf("validID(%q) = false", s)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/b", "a\\b", "/etc/passwd", "a b", "a\x00b",
		strings.Repeat("a", 65), "a:b", "a%2fb"}
	for _, s := range bad {
		if validID(s) {
			t.Fatalf("validID(%q) = true", s)
		}
	}
}

// handleTaskLog must reject a traversal id instead of joining it into a path.
func TestTaskLogRejectsTraversal(t *testing.T) {
	data := t.TempDir()
	st, err := openStore(data)
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
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	if !srv.auth.verifyPassword("correct horse battery staple") {
		t.Fatal("initial password rejected")
	}
	newHash := hashPassword("new secret")
	srv.updateConfig(func(c *Config) { c.PasswordHash = newHash; c.MaxConcurrent = 2 })
	if srv.auth.verifyPassword("correct horse battery staple") {
		t.Fatal("old password still accepted after config swap")
	}
	if !srv.auth.verifyPassword("new secret") {
		t.Fatal("new password rejected after config swap")
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
	st, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := testConfig(t)
	cfg.DataDir = data
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)

	sess := &Session{Agent: "claude", Cwd: data, Title: "d"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: StatusCompleted}
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
	other := &Task{SessionID: "other", Agent: "claude", Cwd: data, Prompt: "o", Status: StatusCompleted}
	if err := st.CreateTask(other); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(data, "tasks", other.ID)
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"password": "correct horse battery staple"})
	r := httptest.NewRequest("POST", "/api/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			ck = c
		}
	}
	var loginResp struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &loginResp)
	if loginResp.CSRF == "" {
		t.Fatal("login did not return a CSRF token")
	}

	r = httptest.NewRequest("DELETE", "/api/sessions/"+sess.ID+"?confirm="+sess.ID, nil)
	r.AddCookie(ck)
	r.Header.Set("X-CSRF-Token", loginResp.CSRF)
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

// Reconcile must not declare a task interrupted when launch() marked it running
// but has not recorded the runner PID yet: there is no evidence it is gone.
func TestReconcileKeepsTaskWithoutPID(t *testing.T) {
	data := t.TempDir()
	st, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: data, Title: "r"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, 0, 0, currentBootID()); err != nil {
		t.Fatal(err)
	}
	ing := newIngestor(st, data, log.New(io.Discard, "", 0))
	ing.Reconcile()
	got, _ := st.GetTask(task.ID)
	if got.Status == StatusInterrupted {
		t.Fatalf("task without a recorded runner PID was declared interrupted: %q", got.Error)
	}
}

// A task from a previous boot must be interrupted even if its PID was reused.
func TestReconcileInterruptsPreviousBoot(t *testing.T) {
	data := t.TempDir()
	st, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: data, Title: "r"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: data, Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskRunning(task.ID, os.Getpid(), os.Getpid(), "boot-from-before"); err != nil {
		t.Fatal(err)
	}
	ing := newIngestor(st, data, log.New(io.Discard, "", 0))
	ing.Reconcile()
	got, _ := st.GetTask(task.ID)
	if got.Status != StatusInterrupted {
		t.Fatalf("previous-boot task = %s, want interrupted", got.Status)
	}
}

// Signing session cookies with an empty secret uses a publicly known HMAC key,
// so a config without one must be rejected instead of starting insecurely.
func TestConfigRejectsEmptySecret(t *testing.T) {
	cfg := defaultConfig()
	cfg.PasswordHash = hashPassword("x")
	cfg.Secret = ""
	if err := cfg.normalize(); err == nil {
		t.Fatal("empty secret accepted")
	}
	cfg.Secret = "   "
	if err := cfg.normalize(); err == nil {
		t.Fatal("blank secret accepted")
	}
	cfg.Secret = "c2VjcmV0"
	if err := cfg.normalize(); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
	cfg.Secret = "c2VjcmV0"
	cfg.PasswordHash = ""
	if err := cfg.normalize(); err == nil {
		t.Fatal("empty password hash accepted")
	}
}

// Changing the password must invalidate cookies issued under the old secret.
func TestPasswordChangeInvalidatesOldTokens(t *testing.T) {
	cfg := testConfig(t)
	auth := newAuthManager(cfg)
	tok := auth.token()
	if _, ok := auth.verifyToken(tok); !ok {
		t.Fatal("fresh token rejected")
	}
	// rotate the secret the way a password change does
	next := *cfg
	next.Secret = base64.StdEncoding.EncodeToString([]byte("a-brand-new-secret-0123456789"))
	next.PasswordHash = hashPassword("new")
	auth.setConfig(&next)
	if _, ok := auth.verifyToken(tok); ok {
		t.Fatal("token still valid after secret rotation")
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

// Two concurrent opens of the same session (detail + events tail are fetched in
// parallel by the page) must not append the same native tail twice.
func TestConcurrentSyncDoesNotDuplicate(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	os.MkdirAll(work, 0o700)
	proj := filepath.Join(home, ".claude", "projects", "p")
	os.MkdirAll(proj, 0o700)
	uuid := "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(proj, uuid+".jsonl")
	rec := func(ts, typ, id, text string) map[string]any {
		r := claudeRecord(ts, typ, id, text)
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "q"))

	st, _ := openStore(t.TempDir())
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, _ := st.FindNativeSession("claude", uuid)

	// The CLI appends while the page opens.
	appendClaudeRecords(t, path,
		rec("2026-01-01T00:00:05Z", "assistant", "a1", "answer one"),
		rec("2026-01-01T00:00:09Z", "assistant", "a2", "answer two"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = im.SyncSession(sess.ID) }()
	}
	wg.Wait()
	evs, _ := st.RecentEvents(sess.ID, 50)
	t.Logf("events=%d", len(evs))
	seen := map[string]int{}
	for _, e := range evs {
		var p map[string]any
		json.Unmarshal(e.Payload, &p)
		if s, ok := p["text"].(string); ok {
			seen[s]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("%q imported %d times", k, n)
		}
	}
	if len(evs) != 3 {
		t.Fatalf("events=%d, want 3", len(evs))
	}
}

// A page of the oldest pending tasks must not be filled by tasks whose session
// is busy: with 200 such tasks every newer task on a free session was starved.
func TestRunnablePendingTasksSkipsBusySessions(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	busy := &Session{Agent: "claude", Cwd: "/tmp", Title: "busy"}
	if err := st.CreateSession(busy); err != nil {
		t.Fatal(err)
	}
	run := &Task{SessionID: busy.ID, Agent: "claude", Cwd: "/tmp", Prompt: "run", Status: StatusRunning}
	if err := st.CreateTask(run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = st.CreateTask(&Task{SessionID: busy.ID, Agent: "claude", Cwd: "/tmp",
			Prompt: "blocked", Status: StatusPending, CreatedAt: "2026-01-01T00:00:0" + string(rune('0'+i)) + "Z"})
	}
	free := &Session{Agent: "claude", Cwd: "/tmp", Title: "free"}
	if err := st.CreateSession(free); err != nil {
		t.Fatal(err)
	}
	want := &Task{SessionID: free.ID, Agent: "claude", Cwd: "/tmp", Prompt: "new",
		Status: StatusPending, CreatedAt: "2026-02-01T00:00:00Z"}
	if err := st.CreateTask(want); err != nil {
		t.Fatal(err)
	}
	all, err := st.PendingTasks(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range all {
		if task.ID == want.ID {
			t.Fatal("test setup: the free-session task was inside the page; raise the blocking count")
		}
	}
	got, err := st.RunnablePendingTasks(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("runnable pending = %d tasks, want only %s", len(got), want.ID)
	}
}

// A transcript rewritten in place (same path, same size) is a different file;
// reading from the old offset would splice two conversations together.
func TestSyncSessionDetectsInPlaceRewrite(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	uuid := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	path := filepath.Join(proj, uuid+".jsonl")
	rec := func(ts, typ, id, text string) map[string]any {
		r := claudeRecord(ts, typ, id, text)
		r["sessionId"] = uuid
		r["cwd"] = work
		return r
	}
	appendClaudeRecords(t, path, rec("2026-01-01T00:00:00Z", "user", "u1", "original question"))

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	sess, err := st.FindNativeSession("claude", uuid)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the file: a new inode with the same name. The content is longer
	// but the point is that the identity changed, not the size.
	tmp := path + ".new"
	appendClaudeRecords(t, tmp,
		rec("2026-01-02T00:00:00Z", "user", "u2", "rewritten question"),
		rec("2026-01-02T00:00:01Z", "assistant", "a2", "rewritten answer"))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := im.SyncSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := st.RecentEvents(sess.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if s, ok := p["text"].(string); ok {
			texts = append(texts, s)
		}
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "original question") {
		t.Fatalf("stale snapshot kept after replacement: %q", joined)
	}
	if !strings.Contains(joined, "rewritten answer") {
		t.Fatalf("replacement not imported: %q", joined)
	}
}

func TestLaterTSComparesInstants(t *testing.T) {
	// Lexically "2026-01-01T09:00:00+08:00" sorts before "...T02:00:00Z", but as
	// instants the first is 01:00 UTC and the second is 02:00 UTC.
	a := "2026-01-01T09:00:00+08:00"
	b := "2026-01-01T02:00:00Z"
	if got := laterTS(a, b); got != b {
		t.Fatalf("laterTS(%q, %q) = %q, want %q", a, b, got, b)
	}
	if got := laterTS("", b); got != b {
		t.Fatalf("laterTS(\"\", b) = %q", got)
	}
	if got := laterTS(a, ""); got != a {
		t.Fatalf("laterTS(a, \"\") = %q", got)
	}
}

func TestUnknownAPIPathReturns404(t *testing.T) {
	st, err := openStore(t.TempDir())
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

// A late exit record must not rewrite an outcome that was already recorded
// (for example the escalated cancel that finished the task first).
func TestFinishTaskKeepsTerminalStatus(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTask(task.ID, StatusCancelled, nil, "用户取消", nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := st.FinishTask(task.ID, StatusCompleted, &code, "", nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(task.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("late exit rewrote terminal status to %q", got.Status)
	}
}

// ---------------------------------------------------------------------------
// One-click update / restart
// ---------------------------------------------------------------------------

func TestAgentVersionExtraction(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.163.0":     "0.163.0",
		"2.1.304 (Claude Code)": "2.1.304",
		"v1.2.3\n":              "1.2.3",
		"1.2":                   "1.2",
		"no version here":       "no version here",
		"":                      "",
	}
	for in, want := range cases {
		if got := agentVersion(in); got != want {
			t.Errorf("agentVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareAgentVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.163.0", "0.164.0", -1},
		{"0.164.0", "0.163.0", 1},
		{"2.1.304", "2.1.304", 0},
		{"2.1", "2.1.0", 0},
		{"2.10.0", "2.9.9", 1},
	}
	for _, c := range cases {
		if got := compareAgentVersions(c.a, c.b); got != c.want {
			t.Errorf("compareAgentVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// A binary inside node_modules belongs to npm; a native Claude Code install
// must use its own updater, because npm would update a different copy.
func TestAgentUpdateMethod(t *testing.T) {
	dir := t.TempDir()
	npmPath := filepath.Join(dir, "lib", "node_modules", "@openai", "codex", "bin", "codex.js")
	if got := agentUpdateMethod("codex", npmPath); got != "npm" {
		t.Errorf("node_modules codex method = %q, want npm", got)
	}
	if got := agentUpdateMethod("claude", filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")); got != "npm" {
		t.Errorf("node_modules claude method = %q, want npm", got)
	}
	if got := agentUpdateMethod("claude", filepath.Join(dir, ".local", "share", "claude", "versions", "2.1.304")); got != "native" {
		t.Errorf("native claude method = %q, want native", got)
	}
	if got := agentUpdateMethod("codex", ""); got != "npm" {
		t.Errorf("missing binary method = %q, want npm", got)
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
	st, err := openStore(t.TempDir())
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
	newSrv := func(cfg *Config) *Server {
		st, err := openStore(cfg.DataDir)
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
	if err := node.store.CreateSession(&Session{Agent: "claude", Cwd: nodeCfg.DataDir, Title: "on the mac"}); err != nil {
		t.Fatal(err)
	}
	ns := httptest.NewServer(node.Handler())
	defer ns.Close()

	hubCfg := testConfig(t)
	hubCfg.Nodes = []NodeConfig{{ID: "mac", Name: "Mac", URL: ns.URL, Token: "node-secret"},
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
	if rec := do("POST", "/n/mac/api/logout"); rec.Code != 403 {
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
	st, err := openStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := newServer(cfg, "", st, log.New(io.Discard, "", 0), nil)
	sess := &Session{Agent: "claude", Cwd: cfg.DataDir, Title: "t"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: cfg.DataDir, Prompt: "p", Status: StatusWaitingInput}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	perm := &Permission{TaskID: task.ID, SessionID: sess.ID, ToolName: "Bash", Input: json.RawMessage(`{}`), Status: "pending"}
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

func TestImporterSkipsCodexSubagentThreads(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "21")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id string, source any) {
		meta, _ := json.Marshal(map[string]any{"timestamp": "2026-10-21T00:00:00Z", "type": "session_meta",
			"payload": map[string]any{"id": id, "cwd": home, "timestamp": "2026-10-21T00:00:00Z", "source": source}})
		msg, _ := json.Marshal(map[string]any{"timestamp": "2026-10-21T00:00:01Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}})
		_ = os.WriteFile(filepath.Join(dir, "rollout-2026-10-21T00-00-00-"+id+".jsonl"), append(append(meta, '\n'), append(msg, '\n')...), 0o600)
	}
	write("01a15ee8-0000-7000-8000-000000000001", "cli")
	write("01a15ee8-0000-7000-8000-000000000002", map[string]any{"subagent": map[string]any{"other": "guardian"}})
	st, _ := openStore(t.TempDir())
	defer st.Close()
	im := &Importer{store: st, home: home, log: log.New(io.Discard, "", 0)}
	im.ImportAll()
	got, _ := st.ListSessions("", "", true)
	if len(got) != 1 || got[0].NativeID != "01a15ee8-0000-7000-8000-000000000001" {
		t.Fatalf("sessions = %d %+v", len(got), got)
	}
}
