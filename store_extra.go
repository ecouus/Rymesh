package console

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
)

func (s *Store) GetTaskByRequestID(reqID string) (*Task, error) {
	if reqID == "" {
		return nil, sql.ErrNoRows
	}
	return scanTask(s.db.QueryRow(taskSelect+` WHERE t.request_id=?`, reqID))
}

// RunnablePendingTasks returns queued tasks whose session has no live task.
// Filtering in SQL rather than in Go matters: with a fixed page of the oldest
// pending tasks, 200 blocked tasks on busy sessions would starve every newer
// task on a free one.
func (s *Store) RunnablePendingTasks(limit int) ([]*Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(taskSelect+` WHERE t.status=? AND NOT EXISTS (
		SELECT 1 FROM tasks r WHERE r.session_id=t.session_id AND r.status IN (?,?))
		ORDER BY t.created_at ASC LIMIT ?`, StatusPending, StatusRunning, StatusWaitingInput, limit)
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

func (s *Store) PendingTasks(limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.Query(taskSelect+` WHERE t.status=? ORDER BY t.created_at ASC LIMIT ?`, StatusPending, limit)
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

// RecentEvents returns the newest `limit` events in ascending order.
func (s *Store) RecentEvents(sessionID string, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
		FROM events WHERE session_id=? ORDER BY seq DESC LIMIT ?`, sessionID, limit)
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
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// EventWindow is a page of a transcript selected by conversation turns.
// Events are returned in ascending seq order. HasMore reports whether anything
// older than this page exists.
type EventWindow struct {
	Events  []*Event
	HasMore bool
}

// turnStartSeq returns the seq of the n-th newest user event at or before the
// cursor, or 0 when the session has fewer than n user events in that range.
// Turns are delimited by user events, so a page that starts on one never cuts a
// turn in half.
func (s *Store) turnStartSeq(sessionID string, before int64, turns int) (int64, error) {
	if turns <= 0 {
		return 0, nil
	}
	var row *sql.Row
	if before > 0 {
		row = s.db.QueryRow(`SELECT seq FROM events WHERE session_id=? AND kind='user' AND seq<?
			ORDER BY seq DESC LIMIT 1 OFFSET ?`, sessionID, before, turns-1)
	} else {
		row = s.db.QueryRow(`SELECT seq FROM events WHERE session_id=? AND kind='user'
			ORDER BY seq DESC LIMIT 1 OFFSET ?`, sessionID, turns-1)
	}
	var seq int64
	if err := row.Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return seq, nil
}

// RecentTurns returns the newest `turns` conversation turns. Pages are bounded
// by turns so a page boundary never splits a turn in half, and the first page
// always starts on a user message.
//
// `cap` is a soft guard against a pathological transcript, not a page size: it
// widens the fetch window so the turn boundary is still reachable. Measured
// sessions top out around 88 events in a single turn, so the guard never fires
// in practice; when it does (a single turn longer than cap), the whole turn is
// still returned rather than cutting it in half, because a half turn is the
// exact thing turn-based paging exists to prevent.
func (s *Store) RecentTurns(sessionID string, turns, cap int) (*EventWindow, error) {
	if turns <= 0 {
		turns = 10
	}
	if cap <= 0 || cap > 2000 {
		cap = 600
	}
	start, err := s.turnStartSeq(sessionID, 0, turns)
	if err != nil {
		return nil, err
	}
	var evs []*Event
	if start > 0 {
		// Everything from the chosen turn start up to the newest event.
		evs, err = s.EventsRange(sessionID, start, 0)
	} else {
		hasUsers, hasErr := s.hasUserEvents(sessionID)
		if hasErr != nil {
			return nil, hasErr
		}
		if hasUsers {
			// The session has fewer turns than asked for, so this page is the
			// whole conversation. It still reaches back to the session's first
			// event rather than the last `cap` events: a cap-based page can
			// start in the middle of the opening turn, which is exactly what
			// turn-based paging is meant to avoid.
			evs, err = s.EventsRange(sessionID, 0, 0)
		} else {
			// No user messages at all: there is no turn boundary to respect.
			evs, err = s.RecentEvents(sessionID, cap)
		}
	}
	if err != nil {
		return nil, err
	}
	oldest := int64(0)
	if len(evs) > 0 {
		oldest = evs[0].Seq
	}
	hasMore := false
	if oldest > 0 {
		hasMore, err = s.hasEventsBefore(sessionID, oldest)
		if err != nil {
			return nil, err
		}
	}
	return &EventWindow{Events: evs, HasMore: hasMore}, nil
}

// EventsBeforeTurns returns the page of up to `turns` turns immediately older
// than `before`, always starting on a user event. `cap` behaves as in
// RecentTurns: a soft guard that never splits a turn.
func (s *Store) EventsBeforeTurns(sessionID string, before int64, turns, cap int) (*EventWindow, error) {
	if turns <= 0 {
		turns = 10
	}
	if cap <= 0 || cap > 2000 {
		cap = 600
	}
	start, err := s.turnStartSeq(sessionID, before, turns)
	if err != nil {
		return nil, err
	}
	var evs []*Event
	if start > 0 {
		// Everything from the chosen turn start up to (excluding) the cursor.
		evs, err = s.EventsRange(sessionID, start, before)
	} else {
		hasUsers, hasErr := s.hasUserEvents(sessionID)
		if hasErr != nil {
			return nil, hasErr
		}
		if hasUsers {
			// Fewer turns remain than asked for: this page is everything older.
			// Starting at the session's first event, not at the last `cap`
			// events, is what keeps the opening turn whole.
			evs, err = s.EventsRange(sessionID, 0, before)
		} else {
			// No user messages at all: there is no turn boundary to respect.
			evs, err = s.EventsBefore(sessionID, before, cap)
		}
	}
	if err != nil {
		return nil, err
	}
	oldest := int64(0)
	if len(evs) > 0 {
		oldest = evs[0].Seq
	}
	hasMore := false
	if oldest > 0 {
		hasMore, err = s.hasEventsBefore(sessionID, oldest)
		if err != nil {
			return nil, err
		}
	}
	return &EventWindow{Events: evs, HasMore: hasMore}, nil
}

// EventsRange returns events with start <= seq < end in ascending order.
func (s *Store) EventsRange(sessionID string, start, end int64) ([]*Event, error) {
	if end <= 0 {
		rows, err := s.db.Query(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
			FROM events WHERE session_id=? AND seq>=? ORDER BY seq ASC`, sessionID, start)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanEvents(rows)
	}
	rows, err := s.db.Query(`SELECT seq,session_id,task_id,task_seq,sub,ts,kind,payload
		FROM events WHERE session_id=? AND seq>=? AND seq<? ORDER BY seq ASC`,
		sessionID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]*Event, error) {
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

func (s *Store) hasEventsBefore(sessionID string, seq int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=? AND seq<?`,
		sessionID, seq).Scan(&n)
	return n > 0, err
}

// hasUserEvents reports whether the session contains at least one user message,
// i.e. whether turn boundaries exist at all.
func (s *Store) hasUserEvents(sessionID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM events WHERE session_id=? AND kind='user' LIMIT 1`,
		sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) UpdateImportedSession(id, title, cwd, updated string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title=?, cwd=?, updated_at=?, last_message_at=? WHERE id=?`,
		title, cwd, updated, updated, id)
	return err
}

func (s *Store) SourceCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT source, COUNT(*) FROM sessions GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s *Store) TaskCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) GetMeta(k string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

// QueuePosition reports this task's 1-based place in its session's queue, or 0
// when nothing has to finish first.
//
// Only work on the same session blocks a task: different sessions and
// different agents run in parallel. The task itself is never counted — a lone
// pending task used to be reported as "前面还有 1 个任务" although the task
// list contained only itself.
func (s *Store) QueuePosition(taskID string) (int, error) {
	pos, _, err := s.QueueWait(taskID)
	return pos, err
}

// QueueWait returns the 1-based position and the number of tasks still being
// waited for. They differ by one: first in line has position 1 and waits for
// nothing.
func (s *Store) QueueWait(taskID string) (pos, ahead int, err error) {
	var created, status, sessionID string
	if err := s.db.QueryRow(`SELECT created_at, status, session_id FROM tasks WHERE id=?`, taskID).
		Scan(&created, &status, &sessionID); err != nil {
		return 0, 0, err
	}
	ahead, err = s.sessionQueueAhead(sessionID, taskID, created, status)
	if err != nil || status != StatusPending {
		return 0, 0, err
	}
	return ahead + 1, ahead, nil
}

// sessionQueueAhead counts the work a task has to wait for: an agent process
// already running on the session, plus older pending tasks of the same session.
func (s *Store) sessionQueueAhead(sessionID, taskID, created, status string) (int, error) {
	if status != StatusPending {
		return 0, nil
	}
	var ahead int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks
		WHERE session_id=? AND status IN (?,?)`,
		sessionID, StatusRunning, StatusWaitingInput).Scan(&ahead); err != nil {
		return 0, err
	}
	var older int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks
		WHERE session_id=? AND status=? AND id<>? AND created_at<?`,
		sessionID, StatusPending, taskID, created).Scan(&older); err != nil {
		return 0, err
	}
	return ahead + older, nil
}

func (s *Store) ActiveTasksOnCwd(cwd string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE cwd=? AND status IN (?,?)`,
		cwd, StatusRunning, StatusWaitingInput).Scan(&n)
	return n, err
}

