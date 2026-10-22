package console

// Terminals: a login shell on this machine in a pseudo-terminal, for the
// page's terminal view. A hub reaches a server's terminal through the same
// /n/{node} relay as everything else, so this one implementation serves the
// local machine and every node. The shell runs as the console's own user,
// as an SSH login would.
//
// A terminal outlives the page that opened it: closing the view or putting
// the phone away keeps the shell, and the page reconnects at the byte offset
// it last saw. Output is kept as a scrollback window for that. A shell that
// nobody has watched for termIdle is hung up.

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	termScrollback = 256 << 10 // output a reconnecting page can catch up on
	termIdle       = 30 * time.Minute
	termMax        = 8
	termChunk      = 64 << 10 // largest output event
)

type terminal struct {
	id  string
	cwd string
	pty *os.File
	cmd *exec.Cmd

	mu       sync.Mutex
	buf      []byte // the last termScrollback bytes of output
	end      int64  // bytes of output so far; buf holds [end-len(buf), end)
	done     bool
	code     int
	wake     chan struct{} // closed and replaced on output or exit
	viewers  int
	lastSeen time.Time
}

type termManager struct {
	mu       sync.Mutex
	terms    map[string]*terminal
	starting int // opens past the limit check, not yet in terms
}

func newTermManager() *termManager {
	m := &termManager{terms: map[string]*terminal{}}
	go m.reap()
	return m
}

func (m *termManager) get(id string) (*terminal, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.terms[id]
	return t, ok
}

func (m *termManager) start(cwd string, cols, rows int) (*terminal, error) {
	// Hold a slot from the check on, so concurrent opens cannot all pass the
	// limit before any of them is registered.
	m.mu.Lock()
	if len(m.terms)+m.starting >= termMax {
		m.mu.Unlock()
		return nil, fmt.Errorf("已打开 %d 个终端，请先关闭不用的", termMax)
	}
	m.starting++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.starting--
		m.mu.Unlock()
	}()

	master, slavePath, err := openPTY()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close()
	setWinsize(master, cols, rows)

	shell := termShell()
	cmd := exec.Command(shell)
	// A leading "-" in argv[0] is how every shell is told it is a login shell.
	cmd.Args = []string{"-" + filepath.Base(shell)}
	cmd.Dir = cwd
	cmd.Env = termEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	t := &terminal{id: randomID(12), cwd: cwd, pty: master, cmd: cmd,
		wake: make(chan struct{}), lastSeen: time.Now()}
	m.mu.Lock()
	m.terms[t.id] = t
	m.mu.Unlock()
	go t.pump(m)
	return t, nil
}

// pump copies the shell's output into the scrollback until the shell exits.
func (t *terminal) pump(m *termManager) {
	b := make([]byte, 32<<10)
	for {
		n, err := t.pty.Read(b)
		if n > 0 {
			t.mu.Lock()
			t.buf = append(t.buf, b[:n]...)
			if over := len(t.buf) - termScrollback; over > 0 {
				t.buf = append(t.buf[:0:0], t.buf[over:]...)
			}
			t.end += int64(n)
			t.notify()
			t.mu.Unlock()
		}
		if err != nil {
			// Linux reports EIO once the last process holding the slave is
			// gone; either way the shell is finished.
			break
		}
	}
	code := 0
	if err := t.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	t.pty.Close()
	t.mu.Lock()
	t.done, t.code = true, code
	t.notify()
	t.mu.Unlock()
	// Leave the record a moment so a connected page can show the exit.
	time.AfterFunc(time.Minute, func() {
		m.mu.Lock()
		delete(m.terms, t.id)
		m.mu.Unlock()
	})
}

// notify wakes every stream waiting for output. Callers hold t.mu.
func (t *terminal) notify() {
	close(t.wake)
	t.wake = make(chan struct{})
}

// hangup ends the shell and everything it started.
func (t *terminal) hangup() {
	if p := t.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGHUP)
		time.AfterFunc(3*time.Second, func() {
			t.mu.Lock()
			done := t.done
			t.mu.Unlock()
			if !done { // once reaped, the PID may belong to someone else
				_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
			}
		})
	}
}

func (m *termManager) reap() {
	for range time.Tick(time.Minute) {
		m.mu.Lock()
		var idle []*terminal
		for _, t := range m.terms {
			t.mu.Lock()
			if !t.done && t.viewers == 0 && time.Since(t.lastSeen) > termIdle {
				idle = append(idle, t)
			}
			t.mu.Unlock()
		}
		m.mu.Unlock()
		for _, t := range idle {
			t.hangup()
		}
	}
}

