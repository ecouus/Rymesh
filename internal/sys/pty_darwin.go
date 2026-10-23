package sys

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenPTY returns the master side of a new pseudo-terminal and the path of
// its slave side.
func OpenPTY() (*os.File, string, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	// grantpt and unlockpt. ptsname is derived from the master's minor
	// number, which is what libc's TIOCPTYGNAME reports as /dev/ttysNNN.
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		unix.Close(fd)
		return nil, "", err
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		unix.Close(fd)
		return nil, "", err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(st.Rdev))), nil
}