var errNoRows = sql.ErrNoRows

func isNoRowsErr(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ---------------------------------------------------------------------------
// Native snapshot tracking
//
// Imported sessions used to be a one-time snapshot taken at service start, so a
// conversation continued in the CLI afterwards never appeared in the console.
// Each imported session now records how far its native file has been read, and
// the importer appends only the new tail when the page is opened.

// ImportState is persisted in sessions.meta for imported sessions.
type ImportState struct {
	Path    string `json:"import_path,omitempty"`
	Offset  int64  `json:"import_offset,omitempty"`
	Size    int64  `json:"import_size,omitempty"`
	Seq     int64  `json:"import_seq,omitempty"`
	Updated string `json:"import_updated,omitempty"`
	// Ino and MTime identify the exact file the offset belongs to. Without them
	// a transcript replaced in place (same path, same or larger size) would be
	// read from the old offset, splicing two different conversations together.
	Ino   uint64 `json:"import_ino,omitempty"`
	MTime int64  `json:"import_mtime,omitempty"`
}

// fileStamp returns the identity fields of a transcript.
func fileStamp(fi os.FileInfo) (ino uint64, mtime int64) {
	mtime = fi.ModTime().UnixNano()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return ino, mtime
}

func parseImportState(meta string) ImportState {
	var st ImportState
	if strings.TrimSpace(meta) == "" {
		return st
	}
	_ = json.Unmarshal([]byte(meta), &st)
	return st
}

// SetImportState stores the native-file cursor. Only the importer writes it, so
// a whole-document write is safe here.
func (s *Store) SetImportState(sessionID string, st ImportState) error {
	// Preserve any future meta keys by merging into the existing document.
	existing := s.GetSessionMeta(sessionID)
	m := map[string]any{}
	if strings.TrimSpace(existing) != "" {
		_ = json.Unmarshal([]byte(existing), &m)
	}
	m["import_path"] = st.Path
	m["import_offset"] = st.Offset
	m["import_size"] = st.Size
	m["import_seq"] = st.Seq
	m["import_ino"] = st.Ino
	m["import_mtime"] = st.MTime
	if st.Updated != "" {
		m["import_updated"] = st.Updated
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE sessions SET meta=? WHERE id=?`, string(b), sessionID)
	return err
}

func (s *Store) GetSessionMeta(sessionID string) string {
	var m string
	if err := s.db.QueryRow(`SELECT meta FROM sessions WHERE id=?`, sessionID).Scan(&m); err != nil {
		return ""
	}
	return m
}

// HasNonImportEvents reports whether the session contains events produced by a
// console task. Those turns are already captured from the task log; importing
// the native transcript again would duplicate them.
func (s *Store) HasNonImportEvents(sessionID, importTaskID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=? AND task_id<>?`,
		sessionID, importTaskID).Scan(&n)
	return n > 0, err
}

// LastNonImportEventTS returns the timestamp of the newest console-produced
// event, used to skip native lines that belong to a console-run turn.
func (s *Store) LastNonImportEventTS(sessionID, importTaskID string) (string, error) {
	var ts sql.NullString
	err := s.db.QueryRow(`SELECT MAX(ts) FROM events WHERE session_id=? AND task_id<>?`,
		sessionID, importTaskID).Scan(&ts)
	return ts.String, err
}

// AppendImportedEvents adds events to an imported session and advances its
// cursor in one transaction. seq is the last task_seq already used.
func (s *Store) AppendImportedEvents(sessionID, taskID string, seq int64, evs []impEvent, updated, title string, st ImportState) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return seq, err
	}
	defer tx.Rollback()
	for _, e := range evs {
		p, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		seq++
		if _, err := tx.Exec(`INSERT OR IGNORE INTO events
			(session_id,task_id,task_seq,sub,ts,kind,payload) VALUES (?,?,?,?,?,?,?)`,
			sessionID, taskID, seq, 0, e.TS, e.Kind, string(p)); err != nil {
			return seq, err
		}
	}
	sets := []string{"updated_at=?", "last_message_at=?"}
	args := []any{updated, updated}
	if strings.TrimSpace(title) != "" {
		sets = append(sets, "title=?")
		args = append(args, title)
	}
	args = append(args, sessionID)
	if _, err := tx.Exec(`UPDATE sessions SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return seq, err
	}
	var cur string
	_ = tx.QueryRow(`SELECT meta FROM sessions WHERE id=?`, sessionID).Scan(&cur)
	m := map[string]any{}
	if strings.TrimSpace(cur) != "" {
		_ = json.Unmarshal([]byte(cur), &m)
	}
	m["import_path"] = st.Path
	m["import_offset"] = st.Offset
	m["import_size"] = st.Size
	m["import_seq"] = st.Seq
	m["import_ino"] = st.Ino
	m["import_mtime"] = st.MTime
	if st.Updated != "" {
		m["import_updated"] = st.Updated
	}
	b, err := json.Marshal(m)
	if err != nil {
		return seq, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET meta=? WHERE id=?`, string(b), sessionID); err != nil {
		return seq, err
	}
	return seq, tx.Commit()
}

