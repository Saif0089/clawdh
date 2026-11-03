// Package winproc starts the helper programs clawdh runs in the background
// without putting a window on anyone's screen.
//
// On Windows a program that is not a window (powershell, cmd, schtasks,
// taskkill, claude's own .cmd shim) is a console program, and Windows gives a
// console program a console window of its own unless the parent says
// otherwise — or lets it share the parent's console, which is no better when
// the parent's console is itself a visible window. Both showed up as terminal
// and PowerShell windows opening on people's screens around an update: the
// update path alone runs a notification through PowerShell and restarts the
// server. Hiding the window with STARTUPINFO is not enough on its own either:
// where Windows Terminal is the default terminal, a console program started
// without a console can still open a Windows Terminal window. CREATE_NO_WINDOW
// is the flag that means "no console window, ever", so every background spawn
// goes through here and gets it.
//
// Two kinds of spawn deliberately do not: a terminal the person asked for (the
// page's "open terminal" button) and the claude session `clawdh run` hands the
// person's own terminal to. test/guard fails the build for any other
// os/exec call in code that can run on Windows, so a new spawn cannot quietly
// bring the windows back.
//
// Everywhere except Windows these are plain os/exec.
package winproc

import (
	"context"
	"os/exec"
)

// Command is exec.Command whose child never shows a window.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	Hide(cmd)
	return cmd
}

// CommandContext is exec.CommandContext whose child never shows a window.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	Hide(cmd)
	return cmd
}
