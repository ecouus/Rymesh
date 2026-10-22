package console

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Session struct {
	ID               string `json:"id"`
	Agent            string `json:"agent"`
	NativeID         string `json:"native_id,omitempty"`
	Cwd              string `json:"cwd"`
	Title            string `json:"title"`
	Source           string `json:"source"`
	Hidden           bool   `json:"hidden"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	LastMessageAt    string `json:"last_message_at,omitempty"`
	SnapshotImported bool   `json:"snapshot_imported"`
	Meta             string `json:"-"`
}

type Task struct {
	ID              string `json:"id"`
	SessionID       string `json:"session_id"`
	Agent           string `json:"agent"`
	NativeID        string `json:"native_id,omitempty"`
	Cwd             string `json:"cwd"`
	Prompt          string `json:"prompt"`
	Status          string `json:"status"`
	Profile         string `json:"profile"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at,omitempty"`
	EndedAt         string `json:"ended_at,omitempty"`
	PID             int    `json:"pid,omitempty"`
	PGID            int    `json:"pgid,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Error           string `json:"error,omitempty"`
	CancelRequested bool   `json:"cancel_requested"`
	BootID          string `json:"-"`
	LogSize         int64  `json:"log_size"`
	IngestOffset    int64  `json:"-"`
	LastSeq         int64  `json:"last_seq"`
	RequestID       string `json:"request_id,omitempty"`
	Meta            string `json:"-"`
	SessionTitle    string `json:"session_title,omitempty"`
	SessionCwd      string `json:"session_cwd,omitempty"`
	// QueuePosition is this task's 1-based place in its session's queue (0 when
	// not waiting). QueueAhead is how many tasks it actually waits for, which is
	// what the UI says out loud; a lone queued task is first in line, not
	// "1 task ahead". Both are computed per response, not stored.
	QueuePosition int `json:"queue_position,omitempty"`
	QueueAhead    int `json:"queue_ahead,omitempty"`
}

type Event struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"session_id"`
	TaskID    string          `json:"task_id"`
	TaskSeq   int64           `json:"task_seq"`
	Sub       int             `json:"sub"`
	TS        string          `json:"ts"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

type Permission struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	SessionID  string          `json:"session_id"`
	ToolName   string          `json:"tool_name"`
	Input      json.RawMessage `json:"input"`
	Status     string          `json:"status"`
	CreatedAt  string          `json:"created_at"`
	ResolvedAt string          `json:"resolved_at,omitempty"`
	Response   string          `json:"response,omitempty"`
	// SessionTitle is filled by PendingPermissions so a hub can name the
	// conversation of an approval waiting on another device.
	SessionTitle string `json:"session_title,omitempty"`
	// Kind is permission, question or plan; Detail holds what the card needs
	// beyond tool and input (options, questions, plan text). See interact.go.
	Kind   string          `json:"kind"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

const (
	StatusPending      = "pending"
	StatusRunning      = "running"
	StatusWaitingInput = "waiting_input"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
	StatusInterrupted  = "interrupted"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func openStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "agentconsole.db")
	dsn := "file:" + path +
		"?_pragma=busy_timeout(8000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and keeps SQLITE_BUSY out of the picture.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds the client's SSH keys and node tokens (vault.go).
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(f, 0o600)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			agent TEXT NOT NULL,
			native_id TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'web',
			hidden INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_message_at TEXT NOT NULL DEFAULT '',
			snapshot_imported INTEGER NOT NULL DEFAULT 0,
			meta TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS sessions_native_idx
			ON sessions(agent, native_id) WHERE native_id <> ''`,
		`CREATE INDEX IF NOT EXISTS sessions_agent_idx ON sessions(agent, hidden, updated_at)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			agent TEXT NOT NULL,
			native_id TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL,
			prompt TEXT NOT NULL,
			status TEXT NOT NULL,
			profile TEXT NOT NULL DEFAULT 'read-only',
			created_at TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT '',
			ended_at TEXT NOT NULL DEFAULT '',
			pid INTEGER NOT NULL DEFAULT 0,
			pgid INTEGER NOT NULL DEFAULT 0,
			exit_code INTEGER,
			error TEXT NOT NULL DEFAULT '',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			boot_id TEXT NOT NULL DEFAULT '',
			log_size INTEGER NOT NULL DEFAULT 0,
			ingest_offset INTEGER NOT NULL DEFAULT 0,
			last_seq INTEGER NOT NULL DEFAULT 0,
			request_id TEXT NOT NULL DEFAULT '',
			meta TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status, created_at)`,
		`CREATE INDEX IF NOT EXISTS tasks_session_idx ON tasks(session_id, created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS tasks_request_idx ON tasks(request_id) WHERE request_id <> ''`,
		`CREATE TABLE IF NOT EXISTS events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			task_seq INTEGER NOT NULL,
			sub INTEGER NOT NULL DEFAULT 0,
			ts TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL,
			UNIQUE(task_id, task_seq, sub)
		)`,
		`CREATE INDEX IF NOT EXISTS events_session_idx ON events(session_id, seq)`,
		`CREATE TABLE IF NOT EXISTS permissions (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			input TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			resolved_at TEXT NOT NULL DEFAULT '',
			response TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS permissions_status_idx ON permissions(status, created_at)`,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
	}
	for _, q := range append(stmts, vaultSchema...) {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w\n%s", err, q)
		}
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS does
	// not touch an existing table, so they are added one by one.
	for _, c := range []struct{ table, column, def string }{
		{"permissions", "kind", `TEXT NOT NULL DEFAULT 'permission'`},
		{"permissions", "detail", `TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.addColumn(c.table, c.column, c.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addColumn(table, column, def string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	exists := false
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == column {
			exists = true
		}
	}
	rows.Close()
	if exists {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + def)
	return err
}

func (s *Store) CreateSession(sess *Session) error {
	if sess.ID == "" {
		sess.ID = randomID(12)
	}
	if sess.Source == "" {
		sess.Source = "web"
	}
	if sess.CreatedAt == "" {
		sess.CreatedAt = nowRFC3339()
	}
	sess.UpdatedAt = nowRFC3339()
	_, err := s.db.Exec(`INSERT INTO sessions
		(id,agent,native_id,cwd,title,source,hidden,created_at,updated_at,last_message_at,snapshot_imported,meta)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Agent, sess.NativeID, sess.Cwd, sess.Title, sess.Source,
		boolInt(sess.Hidden), sess.CreatedAt, sess.UpdatedAt, sess.LastMessageAt,
		boolInt(sess.SnapshotImported), defaultJSON(sess.Meta))
	return err
}

// UpsertNativeSession imports metadata without touching existing web sessions.
func (s *Store) UpsertNativeSession(sess *Session) (created bool, err error) {
	var existing string
	err = s.db.QueryRow(`SELECT id FROM sessions WHERE agent=? AND native_id=? AND native_id<>''`,
		sess.Agent, sess.NativeID).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	return true, s.CreateSession(sess)
}

func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id,agent,native_id,cwd,title,source,hidden,created_at,updated_at,
		last_message_at,snapshot_imported,meta FROM sessions WHERE id=?`, id)
	return scanSession(row)
}

func (s *Store) FindNativeSession(agent, nativeID string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id,agent,native_id,cwd,title,source,hidden,created_at,updated_at,
		last_message_at,snapshot_imported,meta FROM sessions
		WHERE agent=? AND native_id=? AND native_id<>''`, agent, nativeID)
	return scanSession(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(r rowScanner) (*Session, error) {
	var x Session
	var hidden, snap int
	err := r.Scan(&x.ID, &x.Agent, &x.NativeID, &x.Cwd, &x.Title, &x.Source, &hidden,
		&x.CreatedAt, &x.UpdatedAt, &x.LastMessageAt, &snap, &x.Meta)
	if err != nil {
		return nil, err
	}
	x.Hidden = hidden != 0
	x.SnapshotImported = snap != 0
	return &x, nil
}

func (s *Store) ListSessions(agent, query string, includeHidden bool) ([]*Session, error) {
	q := `SELECT id,agent,native_id,cwd,title,source,hidden,created_at,updated_at,
		last_message_at,snapshot_imported,meta FROM sessions WHERE 1=1`
	var args []any
	if !includeHidden {
		q += ` AND hidden=0`
	}
	if agent != "" {
		q += ` AND agent=?`
		args = append(args, agent)
	}
	if strings.TrimSpace(query) != "" {
		q += ` AND (title LIKE ? OR cwd LIKE ?)`
		like := "%" + strings.TrimSpace(query) + "%"
		args = append(args, like, like)
	}
	q += ` ORDER BY COALESCE(NULLIF(last_message_at,''), updated_at) DESC LIMIT 500`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSession(id string, title *string, hidden *bool) error {
	sets := []string{"updated_at=?"}
	args := []any{nowRFC3339()}
	if title != nil {
		sets = append(sets, "title=?")
		args = append(args, *title)
	}
	if hidden != nil {
		sets = append(sets, "hidden=?")
		args = append(args, boolInt(*hidden))
	}
	args = append(args, id)
	_, err := s.db.Exec(`UPDATE sessions SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

// UpdateSessionTitle renames a session without touching its timestamps, so a
// title upgrade does not reorder the sidebar as if a message had arrived.
func (s *Store) UpdateSessionTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title=? WHERE id=?`, title, id)
	return err
}

func (s *Store) SetSessionNativeID(id, nativeID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET native_id=?, updated_at=? WHERE id=? AND native_id=''`,
		nativeID, nowRFC3339(), id)
	if err != nil {
		return err
	}
	// If another web session already owns this native id, keep this one usable but do not
	// create a conflicting mapping.
	return nil
}