// ReplaceImportedEvents atomically replaces the snapshot rows of an imported
// session and stores the new cursor. Console-produced events are untouched:
// they live under their own task ids.
func (s *Store) ReplaceImportedEvents(sessionID, taskID string, evs []impEvent, updated, title string, st ImportState) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM events WHERE session_id=? AND task_id=?`, sessionID, taskID); err != nil {
		return 0, err
	}
	seq := int64(0)
	for _, e := range evs {
		p, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		seq++
		if _, err := tx.Exec(`INSERT OR IGNORE INTO events
			(session_id,task_id,task_seq,sub,ts,kind,payload) VALUES (?,?,?,?,?,?,?)`,
			sessionID, taskID, seq, 0, e.TS, e.Kind, string(p)); err != nil {
			return seq, err
		}
	}
	sets := []string{"updated_at=?", "last_message_at=?"}
	args := []any{updated, updated}
	if strings.TrimSpace(title) != "" {
		sets = append(sets, "title=?")
		args = append(args, title)
	}
	args = append(args, sessionID)
	if _, err := tx.Exec(`UPDATE sessions SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return seq, err
	}
	var cur string
	_ = tx.QueryRow(`SELECT meta FROM sessions WHERE id=?`, sessionID).Scan(&cur)
	m := map[string]any{}
	if strings.TrimSpace(cur) != "" {
		_ = json.Unmarshal([]byte(cur), &m)
	}
	m["import_path"] = st.Path
	m["import_offset"] = st.Offset
	m["import_size"] = st.Size
	m["import_seq"] = st.Seq
	m["import_ino"] = st.Ino
	m["import_mtime"] = st.MTime
	if st.Updated != "" {
		m["import_updated"] = st.Updated
	}
	b, err := json.Marshal(m)
	if err != nil {
		return seq, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET meta=? WHERE id=?`, string(b), sessionID); err != nil {
		return seq, err
	}
	return seq, tx.Commit()
}

// LastImportedSeq returns the highest task_seq already written for an import
// task, so an incremental refresh continues numbering from there.
func (s *Store) LastImportedSeq(sessionID, taskID string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(task_seq) FROM events WHERE session_id=? AND task_id=?`,
		sessionID, taskID).Scan(&n)
	return n.Int64, err
}

