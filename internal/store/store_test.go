package store

import (
	"encoding/json"
	"strings"
	"testing"

	"rymesh/internal/util"
)

func TestDeleteSessionRowCleansDependents(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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
		TS: util.NowRFC3339(), Kind: "assistant", Payload: json.RawMessage(`{"text":"hi"}`)}); err != nil {
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
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+`=?`, sess.ID).Scan(&n); err != nil {
			t.Fatalf("%s count: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s still has %d rows", table, n)
		}
	}
}

func TestStoreEventPagingAndIdempotency(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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
			TS: util.NowRFC3339(), Kind: "assistant", Payload: payload})
		if err != nil || !added {
			t.Fatalf("insert %d: added=%v err=%v", i, added, err)
		}
	}
	// Same (task, task_seq, sub) must be ignored for idempotent re-ingestion.
	if added, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: 5,
		TS: util.NowRFC3339(), Kind: "assistant", Payload: json.RawMessage(`{"text":"dup"}`)}); err != nil || added {
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
				TS: util.NowRFC3339(), Kind: kind, Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sess
}

func TestRecentTurnsPagesByTurn(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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
			TS: util.NowRFC3339(), Kind: kind, Payload: payload}); err != nil {
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
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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
	if err := st.FinishTask(t1.ID, StatusCompleted, &code, "", util.NowRFC3339()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(t1.ID)
	if err != nil || got.Status != StatusCompleted || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("finished task = %+v err %v", got, err)
	}
}

// A lone pending task used to report "前面还有 1 个任务" because the query
// counted the task itself; nothing else was ever in the list.
func TestQueuePositionCountsOnlyWorkAhead(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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
	st, err := OpenStore(t.TempDir())
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

// An identifier that becomes a path component must never be able to escape the
// data directory ("../" or an absolute path read another events.jsonl).
func TestValidIDRejectsPathComponents(t *testing.T) {
	good := []string{"V5NPljRYl82AWam0", "abc-DEF_123", "a", "c3d1e3ab-e4b9-4d8e-b0ff-b5b46ce98e36"}
	for _, s := range good {
		if !ValidID(s) {
			t.Fatalf("validID(%q) = false", s)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/b", "a\\b", "/etc/passwd", "a b", "a\x00b",
		strings.Repeat("a", 65), "a:b", "a%2fb"}
	for _, s := range bad {
		if ValidID(s) {
			t.Fatalf("validID(%q) = true", s)
		}
	}
}

// A page of the oldest pending tasks must not be filled by tasks whose session
// is busy: with 200 such tasks every newer task on a free session was starved.
func TestRunnablePendingTasksSkipsBusySessions(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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

// A late exit record must not rewrite an outcome that was already recorded
// (for example the escalated cancel that finished the task first).
func TestFinishTaskKeepsTerminalStatus(t *testing.T) {
	st, err := OpenStore(t.TempDir())
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
	if err := st.FinishTask(task.ID, StatusCancelled, nil, "用户取消", util.NowRFC3339()); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := st.FinishTask(task.ID, StatusCompleted, &code, "", util.NowRFC3339()); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetTask(task.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("late exit rewrote terminal status to %q", got.Status)
	}
}

// A live stream waits on Store.Changed; a new event or a status change must
// wake it instead of leaving it to the next poll.
func TestStoreChangedWakesOnWrite(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := &Session{Agent: "claude", Cwd: "/tmp", Title: "wake", Source: "web"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	task := &Task{SessionID: sess.ID, Agent: "claude", Cwd: "/tmp", Prompt: "p", Status: StatusRunning}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	woke := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	ch := st.Changed()
	if woke(ch) {
		t.Fatal("woken before any write")
	}
	if _, err := st.InsertEvent(&Event{SessionID: sess.ID, TaskID: task.ID, TaskSeq: 1,
		TS: util.NowRFC3339(), Kind: "assistant", Payload: json.RawMessage(`{"text":"hi"}`)}); err != nil {
		t.Fatal(err)
	}
	if !woke(ch) {
		t.Fatal("event insert did not wake the waiter")
	}

	ch = st.Changed()
	if woke(ch) {
		t.Fatal("a fresh channel is already closed")
	}
	if err := st.SetTaskWaiting(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if !woke(ch) {
		t.Fatal("status change did not wake the waiter")
	}
}