func (s *Store) TouchSession(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_message_at=?, updated_at=? WHERE id=?`,
		nowRFC3339(), nowRFC3339(), id)
	return err
}

func (s *Store) MarkSnapshotImported(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET snapshot_imported=1 WHERE id=?`, id)
	return err
}

func (s *Store) DeleteSessionRow(id string) error {
	// One transaction: a session row without its events, or events whose
	// session is gone, would both leave the console in an inconsistent state.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM sessions WHERE id=?`,
		`DELETE FROM tasks WHERE session_id=?`,
		`DELETE FROM events WHERE session_id=?`,
		`DELETE FROM permissions WHERE session_id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CreateTask(t *Task) error {
	if t.ID == "" {
		t.ID = randomID(12)
	}
	if t.CreatedAt == "" {
		t.CreatedAt = nowRFC3339()
	}
	if t.Status == "" {
		t.Status = StatusPending
	}
	if t.Profile == "" {
		t.Profile = "read-only"
	}
	_, err := s.db.Exec(`INSERT INTO tasks
		(id,session_id,agent,native_id,cwd,prompt,status,profile,created_at,started_at,ended_at,
		 pid,pgid,exit_code,error,cancel_requested,boot_id,log_size,ingest_offset,last_seq,request_id,meta)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SessionID, t.Agent, t.NativeID, t.Cwd, t.Prompt, t.Status, t.Profile,
		t.CreatedAt, t.StartedAt, t.EndedAt, t.PID, t.PGID, t.ExitCode, t.Error,
		boolInt(t.CancelRequested), t.BootID, t.LogSize, t.IngestOffset, t.LastSeq,
		t.RequestID, defaultJSON(t.Meta))
	return err
}

