//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a terminal: it answers a terminal's own
// ioctl. /dev/null is a character device but not a terminal, and must not count
// as one.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
