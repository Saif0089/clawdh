package cli

import (
	"clawdh/internal/winproc"
	"os/exec"
	"strconv"
)

// endChild kills Claude Code and everything under it. Windows has no SIGTERM,
// and killing the direct child is not enough: an npm-installed Claude Code is
// claude.cmd, so the child clawdh started is cmd.exe and the node process doing
// the work is its child. Killing only the shim left that node process alive,
// still holding the console, to fight the session the supervisor relaunched
// into it. internal/ptyio hit the same shim and kills the tree for the same
// reason. Falls back to the direct child if taskkill is unavailable.
func endChild(cmd *exec.Cmd) {
	kill := winproc.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