const taskSelect = `SELECT t.id,t.session_id,t.agent,t.native_id,t.cwd,t.prompt,t.status,t.profile,
	t.created_at,t.started_at,t.ended_at,t.pid,t.pgid,t.exit_code,t.error,t.cancel_requested,
	t.boot_id,t.log_size,t.ingest_offset,t.last_seq,t.request_id,t.meta,
	COALESCE(s.title,''),COALESCE(s.cwd,'')
	FROM tasks t LEFT JOIN sessions s ON s.id=t.session_id`

func scanTask(r rowScanner) (*Task, error) {
	var t Task
	var cancel int
	var exit sql.NullInt64
	err := r.Scan(&t.ID, &t.SessionID, &t.Agent, &t.NativeID, &t.Cwd, &t.Prompt, &t.Status,
		&t.Profile, &t.CreatedAt, &t.StartedAt, &t.EndedAt, &t.PID, &t.PGID, &exit,
		&t.Error, &cancel, &t.BootID, &t.LogSize, &t.IngestOffset, &t.LastSeq,
		&t.RequestID, &t.Meta, &t.SessionTitle, &t.SessionCwd)
	if err != nil {
		return nil, err
	}
	t.CancelRequested = cancel != 0
	if exit.Valid {
		v := int(exit.Int64)
		t.ExitCode = &v
	}
	return &t, nil
}

