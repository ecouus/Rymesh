package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TaskSpec is written by the web server before the detached runner is started.
// The runner never talks to the database; files are the whole contract, which is
// what lets agent processes outlive the web service.
type TaskSpec struct {
	TaskID      string `json:"task_id"`
	SessionID   string `json:"session_id"`
	Agent       string `json:"agent"`
	NativeID    string `json:"native_id"`
	IsNew       bool   `json:"is_new"`
	Cwd         string `json:"cwd"`
	Prompt      string `json:"prompt"`
	Profile     string `json:"profile"`
	MaxLogBytes int64  `json:"max_log_bytes"`
	PermTimeout int    `json:"permission_timeout_seconds"`
	RunnerID    string `json:"runner_id"`
	Executable  string `json:"executable"`
	// Per-conversation settings (slash.go), applied to every message.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type eventWriter struct {
	mu     sync.Mutex
	f      *os.File
	seq    int64
	max    int64
	size   int64
	capped bool
}

func newEventWriter(path string, startSeq int64, max int64) (*eventWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return &eventWriter{f: f, seq: startSeq, max: max, size: size}, nil
}

func (w *eventWriter) append(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.capped {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.seq++
	wrap := map[string]any{
		"seq":     w.seq,
		"ts":      nowRFC3339(),
		"stream":  "event",
		"payload": json.RawMessage(b),
	}
	// State/notice/exit helpers build their own envelope via appendRaw.
	line, _ := json.Marshal(wrap)
	if w.max > 0 && w.size+int64(len(line))+1 > w.max {
		w.capped = true
		notice := map[string]any{
			"seq": w.seq, "ts": nowRFC3339(), "stream": "notice",
			"text": fmt.Sprintf("日志已达上限 %d 字节，后续输出不再记录；Agent 仍在继续运行。", w.max),
		}
		nb, _ := json.Marshal(notice)
		w.f.Write(append(nb, '\n'))
		w.f.Sync()
		return
	}
	n, _ := w.f.Write(append(line, '\n'))
	w.size += int64(n)
}

func (w *eventWriter) appendEnvelope(env map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	stream, _ := env["stream"].(string)
	// Even after the output cap, state and exit records must survive: they are
	// how the web service learns the true final status.
	if w.capped && stream != "exit" && stream != "state" {
		return
	}
	w.seq++
	env["seq"] = w.seq
	if _, ok := env["ts"]; !ok {
		env["ts"] = nowRFC3339()
	}
	line, _ := json.Marshal(env)
	if w.max > 0 && w.size+int64(len(line))+1 > w.max {
		w.capped = true
		// The exit record is how the web service learns the true final status.
		// Dropping it made the task look interrupted ("runner 进程已退出且没有
		// 退出记录") and lost the real exit code, so it always gets written.
		if stream != "exit" {
			return
		}
	}
	n, _ := w.f.Write(append(line, '\n'))
	w.size += int64(n)
}

func (w *eventWriter) notice(text string) {
	w.appendEnvelope(map[string]any{"stream": "notice", "text": text})
}

func (w *eventWriter) state(st map[string]any) {
	w.appendEnvelope(map[string]any{"stream": "state", "state": st})
}

func (w *eventWriter) stderr(line string) {
	w.appendEnvelope(map[string]any{"stream": "stderr", "line": line})
}

func (w *eventWriter) exit(exit map[string]any) {
	w.appendEnvelope(map[string]any{"stream": "exit", "exit": exit})
}

func (w *eventWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.f.Sync()
	w.f.Close()
}

func readLines(r io.Reader, max int64) <-chan []byte {
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			var buf []byte
			for {
				part, err := br.ReadSlice('\n')
				buf = append(buf, part...)
				if err == bufio.ErrBufferFull {
					if max > 0 && int64(len(buf)) > max {
						buf = buf[:max]
						// Drain the rest of this physical line.
						for {
							_, e := br.ReadSlice('\n')
							if e != bufio.ErrBufferFull {
								break
							}
						}
						break
					}
					continue
				}
				break
			}
			line := strings.TrimRight(string(buf), "\r\n")
			if line != "" {
				// Block rather than drop: a slow consumer (e.g. a stalled disk
				// write) must apply back-pressure, not silently lose output.
				ch <- []byte(line)
			}
			if len(buf) > 0 && buf[len(buf)-1] == '\n' {
				continue
			}
			// EOF or oversized line.
			if br.Buffered() == 0 {
				return
			}
		}
	}()
	return ch
}

func currentBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return platformBootID()
	}
	return strings.TrimSpace(string(b))
}

func atomicWriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runTaskMain(taskDir string) int {
	specPath := filepath.Join(taskDir, "task.json")
	b, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-task:", err)
		return 2
	}
	var spec TaskSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "run-task: bad task.json:", err)
		return 2
	}
	control := filepath.Join(taskDir, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "run-task:", err)
		return 2
	}
	eventsPath := filepath.Join(taskDir, "events.jsonl")
	startSeq := int64(0)
	if st, err := os.Stat(eventsPath); err == nil {
		// Existing events: continue numbering after the last line.
		startSeq = countLines(eventsPath)
		_ = st
	}
	w, err := newEventWriter(eventsPath, startSeq, spec.MaxLogBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-task:", err)
		return 2
	}
	defer w.close()

	bootID := currentBootID()
	statePath := filepath.Join(taskDir, "state.json")
	writeState := func(extra map[string]any) {
		st := map[string]any{
			"task_id": spec.TaskID, "session_id": spec.SessionID,
			"runner_pid": os.Getpid(), "boot_id": bootID,
			"updated_at": nowRFC3339(),
		}
		for k, v := range extra {
			st[k] = v
		}
		_ = atomicWriteJSON(statePath, st)
	}
	writeState(map[string]any{"status": StatusRunning})
	if err := os.WriteFile(filepath.Join(taskDir, "runner.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "run-task: write pid:", err)
	}
	w.state(map[string]any{"status": StatusRunning, "runner_pid": os.Getpid(), "boot_id": bootID})

	cmd, err := buildAgentCommand(spec, control)
	if err != nil {
		w.notice("无法启动 Agent: " + err.Error())
		w.exit(map[string]any{"code": 127, "signal": "", "reason": "failed", "error": err.Error()})
		writeState(map[string]any{"status": StatusFailed, "error": err.Error()})
		return 127
	}
	cmd.Dir = spec.Cwd
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		w.exit(map[string]any{"code": 127, "reason": "failed", "error": err.Error()})
		return 127
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.exit(map[string]any{"code": 127, "reason": "failed", "error": err.Error()})
		return 127
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		w.exit(map[string]any{"code": 127, "reason": "failed", "error": err.Error()})
		return 127
	}
	// Own process group: cancellation can signal the agent and its tool children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		w.notice("Agent 启动失败: " + err.Error())
		w.exit(map[string]any{"code": 127, "reason": "failed", "error": err.Error()})
		writeState(map[string]any{"status": StatusFailed, "error": err.Error()})
		return 127
	}
	childPID := cmd.Process.Pid
	_ = os.WriteFile(filepath.Join(taskDir, "child.pid"), []byte(fmt.Sprintf("%d\n", childPID)), 0o600)
	writeState(map[string]any{"status": StatusRunning, "child_pid": childPID})
	w.state(map[string]any{"status": StatusRunning, "child_pid": childPID, "native_id": spec.NativeID})

	// stop releases requests still waiting for the user once the agent exits.
	stop := make(chan struct{})
	sess := newAgentSession(spec, control, w, stdin, stop, func(id string) {
		w.state(map[string]any{"native_id": id})
		writeState(map[string]any{"status": StatusRunning, "child_pid": childPID, "native_id": id})
	})
	go sess.start()

	var cancel atomic.Bool
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	var sigMu sync.Mutex
	var sigName string
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				continue
			}
			sigMu.Lock()
			sigName = sig.String()
			sigMu.Unlock()
			cancel.Store(true)
			w.notice("收到 " + sig.String() + "，正在停止 Agent…")
			signalChild(childPID, syscall.SIGINT)
			go func() {
				time.Sleep(4 * time.Second)
				if processAlive(childPID) {
					signalChild(childPID, syscall.SIGTERM)
				}
				time.Sleep(4 * time.Second)
				if processAlive(childPID) {
					_ = syscall.Kill(-childPID, syscall.SIGKILL)
				}
			}()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for line := range readLines(stdout, 8<<20) {
			sess.onLine(line)
		}
	}()
	go func() {
		defer wg.Done()
		for line := range readLines(stderr, 1<<20) {
			w.stderr(string(line))
		}
	}()
	// A grandchild (a shell tool that daemonizes) can inherit the stdout pipe
	// and keep it open after the agent exits, so reading until EOF would wedge
	// the runner forever and the task would never get its exit record. Wait for
	// the process itself, then give the readers a short grace period.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	err = cmd.Wait()
	close(stop)
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		_ = stdout.Close()
		_ = stderr.Close()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
		}
	}
	code := 0
	signalName := ""
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signalName = ws.Signal().String()
			}
		} else {
			code = 1
		}
	}
	sigMu.Lock()
	gotSig := sigName
	sigMu.Unlock()
	reason := "exited"
	status := StatusCompleted
	switch {
	case cancel.Load() || gotSig != "":
		reason = "cancelled"
		status = StatusCancelled
	case signalName != "":
		// The agent CLI itself died from a signal (crash or external kill).
		// "interrupted" is reserved for tasks whose outcome was lost because
		// the runner or host died, so this is a failed task.
		reason = "signaled"
		status = StatusFailed
	case code != 0:
		reason = "failed"
		status = StatusFailed
	}
	// A protocol that reports its own outcome (Codex's turn/completed) is more
	// precise than the exit code: app-server exits 0 even after a failed turn.
	if st, msg, ok := sess.outcome(); ok && reason != "cancelled" {
		status = st
		switch st {
		case StatusCompleted:
			reason, code = "exited", 0
		case StatusCancelled:
			reason = "cancelled"
		default:
			reason = "failed"
			if code == 0 {
				code = 1
			}
			w.notice(msg)
		}
	}
	exit := map[string]any{
		"code": code, "signal": signalName, "reason": reason,
		"status": status, "ended_at": nowRFC3339(),
	}
	w.exit(exit)
	writeState(map[string]any{"status": status, "exit_code": code,
		"signal": signalName, "reason": reason, "ended_at": nowRFC3339()})
	w.notice("任务结束：" + status)
	return code
}

