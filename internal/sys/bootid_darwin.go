package sys

import (
	"encoding/hex"
	"syscall"
)

// platformBootID identifies the current boot on macOS, which has no
// /proc/sys/kernel/random/boot_id: the boot time changes on every boot.
func platformBootID() string {
	s, err := syscall.Sysctl("kern.boottime")
	if err != nil || s == "" {
		return ""
	}
	return hex.EncodeToString([]byte(s))
}