func (s *Store) GetTask(id string) (*Task, error) {
	return scanTask(s.db.QueryRow(taskSelect+` WHERE t.id=?`, id))
}

func (s *Store) ListTasks(statuses []string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := taskSelect
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE t.status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY t.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TasksForSession(sessionID string, limit int) ([]*Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(taskSelect+` WHERE t.session_id=? ORDER BY t.created_at ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ActiveTasks() ([]*Task, error) {
	rows, err := s.db.Query(taskSelect+` WHERE t.status IN (?,?,?) ORDER BY t.created_at ASC`,
		StatusRunning, StatusWaitingInput, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SessionHasActiveTask reports whether a session has running, waiting or
// queued work. Deleting a session uses it: queued work would otherwise start
// against a transcript that no longer exists.
func (s *Store) SessionHasActiveTask(sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session_id=? AND status IN (?,?,?)`,
		sessionID, StatusRunning, StatusWaitingInput, StatusPending).Scan(&n)
	return n > 0, err
}

// SessionHasRunningTask reports whether a session is occupied by an agent
// process. The scheduler uses only this: a pending task is not occupying the
// session, otherwise every queued task would block itself and never start.
func (s *Store) SessionHasRunningTask(sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session_id=? AND status IN (?,?)`,
		sessionID, StatusRunning, StatusWaitingInput).Scan(&n)
	return n > 0, err
}

func (s *Store) CountActiveTasks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status IN (?,?)`,
		StatusRunning, StatusWaitingInput).Scan(&n)
	return n, err
}

func (s *Store) MarkTaskRunning(id string, pid, pgid int, bootID string) error {
	// Only a task that is still live may become running: a state line buffered
	// before a cancel must not revive a task that is already terminal.
	_, err := s.db.Exec(`UPDATE tasks SET status=?, started_at=?, pid=?, pgid=?, boot_id=?, error=''
		WHERE id=? AND status IN (?,?)`,
		StatusRunning, nowRFC3339(), pid, pgid, bootID, id, StatusPending, StatusRunning)
	return err
}

// SetTaskProcess records the runner PID without touching started_at or the
// claimed status.
func (s *Store) SetTaskProcess(id string, pid, pgid int, bootID string) error {
	_, err := s.db.Exec(`UPDATE tasks SET pid=?, pgid=?, boot_id=? WHERE id=?`,
		pid, pgid, bootID, id)
	return err
}

// ClaimPendingTask atomically moves a queued task to running. Two schedulers
// can overlap (an old process still shutting down while a new one starts after
// a service restart), and without this claim both would start a runner for the
// same task, giving two agents the same prompt and two writers the same log.
func (s *Store) ClaimPendingTask(id, bootID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status=?, started_at=?, boot_id=?, error=''
		WHERE id=? AND status=?`, StatusRunning, nowRFC3339(), bootID, id, StatusPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetTaskLiveStatus updates running/waiting_input only while the task is still
// live. A stale state line arriving after a cancel or exit is ignored instead of
// flipping a terminal task back to running.
func (s *Store) SetTaskLiveStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=? WHERE id=? AND status IN (?,?)`,
		status, id, StatusRunning, StatusWaitingInput)
	return err
}

