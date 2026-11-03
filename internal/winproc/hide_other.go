//go:build !windows

package winproc

import "os/exec"

// Hide does nothing here: only Windows opens a console window for a program
// started in the background.
func Hide(cmd *exec.Cmd) {}