func setWinsize(f *os.File, cols, rows int) {
	if cols < 2 || rows < 2 || cols > 1000 || rows > 1000 {
		return
	}
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
}

// termShell is the user's shell: $SHELL, else their passwd entry (a
// service started by systemd has no $SHELL), else bash or sh.
func termShell() string {
	if sh := os.Getenv("SHELL"); sh != "" && isExecutable(sh) {
		return sh
	}
	if f, err := os.Open("/etc/passwd"); err == nil {
		defer f.Close()
		uid := strconv.Itoa(os.Getuid())
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p := strings.Split(sc.Text(), ":")
			if len(p) >= 7 && p[2] == uid && isExecutable(p[6]) {
				return p[6]
			}
		}
	}
	for _, sh := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if isExecutable(sh) {
			return sh
		}
	}
	return "/bin/sh"
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// termEnv is this process's environment with what a terminal needs: the
// terminal type xterm.js emulates, and a UTF-8 locale when none is set (a
// systemd service or a Finder-launched app has none, and Chinese output
// would come out as escapes).
func termEnv() []string {
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor"}
	hasLocale := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		// The rest describe the terminal this process was started from (a
		// zsh that sees Apple Terminal's session id prints "Restored session").
		case "TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID":
			continue
		case "LANG", "LC_ALL", "LC_CTYPE":
			hasLocale = true
		}
		env = append(env, kv)
	}
	if !hasLocale {
		if runtime.GOOS == "darwin" {
			env = append(env, "LANG=en_US.UTF-8")
		} else {
			env = append(env, "LANG=C.UTF-8")
		}
	}
	return env
}

// ---------------------------------------------------------------- handlers

func (s *Server) handleTermCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cwd  string `json:"cwd"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	cfg := s.config()
	if cfg.HubOnly {
		writeErr(w, 400, "这台设备没有本机终端")
		return
	}
	cwd, err := cfg.allowedCWD(req.Cwd)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t, err := s.terms.start(cwd, req.Cols, req.Rows)
	if err != nil {
		writeErr(w, 500, "无法打开终端: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": t.id, "cwd": t.cwd})
}

func (s *Server) termFor(w http.ResponseWriter, r *http.Request) (*terminal, bool) {
	t, ok := s.terms.get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "终端已结束")
	}
	return t, ok
}

// handleTermStream sends output from byte offset ?after= on, then follows
// the shell live. Output older than the scrollback is gone; the stream then
// starts at the oldest byte it still has.
func (s *Server) handleTermStream(w http.ResponseWriter, r *http.Request) {
	t, ok := s.termFor(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprintf(w, "retry: 2000\n\n")
	fl.Flush()

	t.mu.Lock()
	t.viewers++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.viewers--
		t.lastSeen = time.Now()
		t.mu.Unlock()
	}()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		t.mu.Lock()
		start := t.end - int64(len(t.buf))
		if after < start || after > t.end {
			after = start
		}
		data := t.buf[after-start:]
		if len(data) > termChunk {
			data = data[:termChunk]
		}
		out := base64.StdEncoding.EncodeToString(data)
		at := after
		after += int64(len(data))
		more := after < t.end
		done, code, wake := t.done, t.code, t.wake
		t.mu.Unlock()

		if len(data) > 0 {
			fmt.Fprintf(w, "event: out\ndata: {\"at\":%d,\"d\":%q}\n\n", at, out)
			fl.Flush()
		}
		if more {
			continue
		}
		if done {
			fmt.Fprintf(w, "event: exit\ndata: {\"code\":%d}\n\n", code)
			fl.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) handleTermInput(w http.ResponseWriter, r *http.Request) {
	t, ok := s.termFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Data string `json:"data"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if _, err := t.pty.Write([]byte(req.Data)); err != nil {
		writeErr(w, 410, "终端已结束")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleTermResize(w http.ResponseWriter, r *http.Request) {
	t, ok := s.termFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	setWinsize(t.pty, req.Cols, req.Rows)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleTermClose(w http.ResponseWriter, r *http.Request) {
	t, ok := s.termFor(w, r)
	if !ok {
		return
	}
	t.hangup()
	writeJSON(w, 200, map[string]any{"ok": true})
}