// SetTaskStatus records an error message for a task without changing a status
// that is already terminal. It is used for agent-reported errors, which can be
// ingested after the task has finished.
func (s *Store) SetTaskStatus(id, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=?, error=?
		WHERE id=? AND status NOT IN (?,?,?,?)`, status, errMsg, id,
		StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted)
	return err
}

func (s *Store) SetTaskWaiting(id string, waiting bool) error {
	if waiting {
		_, err := s.db.Exec(`UPDATE tasks SET status=? WHERE id=? AND status=?`,
			StatusWaitingInput, id, StatusRunning)
		return err
	}
	_, err := s.db.Exec(`UPDATE tasks SET status=? WHERE id=? AND status=?`,
		StatusRunning, id, StatusWaitingInput)
	return err
}

// FinishTaskIfPending cancels a task only while it is still queued. The
// scheduler may launch it concurrently, so an unconditional write could mark a
// live agent cancelled while its process keeps running.
func (s *Store) FinishTaskIfPending(id, status, errMsg string) (bool, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status=?, error=?, ended_at=? WHERE id=? AND status=?`,
		status, errMsg, nowRFC3339(), id, StatusPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) FinishTask(id, status string, exitCode *int, errMsg, endedAt string) error {
	if endedAt == "" {
		endedAt = nowRFC3339()
	}
	// A terminal task stays terminal. A late exit record from an escalated
	// cancel (or a duplicate line after a restart) used to overwrite the
	// recorded outcome with a different one.
	_, err := s.db.Exec(`UPDATE tasks SET status=?, exit_code=?, error=?, ended_at=?
		WHERE id=? AND status NOT IN (?,?,?,?)`,
		status, exitCode, errMsg, endedAt, id,
		StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted)
	return err
}

func (s *Store) SetTaskOffset(id string, offset int64) error {
	_, err := s.db.Exec(`UPDATE tasks SET ingest_offset=? WHERE id=?`, offset, id)
	return err
}

func (s *Store) SetTaskLastSeq(id string, seq int64) error {
	_, err := s.db.Exec(`UPDATE tasks SET last_seq=? WHERE id=?`, seq, id)
	return err
}

func (s *Store) SetTaskLogSize(id string, size int64) error {
	_, err := s.db.Exec(`UPDATE tasks SET log_size=? WHERE id=?`, size, id)
	return err
}

func (s *Store) SetTaskNativeID(id, nativeID string) error {
	_, err := s.db.Exec(`UPDATE tasks SET native_id=? WHERE id=?`, nativeID, id)
	return err
}

func (s *Store) SetTaskMeta(id, meta string) error {
	_, err := s.db.Exec(`UPDATE tasks SET meta=? WHERE id=?`, defaultJSON(meta), id)
	return err
}

func (s *Store) RequestCancel(id string) error {
	_, err := s.db.Exec(`UPDATE tasks SET cancel_requested=1 WHERE id=?`, id)
	return err
}

