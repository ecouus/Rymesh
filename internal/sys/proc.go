package sys

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func CurrentBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return platformBootID()
	}
	return strings.TrimSpace(string(b))
}

func SignalChild(pid int, sig syscall.Signal) {
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, sig)
	_ = syscall.Kill(pid, sig)
}

func ProcessAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// ProcessCmdline returns a process's command line with arguments separated
// by spaces, for telling a recorded PID from a process that reused it.
// Linux has /proc; macOS has no /proc, so ask ps there.
func ProcessCmdline(pid int) (string, error) {
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
