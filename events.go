package console

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type normEvent struct {
	Kind    string
	Payload any
}

type Ingestor struct {
	store   *Store
	dataDir string
	log     *log.Logger
	mu      sync.Mutex
	tailing map[string]bool
	onDone  func()
}

func newIngestor(st *Store, dataDir string, logger *log.Logger) *Ingestor {
	return &Ingestor{store: st, dataDir: dataDir, log: logger, tailing: map[string]bool{}}
}

func (ing *Ingestor) taskDir(taskID string) string {
	return filepath.Join(ing.dataDir, "tasks", taskID)
}

func (ing *Ingestor) Start(taskID string) {
	ing.mu.Lock()
	if ing.tailing[taskID] {
		ing.mu.Unlock()
		return
	}
	ing.tailing[taskID] = true
	ing.mu.Unlock()
	go func() {
		defer func() {
			ing.mu.Lock()
			delete(ing.tailing, taskID)
			ing.mu.Unlock()
		}()
		ing.tail(taskID)
	}()
}

func (ing *Ingestor) tail(taskID string) {
	var offset int64
	t, err := ing.store.GetTask(taskID)
	if err == nil {
		offset = t.IngestOffset
	}
	idleExit := 0
	for {
		advanced, sawExit := ing.readOnce(taskID, &offset)
		if sawExit {
			// Drain a little more in case the writer flushed the exit before the tail.
			time.Sleep(100 * time.Millisecond)
			ing.readOnce(taskID, &offset)
			if ing.onDone != nil {
				ing.onDone()
			}
			return
		}
		if advanced {
			idleExit = 0
		} else {
			idleExit++
		}
		t, err := ing.store.GetTask(taskID)
		if err == nil {
			switch t.Status {
			case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
				if ing.onDone != nil {
					ing.onDone()
				}
				return
			}
			// PID 0 is "not recorded yet", not "dead": treating it as dead used
			// to interrupt a task whose runner had just started, while the agent
			// itself kept running. The exit record is the only authority until a
			// PID exists.
			if idleExit >= 4 && t.PID > 0 && (t.Status == StatusRunning || t.Status == StatusWaitingInput) {
				if !processAlive(t.PID) {
					// Give the writer a final chance.
					time.Sleep(500 * time.Millisecond)
					if !processAlive(t.PID) {
						ing.markInterrupted(taskID, "runner 进程已退出且没有退出记录")
						if ing.onDone != nil {
							ing.onDone()
						}
						return
					}
				}
			}
		}
		// Read again soon while output is flowing; the agent tends to write
		// in bursts.
		if advanced {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// Reconcile reads everything already on disk after a service restart and
// classifies tasks whose runner is gone. It never restarts an agent.
func (ing *Ingestor) Reconcile() {
	tasks, err := ing.store.ActiveTasks()
	if err != nil {
		ing.log.Printf("reconcile: %v", err)
		return
	}
	boot := currentBootID()
	for _, t := range tasks {
		if t.Status == StatusPending {
			continue
		}
		var offset int64 = t.IngestOffset
		sawExit := false
		for {
			advanced, exited := ing.readOnce(t.ID, &offset)
			if exited {
				sawExit = true
			}
			if !advanced {
				break
			}
		}
		cur, err := ing.store.GetTask(t.ID)
		if err != nil {
			continue
		}
		if sawExit {
			continue
		}
		switch cur.Status {
		case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
			continue
		}
		if t.BootID != "" && boot != "" && t.BootID != boot {
			ing.markInterrupted(t.ID, "服务器已重启，原 Agent 进程已终止")
			continue
		}
		// PID 0 means launch() marked the task running but has not recorded the
		// runner yet. There is no evidence the runner is gone, so re-attach and
		// let the normal tail loop decide via processAlive and the exit record.
		if t.PID > 0 && !processAlive(t.PID) {
			ing.markInterrupted(t.ID, "Web 服务重启后未找到存活的 runner 进程")
			continue
		}
		ing.Start(t.ID)
	}
}

func (ing *Ingestor) markInterrupted(taskID, reason string) {
	ing.stopOrphanChild(taskID)
	_ = ing.store.FinishTask(taskID, StatusInterrupted, nil, reason, nowRFC3339())
	_ = ing.store.ExpirePermissionsForTask(taskID)
	// Record the transition in the session stream as well, so the console shows
	// why the task stopped instead of just going quiet. The fixed task_seq
	// keeps this idempotent if reconciliation runs twice.
	if t, err := ing.store.GetTask(taskID); err == nil && t.SessionID != "" {
		sp, _ := json.Marshal(map[string]any{
			"status": StatusInterrupted, "reason": "lost", "message": reason,
		})
		_, _ = ing.store.InsertEvent(&Event{
			SessionID: t.SessionID, TaskID: taskID, TaskSeq: 1 << 43, Sub: 0,
			TS: nowRFC3339(), Kind: "status", Payload: sp,
		})
		np, _ := json.Marshal(map[string]any{"text": reason})
		_, _ = ing.store.InsertEvent(&Event{
			SessionID: t.SessionID, TaskID: taskID, TaskSeq: 1 << 43, Sub: 1,
			TS: nowRFC3339(), Kind: "notice", Payload: np,
		})
	}
	ing.log.Printf("task %s -> interrupted: %s", taskID, reason)
}

// stopOrphanChild terminates the agent process of a task whose runner is gone.
// Without this the agent could keep running unmanaged (and still perform side
// effects) after its task has been reported as interrupted. Identity checks
// guard against killing an unrelated process that reused the recorded PID.
func (ing *Ingestor) stopOrphanChild(taskID string) {
	b, err := os.ReadFile(filepath.Join(ing.taskDir(taskID), "child.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || pid == os.Getpid() || !processAlive(pid) {
		return
	}
	// The agent was started as its own process-group leader.
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return
	}
	// Verify the process still looks like the agent before sending signals.
	t, err := ing.store.GetTask(taskID)
	if err != nil {
		return
	}
	cmdline, err := processCmdline(pid)
	if err != nil {
		return
	}
	cl := strings.ToLower(cmdline)
	names := []string{strings.ToLower(t.Agent)}
	if b := agentBinary(t.Agent); b != "" {
		names = append(names, strings.ToLower(filepath.Base(b)))
	}
	agent := false
	for _, n := range names {
		agent = agent || (n != "" && strings.Contains(cl, n))
	}
	if !agent {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	ing.log.Printf("task %s: terminated orphan agent pid %d", taskID, pid)
}

func agentBinary(name string) string {
	if agents == nil { // set up by the CLI entry point; absent when embedded
		return ""
	}
	if a, ok := agents.Get(name); ok {
		return a.Binary
	}
	return ""
}

// readOnce consumes complete lines starting at *offset and returns whether it
// advanced and whether an exit record was seen. It emits normalized events into
// SQLite exactly once thanks to the (task_id, task_seq, sub) unique key.
func (ing *Ingestor) readOnce(taskID string, offset *int64) (advanced, sawExit bool) {
	defer ing.scanControl(taskID)
	path := filepath.Join(ing.taskDir(taskID), "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= *offset {
		if err == nil && st.Size() < *offset {
			*offset = 0
		}
		return false, false
	}
	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return false, false
	}
	// The batch budget only decides when to stop starting new lines. A single
	// line may be larger than the budget (the runner allows 8 MiB of agent
	// output per line, and JSON re-encoding can grow it); capping the read
	// itself used to leave such a line unconsumed forever and wedge the task.
	br := bufio.NewReaderSize(io.LimitReader(f, st.Size()-*offset), 64*1024)
	var consumed int64
	for consumed < ingestBatchBytes {
		line, n, ok := readEventLine(br, ingestMaxLineBytes)
		if !ok {
			// No complete line yet: the writer is mid-line.
			break
		}
		lineOffset := *offset + consumed
		var err error
		if line == nil {
			err = ing.skipOversizedLine(taskID, lineOffset, n)
		} else {
			err = ing.processLine(taskID, strings.TrimRight(string(line), "\r\n"))
		}
		if err != nil {
			// Stop before this line: the offset must not move past an event
			// that is not in the database, or it would never be shown. The
			// next pass retries it; rows already written are ignored then.
			ing.log.Printf("task %s: ingest at offset %d: %v (will retry)", taskID, lineOffset, err)
			break
		}
		consumed += n
		if line != nil && ing.isExitLine(line) {
			sawExit = true
		}
	}
	advanced = consumed > 0
	if consumed > 0 {
		*offset += consumed
		_ = ing.store.SetTaskOffset(taskID, *offset)
		_ = ing.store.SetTaskLogSize(taskID, st.Size())
	}
	return advanced, sawExit
}

const (
	// ingestBatchBytes bounds how much one readOnce pass consumes.
	ingestBatchBytes = 2 << 20
	// ingestMaxLineBytes is the largest event line that is parsed; longer
	// lines are skipped with a notice instead of being held in memory.
	ingestMaxLineBytes = 32 << 20
)

// readEventLine reads the next complete '\n'-terminated line. n is the number
// of bytes it spans. A line longer than max is drained and returned as nil.
// ok is false when the input ends before a newline (a partial write), in which
// case nothing should be treated as consumed.
func readEventLine(br *bufio.Reader, max int) (line []byte, n int64, ok bool) {
	over := false
	for {
		part, err := br.ReadSlice('\n')
		n += int64(len(part))
		if !over {
			if len(line)+len(part) > max {
				over, line = true, nil
			} else {
				line = append(line, part...)
			}
		}
		switch err {
		case nil:
			if over {
				return nil, n, true
			}
			return line, n, true
		case bufio.ErrBufferFull:
			continue
		default:
			return nil, n, false
		}
	}
}

// skipOversizedLine records that an event line was too large to ingest. The
// task_seq is derived from the line's file offset so a re-read stays idempotent.
func (ing *Ingestor) skipOversizedLine(taskID string, offset, size int64) error {
	ing.log.Printf("task %s: skipped oversized event line at offset %d (%d bytes)", taskID, offset, size)
	t, err := ing.task(taskID)
	if t == nil {
		return err
	}
	return ing.insert(t, 1<<40+offset, 0, nowRFC3339(), "notice", map[string]any{
		"text": fmt.Sprintf("一条事件过大（%d 字节），已跳过显示；Agent 输出不受影响。", size),
	})
}

// task loads a task for ingesting its events. A task that no longer exists
// (its session was deleted) yields nil and no error: its lines are skipped.
// Any other failure is returned so the line is retried.
func (ing *Ingestor) task(id string) (*Task, error) {
	t, err := ing.store.GetTask(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (ing *Ingestor) isExitLine(line []byte) bool {
	var env struct {
		Stream string `json:"stream"`
	}
	if json.Unmarshal(line, &env) == nil && env.Stream == "exit" {
		return true
	}
	return false
}

// processLine stores one event line. An error means it was not (completely)
// stored and must be read again.
func (ing *Ingestor) processLine(taskID, line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var env struct {
		Seq     int64           `json:"seq"`
		TS      string          `json:"ts"`
		Stream  string          `json:"stream"`
		Payload json.RawMessage `json:"payload"`
		State   map[string]any  `json:"state"`
		Exit    map[string]any  `json:"exit"`
		Line    string          `json:"line"`
		Text    string          `json:"text"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		ing.log.Printf("task %s: bad event line: %v", taskID, err)
		return nil
	}
	t, err := ing.task(taskID)
	if t == nil {
		return err
	}
	if env.TS == "" {
		env.TS = nowRFC3339()
	}
	switch env.Stream {
	case "event":
		var raw any
		if err := json.Unmarshal(env.Payload, &raw); err != nil {
			return ing.insert(t, env.Seq, 0, env.TS, "raw", map[string]any{"text": string(env.Payload)})
		}
		ing.applyNativeState(t, raw)
		norms := normalizeEvent(t.Agent, raw, env.TS)
		for i, n := range norms {
			if err := ing.insert(t, env.Seq, i, env.TS, n.Kind, n.Payload); err != nil {
				return err
			}
		}
	case "state":
		ing.applyState(t, env.State)
	case "exit":
		ing.applyExit(t, env.Exit)
	case "stderr":
		if strings.TrimSpace(env.Line) != "" {
			err = ing.insert(t, env.Seq, 0, env.TS, "stderr", map[string]any{"text": env.Line})
		}
	case "notice":
		err = ing.insert(t, env.Seq, 0, env.TS, "notice", map[string]any{"text": env.Text})
	case "text":
		err = ing.insert(t, env.Seq, 0, env.TS, "text", map[string]any{"text": env.Text})
	}
	if err != nil {
		return err
	}
	if env.Seq > 0 {
		_ = ing.store.SetTaskLastSeq(taskID, env.Seq)
	}
	return nil
}

// insert stores one event; storing it again later is a no-op (unique key).
func (ing *Ingestor) insert(t *Task, taskSeq int64, sub int, ts, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil // nothing that a retry could store
	}
	_, err = ing.store.InsertEvent(&Event{
		SessionID: t.SessionID, TaskID: t.ID, TaskSeq: taskSeq, Sub: sub,
		TS: ts, Kind: kind, Payload: b,
	})
	return err
}

func (ing *Ingestor) applyState(t *Task, st map[string]any) {
	if st == nil {
		return
	}
	if id, _ := st["native_id"].(string); id != "" {
		_ = ing.store.SetTaskNativeID(t.ID, id)
		sess, err := ing.store.GetSession(t.SessionID)
		if err == nil && sess.NativeID == "" {
			_ = ing.store.SetSessionNativeID(t.SessionID, id)
			sess.NativeID = id
		}
	}
	if status, _ := st["status"].(string); status != "" {
		switch status {
		case StatusRunning, StatusWaitingInput:
			// A state line buffered before a cancel can be ingested after the
			// task was cancelled or finished; it must not resurrect a terminal
			// task. Only a task that is still live may change its live status.
			_ = ing.store.SetTaskLiveStatus(t.ID, status)
		}
	}
	if ss, ok := st["session_settings"].(map[string]any); ok {
		ing.applySessionSettings(t, ss)
	}
	if rp, ok := number(st["runner_pid"]); ok && rp > 1 {
		if t.PID == 0 {
			_ = ing.store.MarkTaskRunning(t.ID, int(rp), int(rp), str(st["boot_id"]))
		}
	}
}

// applySessionSettings saves a model or effort change the agent confirmed and
// says so in the transcript (the CLI's own reply says "this session only").
func (ing *Ingestor) applySessionSettings(t *Task, ss map[string]any) {
	var notes []string
	_, _ = ing.store.UpdateSessionSettings(t.SessionID, func(st *SessionSettings) {
		if v, ok := ss["model"].(string); ok {
			st.Model = v
			notes = append(notes, "模型 "+orDefault(v, "默认"))
		}
		if v, ok := ss["effort"].(string); ok {
			st.Effort = v
			notes = append(notes, "推理强度 "+orDefault(v, "默认"))
		}
	})
	if len(notes) > 0 {
		ing.insert(t, 1<<39, 0, nowRFC3339(), "notice", map[string]any{
			"text": "已为这个对话保存：" + strings.Join(notes, "，") + "。之后的消息都会使用这个设置。"})
	}
}

func (ing *Ingestor) applyNativeState(t *Task, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	// The CLI lists its slash commands (built-ins, skills, custom commands)
	// when it starts; the composer offers them after "/".
	if str(m["type"]) == "system" && str(m["subtype"]) == "init" && t.Agent == "claude" {
		if cmds, ok := m["slash_commands"].([]any); ok && len(cmds) > 0 {
			if b, err := json.Marshal(cmds); err == nil {
				_ = ing.store.SetMeta(claudeCommandsMeta, string(b))
			}
		}
	}
	if id := extractNativeID(raw); id != "" {
		_ = ing.store.SetTaskNativeID(t.ID, id)
		sess, err := ing.store.GetSession(t.SessionID)
		if err == nil && sess.NativeID == "" {
			_ = ing.store.SetSessionNativeID(t.SessionID, id)
			if sess.Title == "" || sess.Title == t.Prompt {
				title := truncateTitle(t.Prompt, 72)
				_ = ing.store.UpdateSession(t.SessionID, &title, nil)
			}
		}
	}
	// A Claude result that reports is_error should fail the task even though the
	// process exits 0.
	if typ, _ := m["type"].(string); typ == "result" {
		if isErr, _ := m["is_error"].(bool); isErr {
			msg, _ := m["result"].(string)
			if msg == "" {
				msg = "Agent 返回错误"
			}
			_ = ing.store.SetTaskStatus(t.ID, t.Status, truncateTitle(msg, 500))
		}
	}
	if typ, _ := m["type"].(string); typ == "error" {
		msg := "Agent 错误"
		if s, _ := m["message"].(string); s != "" {
			msg = s
		}
		_ = ing.store.SetTaskStatus(t.ID, t.Status, truncateTitle(msg, 500))
	}
}

func (ing *Ingestor) applyExit(t *Task, ex map[string]any) {
	if ex == nil {
		return
	}
	status, _ := ex["status"].(string)
	reason, _ := ex["reason"].(string)
	code := 0
	if v, ok := number(ex["code"]); ok {
		code = int(v)
	}
	if status == "" {
		switch {
		case reason == "cancelled":
			status = StatusCancelled
		case code == 0:
			status = StatusCompleted
		default:
			status = StatusFailed
		}
	}
	cur, err := ing.store.GetTask(t.ID)
	if err == nil && cur.Error != "" && status == StatusCompleted {
		status = StatusFailed
	}
	var cp *int
	if _, ok := ex["code"]; ok {
		cp = &code
	}
	_ = ing.store.FinishTask(t.ID, status, cp, str(ex["error"]), str(ex["ended_at"]))
	_ = ing.store.ExpirePermissionsForTask(t.ID)
	_ = ing.store.TouchSession(t.SessionID)
	if status == StatusCompleted {
		// Keep a compact result in the session list title only if the user has
		// not named it.
		ing.insert(t, 1<<40, 0, nowRFC3339(), "status", map[string]any{"status": status, "reason": reason})
	} else {
		ing.insert(t, 1<<40, 0, nowRFC3339(), "status", map[string]any{"status": status, "reason": reason})
	}
}

// scanControl imports MCP permission requests/responses written by the detached
// processes. These files are the durable link between an agent that is waiting
// and a web server that may have restarted.
// permSub derives a stable sub index from a permission id. Only the
// uniqueness of (task_id, task_seq, sub) matters, so a hash is enough.
func permSub(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() & 0x7fffffff)
}

func (ing *Ingestor) scanControl(taskID string) {
	dir := filepath.Join(ing.taskDir(taskID), "control")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	t, err := ing.store.GetTask(taskID)
	if err != nil {
		return
	}
	pendingBefore, _ := ing.store.PendingPermissionCount(taskID)
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, "req-") && strings.HasSuffix(name, ".json") {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			var req ctlRequest
			if json.Unmarshal(b, &req) != nil || req.ID == "" {
				continue
			}
			if _, err := ing.store.GetPermission(req.ID); err == nil {
				continue
			}
			p := &Permission{
				ID: req.ID, TaskID: taskID, SessionID: t.SessionID,
				ToolName: req.ToolName, Input: req.Input, Status: "pending",
				CreatedAt: req.CreatedAt, Kind: req.kind(), Detail: req.detail(),
			}
			if p.CreatedAt == "" {
				p.CreatedAt = nowRFC3339()
			}
			_ = ing.store.CreatePermission(p)
			// The sub index is the permission id, not a clock value: two
			// requests ingested in the same microsecond used to collide on
			// (task_id, task_seq, sub) and one of them was silently dropped.
			ing.insert(t, 1<<41, permSub(req.ID), nowRFC3339(),
				"permission", map[string]any{
					"permission_id": req.ID, "tool_name": req.ToolName,
					"input": req.Input, "tool_use_id": req.ToolUseID,
					"kind": req.kind(), "description": req.Description,
					"options": req.Options, "questions": req.Questions, "plan": req.Plan,
				})
			_ = ing.store.SetTaskWaiting(taskID, true)
		}
		if strings.HasPrefix(name, "done-") && strings.HasSuffix(name, ".json") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "done-"), ".json")
			p, err := ing.store.GetPermission(id)
			if err != nil || p.Status != "pending" {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, name))
			var d struct {
				Decision string `json:"decision"`
				Message  string `json:"message"`
			}
			_ = json.Unmarshal(b, &d)
			status := "denied"
			switch d.Decision {
			case "allowed", "allow":
				status = "allowed"
			case "answered", "expired":
				status = d.Decision
			}
			_, _ = ing.store.ResolvePermission(id, status, d.Decision+" "+d.Message)
			ing.insert(t, 1<<42, permSub(id), nowRFC3339(),
				"permission_result", map[string]any{"permission_id": id, "decision": status})
		}
	}
	pendingAfter, _ := ing.store.PendingPermissionCount(taskID)
	if pendingAfter == 0 && pendingBefore > 0 {
		_ = ing.store.SetTaskWaiting(taskID, false)
	}
}

// ---------------------------------------------------------------------------
// Event normalization
// ---------------------------------------------------------------------------

func normalizeEvent(agent string, raw any, ts string) []normEvent {
	switch agent {
	case "codex":
		return normalizeCodex(raw)
	case "claude":
		return normalizeClaude(raw)
	}
	return []normEvent{{Kind: "raw", Payload: raw}}
}

func normalizeCodex(raw any) []normEvent {
	m, ok := raw.(map[string]any)
	if !ok {
		return []normEvent{{Kind: "raw", Payload: raw}}
	}
	// Tasks run through app-server log JSON-RPC notifications; older task
	// logs hold `codex exec --json` events and are still read below.
	if method := str(m["method"]); method != "" {
		params, _ := m["params"].(map[string]any)
		return normalizeCodexRPC(method, params)
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "thread.started", "turn.started":
		return nil
	case "turn.completed":
		return []normEvent{{Kind: "usage", Payload: map[string]any{"usage": m["usage"], "agent": "codex"}}}
	case "item.started", "item.updated", "item.completed":
		item, _ := m["item"].(map[string]any)
		if item == nil {
			return nil
		}
		it, _ := item["type"].(string)
		switch it {
		case "agent_message":
			if txt, _ := item["text"].(string); txt != "" && typ == "item.completed" {
				return []normEvent{{Kind: "assistant", Payload: map[string]any{"text": txt}}}
			}
		case "reasoning":
			txt, _ := item["text"].(string)
			if txt == "" {
				txt = summaryText(item["summary"])
			}
			if txt != "" {
				return []normEvent{{Kind: "thinking", Payload: map[string]any{"text": txt}}}
			}
		case "command_execution":
			return []normEvent{{Kind: "tool", Payload: map[string]any{
				"id": str(item["id"]), "name": "shell", "status": str(item["status"]),
				"input":  map[string]any{"command": item["command"]},
				"output": item["aggregated_output"], "exit_code": item["exit_code"],
			}}}
		case "file_change":
			return []normEvent{{Kind: "tool", Payload: map[string]any{
				"id": str(item["id"]), "name": "file_change", "status": str(item["status"]),
				"input": item, "output": item["changes"],
			}}}
		case "mcp_tool_call":
			return []normEvent{{Kind: "tool", Payload: map[string]any{
				"id": str(item["id"]), "name": str(item["tool"]), "status": str(item["status"]),
				"input": item["arguments"], "output": item["result"],
			}}}
		case "web_search":
			return []normEvent{{Kind: "tool", Payload: map[string]any{
				"id": str(item["id"]), "name": "web_search", "status": str(item["status"]),
				"input": item["query"], "output": item["results"],
			}}}
		case "todo_list":
			return []normEvent{{Kind: "notice", Payload: map[string]any{"text": "Todo 列表已更新", "items": item["items"]}}}
		default:
			return []normEvent{{Kind: "tool", Payload: map[string]any{
				"id": str(item["id"]), "name": it, "status": str(item["status"]),
				"input": item, "output": item["output"],
			}}}
		}
	case "error":
		msg := str(m["message"])
		if msg == "" {
			msg = "Codex 错误"
		}
		return []normEvent{{Kind: "error", Payload: map[string]any{"text": msg}}}
	}
	return nil
}

// normalizeCodexRPC converts app-server notifications into the console's event
// kinds, reusing the names the exec-format path produces so the page renders
// both the same way.
func normalizeCodexRPC(method string, params map[string]any) []normEvent {
	switch method {
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		if item == nil {
			return nil
		}
		done := method == "item/completed"
		id := str(item["id"])
		switch str(item["type"]) {
		case "agentMessage":
			if txt := str(item["text"]); txt != "" && done {
				return []normEvent{{Kind: "assistant", Payload: map[string]any{"text": txt}}}
			}
		case "reasoning":
			if !done {
				return nil
			}
			txt := strings.TrimSpace(joinStrings(item["summary"]) + "\n\n" + joinStrings(item["content"]))
			if txt != "" {
				return []normEvent{{Kind: "thinking", Payload: map[string]any{"text": txt}}}
			}
		case "plan":
			if txt := str(item["text"]); txt != "" && done {
				return []normEvent{{Kind: "plan", Payload: map[string]any{"text": txt}}}
			}
		case "commandExecution":
			p := map[string]any{"id": id, "name": "shell", "status": codexItemStatus(item, done),
				"input": map[string]any{"command": item["command"], "cwd": item["cwd"]}}
			if done {
				p["output"], p["exit_code"] = item["aggregatedOutput"], item["exitCode"]
			}
			return []normEvent{{Kind: "tool", Payload: p}}
		case "fileChange":
			p := map[string]any{"id": id, "name": "file_change", "status": codexItemStatus(item, done),
				"input": map[string]any{"changes": item["changes"]}}
			if done {
				p["output"] = codexDiffs(item["changes"])
			}
			return []normEvent{{Kind: "tool", Payload: p}}
		case "mcpToolCall":
			p := map[string]any{"id": id, "name": str(item["server"]) + "." + str(item["tool"]),
				"status": codexItemStatus(item, done), "input": item["arguments"]}
			if done {
				out := item["result"]
				if e := item["error"]; e != nil {
					out = e
				}
				p["output"] = out
			}
			return []normEvent{{Kind: "tool", Payload: p}}
		case "webSearch":
			return []normEvent{{Kind: "tool", Payload: map[string]any{"id": id, "name": "web_search",
				"status": codexItemStatus(item, done), "input": map[string]any{"query": item["query"]}}}}
		case "contextCompaction":
			if done {
				return []normEvent{{Kind: "notice", Payload: map[string]any{"text": "上下文已压缩"}}}
			}
		case "enteredReviewMode":
			if done {
				return []normEvent{{Kind: "notice", Payload: map[string]any{"text": "开始代码审查：" + orDefault(str(item["review"]), "未提交的改动")}}}
			}
		case "exitedReviewMode":
			if txt := str(item["review"]); txt != "" && done {
				return []normEvent{{Kind: "assistant", Payload: map[string]any{"text": txt}}}
			}
		}
	case "rymesh/reply", legacyName + "/reply":
		return []normEvent{{Kind: "assistant", Payload: map[string]any{"text": str(params["text"])}}}
	case "rymesh/usage", legacyName + "/usage":
		u, _ := params["usage"].(map[string]any)
		return []normEvent{{Kind: "usage", Payload: map[string]any{"agent": "codex", "usage": map[string]any{
			"input_tokens": u["inputTokens"], "cached_input_tokens": u["cachedInputTokens"],
			"output_tokens": u["outputTokens"]}}}}
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		if e, _ := turn["error"].(map[string]any); e != nil && str(e["message"]) != "" {
			return []normEvent{{Kind: "error", Payload: map[string]any{"text": str(e["message"])}}}
		}
	case "error":
		e, _ := params["error"].(map[string]any)
		msg := orDefault(str(e["message"]), "Codex 错误")
		if b, _ := params["willRetry"].(bool); b {
			return []normEvent{{Kind: "notice", Payload: map[string]any{"text": msg + "（正在重试）"}}}
		}
		return []normEvent{{Kind: "error", Payload: map[string]any{"text": msg}}}
	}
	return nil
}

func codexItemStatus(item map[string]any, done bool) string {
	switch s := str(item["status"]); s {
	case "inProgress", "":
		if done {
			return "completed"
		}
		return "running"
	case "declined":
		return "failed"
	default:
		return s
	}
}

func joinStrings(v any) string {
	arr, _ := v.([]any)
	var parts []string
	for _, x := range arr {
		if s := str(x); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// codexDiffs renders a fileChange item's changes as one unified diff text.
func codexDiffs(v any) string {
	arr, _ := v.([]any)
	var b strings.Builder
	for _, x := range arr {
		ch, _ := x.(map[string]any)
		kind, _ := ch["kind"].(map[string]any)
		fmt.Fprintf(&b, "%s %s\n%s\n", str(kind["type"]), str(ch["path"]), str(ch["diff"]))
	}
	return strings.TrimSpace(b.String())
}

func normalizeClaude(raw any) []normEvent {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "system":
		sub, _ := m["subtype"].(string)
		switch sub {
		case "init":
			model, _ := m["model"].(string)
			return []normEvent{{Kind: "notice", Payload: map[string]any{
				"text": "Claude Code 会话已启动", "model": model, "native_id": m["session_id"],
			}}}
		case "permission_denied":
			return []normEvent{{Kind: "permission_denied", Payload: map[string]any{
				"tool_name": m["tool_name"], "tool_use_id": m["tool_use_id"],
				"message": m["message"], "reason": m["decision_reason_type"],
			}}}
		case "thinking_tokens":
			return nil
		}
	case "assistant":
		msg, _ := m["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var out []normEvent
		for _, c := range content {
			block, _ := c.(map[string]any)
			switch str(block["type"]) {
			case "text":
				if txt := str(block["text"]); txt != "" {
					out = append(out, normEvent{Kind: "assistant", Payload: map[string]any{"text": txt}})
				}
			case "thinking":
				if txt := str(block["thinking"]); txt != "" {
					out = append(out, normEvent{Kind: "thinking", Payload: map[string]any{"text": txt}})
				}
			case "tool_use":
				out = append(out, normEvent{Kind: "tool", Payload: map[string]any{
					"id": str(block["id"]), "name": str(block["name"]),
					"input": block["input"], "status": "running",
				}})
			}
		}
		return out
	case "user":
		msg, _ := m["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var out []normEvent
		for _, c := range content {
			block, _ := c.(map[string]any)
			if str(block["type"]) == "tool_result" {
				out = append(out, normEvent{Kind: "tool_result", Payload: map[string]any{
					"id": str(block["tool_use_id"]), "output": textOf(block["content"]),
					"is_error": block["is_error"],
				}})
			}
		}
		return out
	case "result":
		p := map[string]any{
			"subtype": m["subtype"], "is_error": m["is_error"], "result": m["result"],
			"cost_usd": m["total_cost_usd"], "usage": m["usage"],
			"duration_ms": m["duration_ms"], "stop_reason": m["stop_reason"],
			"num_turns": m["num_turns"],
		}
		return []normEvent{{Kind: "result", Payload: p}}
	}
	return nil
}

func textOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, it := range x {
			if mm, ok := it.(map[string]any); ok {
				if t := str(mm["text"]); t != "" {
					parts = append(parts, t)
				} else {
					b, _ := json.Marshal(mm)
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func summaryText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	arr, _ := v.([]any)
	var parts []string
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			if t := str(m["text"]); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