func (s *Store) InsertEvent(e *Event) (bool, error) {
	if e.TS == "" {
		e.TS = nowRFC3339()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO events
		(session_id,task_id,task_seq,sub,ts,kind,payload) VALUES (?,?,?,?,?,?,?)`,
		e.SessionID, e.TaskID, e.TaskSeq, e.Sub, e.TS, e.Kind, string(e.Payload))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) EventsAfter(sessionID string, after int64, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
		FROM events WHERE session_id=? AND seq>? ORDER BY seq ASC LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.SessionID, &e.TaskID, &e.TaskSeq, &e.Sub, &e.TS, &e.Kind, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EventsBefore returns up to limit events older than before, oldest first.
// It backs the "load earlier" control so a long session does not have to be
// loaded in one page.
func (s *Store) EventsBefore(sessionID string, before int64, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
		FROM events WHERE session_id=? AND seq<? ORDER BY seq DESC LIMIT ?`, sessionID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.SessionID, &e.TaskID, &e.TaskSeq, &e.Sub, &e.TS, &e.Kind, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) GetEvent(sessionID string, seq int64) (*Event, error) {
	var e Event
	var payload string
	err := s.db.QueryRow(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
		FROM events WHERE session_id=? AND seq=?`, sessionID, seq).
		Scan(&e.Seq, &e.SessionID, &e.TaskID, &e.TaskSeq, &e.Sub, &e.TS, &e.Kind, &payload)
	if err != nil {
		return nil, err
	}
	e.Payload = json.RawMessage(payload)
	return &e, nil
}

func (s *Store) LastEventSeq(sessionID string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM events WHERE session_id=?`, sessionID).Scan(&n)
	return n.Int64, err
}

func (s *Store) CreatePermission(p *Permission) error {
	if p.ID == "" {
		p.ID = randomID(12)
	}
	if p.CreatedAt == "" {
		p.CreatedAt = nowRFC3339()
	}
	if len(p.Input) == 0 {
		p.Input = json.RawMessage(`{}`)
	}
	if p.Kind == "" {
		p.Kind = kindPermission
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO permissions
		(id,task_id,session_id,tool_name,input,status,created_at,kind,detail) VALUES (?,?,?,?,?,?,?,?,?)`,
		p.ID, p.TaskID, p.SessionID, p.ToolName, string(p.Input), p.Status, p.CreatedAt, p.Kind, string(p.Detail))
	return err
}

func (s *Store) GetPermission(id string) (*Permission, error) {
	var p Permission
	var input, detail string
	err := s.db.QueryRow(`SELECT id,task_id,session_id,tool_name,input,status,created_at,resolved_at,response,kind,detail
		FROM permissions WHERE id=?`, id).Scan(&p.ID, &p.TaskID, &p.SessionID, &p.ToolName,
		&input, &p.Status, &p.CreatedAt, &p.ResolvedAt, &p.Response, &p.Kind, &detail)
	if err != nil {
		return nil, err
	}
	p.Input = json.RawMessage(input)
	if detail != "" {
		p.Detail = json.RawMessage(detail)
	}
	return &p, nil
}

func (s *Store) PendingPermissions(sessionID string) ([]*Permission, error) {
	q := `SELECT p.id,p.task_id,p.session_id,p.tool_name,p.input,p.status,p.created_at,p.resolved_at,p.response,
		COALESCE(s.title,''),p.kind,p.detail FROM permissions p LEFT JOIN sessions s ON s.id=p.session_id WHERE p.status='pending'`
	var args []any
	if sessionID != "" {
		q += ` AND p.session_id=?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY p.created_at ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Permission
	for rows.Next() {
		var p Permission
		var input, detail string
		if err := rows.Scan(&p.ID, &p.TaskID, &p.SessionID, &p.ToolName, &input,
			&p.Status, &p.CreatedAt, &p.ResolvedAt, &p.Response, &p.SessionTitle, &p.Kind, &detail); err != nil {
			return nil, err
		}
		p.Input = json.RawMessage(input)
		if detail != "" {
			p.Detail = json.RawMessage(detail)
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) ResolvePermission(id, status, response string) error {
	if status != "allowed" && status != "denied" && status != "expired" && status != "answered" {
		return fmt.Errorf("invalid permission status %q", status)
	}
	_, err := s.db.Exec(`UPDATE permissions SET status=?, resolved_at=?, response=? WHERE id=? AND status='pending'`,
		status, nowRFC3339(), response, id)
	return err
}

func (s *Store) ExpirePermissionsForTask(taskID string) error {
	_, err := s.db.Exec(`UPDATE permissions SET status='expired', resolved_at=? WHERE task_id=? AND status='pending'`,
		nowRFC3339(), taskID)
	return err
}

func (s *Store) PendingPermissionCount(taskID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM permissions WHERE task_id=? AND status='pending'`, taskID).Scan(&n)
	return n, err
}

func (s *Store) SessionStats(sessionID string) (events int64, lastSeq int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(seq),0) FROM events WHERE session_id=?`, sessionID).
		Scan(&events, &lastSeq)
	return
}

func (s *Store) RunsOnBoot(bootID string) ([]*Task, error) {
	rows, err := s.db.Query(taskSelect+` WHERE t.status IN (?,?)`, StatusRunning, StatusWaitingInput)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RequeueRunningAsInterrupted(reason string) (int64, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status=?, error=?, ended_at=? WHERE status IN (?,?)`,
		StatusInterrupted, reason, nowRFC3339(), StatusRunning, StatusWaitingInput)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) TrashSessionEvents(id string) error {
	_, err := s.db.Exec(`DELETE FROM events WHERE session_id=?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defaultJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func truncateTitle(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		max = 72
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
