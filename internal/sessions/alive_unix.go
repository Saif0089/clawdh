//go:build !windows

package sessions

import "syscall"

// Alive reports whether a process is still running. Signal 0 asks the kernel
// about it without touching it: no such process means the supervisor that
// would have acted on a switch is long gone. EPERM means it exists under
// another user, which is not one of ours either.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