func countLines(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var n int64
	buf := make([]byte, 64*1024)
	for {
		c, err := f.Read(buf)
		for i := 0; i < c; i++ {
			if buf[i] == '\n' {
				n++
			}
		}
		if err != nil {
			break
		}
	}
	return n
}

func signalChild(pid int, sig syscall.Signal) {
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, sig)
	_ = syscall.Kill(pid, sig)
}

func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// processCmdline returns a process's command line with arguments separated
// by spaces, for telling a recorded PID from a process that reused it.
// Linux has /proc; macOS has no /proc, so ask ps there.
func processCmdline(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")), nil
	}
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("no process %d", pid)
}

// isTaskRunner reports whether pid is still the runner of taskID
// (`rymesh run-task --task-dir …/tasks/<id>`).
func isTaskRunner(pid int, taskID string) bool {
	cl, err := processCmdline(pid)
	return err == nil && strings.Contains(cl, "run-task") && strings.Contains(cl, taskID)
}

func extractNativeID(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := m["thread_id"].(string); ok && id != "" {
		return id
	}
	if sid, ok := m["session_id"].(string); ok && sid != "" {
		return sid
	}
	if st, ok := m["subtype"].(string); ok && st == "init" {
		if sid, ok := m["session_id"].(string); ok {
			return sid
		}
	}
	return ""
}

func buildAgentCommand(spec TaskSpec, controlDir string) (*exec.Cmd, error) {
	agent, ok := agents.Get(spec.Agent)
	if !ok || !agent.Available || agent.Binary == "" {
		return nil, fmt.Errorf("agent %q 不可用", spec.Agent)
	}
	profile := spec.Profile
	if !validProfile(spec.Agent, profile) {
		profile = defaultProfileFor(spec.Agent)
	}
	switch spec.Agent {
	case "codex":
		// Sandbox, approval policy and plan mode travel in the app-server
		// thread and turn requests (codexrpc.go), not on the command line.
		return exec.Command(agent.Binary, "app-server"), nil
	case "claude":
		// stream-json input plus the stdio permission tool is the Agent SDK's
		// control protocol: permission prompts, AskUserQuestion and plan
		// approval arrive as can_use_tool requests (agent_claude.go).
		mode := profile
		if mode == "manual" {
			mode = "default"
		}
		args := []string{"-p",
			"--input-format", "stream-json",
			"--output-format", "stream-json", "--verbose",
			"--permission-mode", mode,
			"--permission-prompt-tool", "stdio",
		}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if validEffort(spec.Effort) {
			args = append(args, "--effort", spec.Effort)
		}
		if profile == "bypassPermissions" {
			args = append(args, "--dangerously-skip-permissions")
		}
		if spec.IsNew || spec.NativeID == "" {
			nid := spec.NativeID
			if nid == "" {
				nid = newUUID()
			}
			args = append(args, "--session-id", nid)
		} else {
			args = append(args, "--resume", spec.NativeID)
		}
		return exec.Command(agent.Binary, args...), nil
	}
	return nil, fmt.Errorf("unsupported agent %q", spec.Agent)
}
