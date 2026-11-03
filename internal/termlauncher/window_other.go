//go:build !windows

package termlauncher

import "os/exec"

// ownWindow does nothing here: terminals on macOS and Linux open their own window.
func ownWindow(*exec.Cmd) {}