// TrashImportedEvents removes only the snapshot rows of an imported session.
// Console-produced task events must survive a re-import: the detached runner
// owns them and they cannot be reconstructed from the native transcript
// without duplication or reordering.
func (s *Store) TrashImportedEvents(sessionID, taskID string) error {
	_, err := s.db.Exec(`DELETE FROM events WHERE session_id=? AND task_id=?`, sessionID, taskID)
	return err
}

// CountImportedEvents is the number of snapshot rows already stored.
func (s *Store) CountImportedEvents(sessionID, taskID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=? AND task_id=?`,
		sessionID, taskID).Scan(&n)
	return n, err
}

// ConsoleTaskWindows returns [started,ended] ranges of console-run tasks on a
// session. Native transcript lines inside a window were already ingested from
// the task log, so an incremental import must not append them again. A task
// that is still running has no end; its window is returned open-ended (End=="")
// because its native records are being ingested live.
type TaskWindow struct{ Start, End string }

func (s *Store) ConsoleTaskWindows(sessionID string) ([]TaskWindow, error) {
	rows, err := s.db.Query(`SELECT started_at, ended_at FROM tasks
		WHERE session_id=? AND started_at<>''`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskWindow
	for rows.Next() {
		var w TaskWindow
		if err := rows.Scan(&w.Start, &w.End); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// validID reports whether s is a bare identifier of the kind randomID and
// newUUID produce. It is used wherever an identifier becomes a path component,
// so that "..", slashes and Windows separators can never escape the data dir.
func validID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
