//go:build windows

package termlauncher

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// ownWindow gives the program a console window of its own. The server runs with
// a console that has no window, and a console program started from it would
// share that one: the person would press "open terminal" and nothing would
// appear, while a PowerShell they cannot see stayed running. The terminal
// is the one window clawdh is meant to open.
func ownWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
}
