//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a console the person can see, rather than
// the log file the background server's output goes to. A console answers
// GetConsoleMode; a file, a pipe or NUL does not.
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
