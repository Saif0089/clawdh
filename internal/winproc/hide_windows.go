//go:build windows

package winproc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hide makes cmd start without a console window of its own. Flags the caller
// already set (a new process group, say) are kept.
//
// HideWindow alone asks the child's first window to start hidden, which is
// honoured for a console that Windows creates but not by every terminal host;
// CREATE_NO_WINDOW means no console window is created at all, and a console
// program started this way hands the same windowless console on to anything it
// starts, so a spawn further down cannot open one either.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
