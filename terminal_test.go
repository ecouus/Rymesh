package console

import (
	"strings"
	"testing"
	"time"
)

// waitOutput waits until the terminal's output contains want.
func waitOutput(t *testing.T, term *terminal, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		term.mu.Lock()
		got, wake, done := string(term.buf), term.wake, term.done
		term.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		if done {
			t.Fatalf("shell exited before printing %q; output:\n%s", want, got)
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("no %q in output:\n%s", want, got)
		}
	}
}

func TestTerminalRunsAShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	dir := t.TempDir()
	m := &termManager{terms: map[string]*terminal{}}
	term, err := m.start(dir, 100, 30)
	if err != nil {
		t.Fatal(err)
	}

	// The shell starts in the requested directory, sees a terminal of the
	// requested size, and echoes UTF-8 untouched.
	if _, err := term.pty.Write([]byte("pwd; stty size; echo 终端-$((40+2))\n")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, term, "终端-42")
	waitOutput(t, term, "30 100")
	term.mu.Lock()
	out := string(term.buf)
	term.mu.Unlock()
	if !strings.Contains(out, strings.TrimPrefix(dir, "/private")) {
		t.Fatalf("shell did not start in %s:\n%s", dir, out)
	}

	setWinsize(term.pty, 120, 40)
	if _, err := term.pty.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, term, "40 120")

	term.hangup()
	deadline := time.After(10 * time.Second)
	for {
		term.mu.Lock()
		done, wake := term.done, term.wake
		term.mu.Unlock()
		if done {
			break
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatal("shell still running after hangup")
		}
	}
}
