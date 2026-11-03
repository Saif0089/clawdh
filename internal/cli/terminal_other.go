//go:build !windows && !linux && !darwin

package cli

import "os"

// isTerminal is the best this platform offers without a terminal ioctl: a
// character device. clawdh is released only for macOS, Linux and Windows.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
