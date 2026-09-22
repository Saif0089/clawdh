package sessions

import "golang.org/x/sys/windows"

// Alive reports whether a process is still running. Windows has no signal 0,
// so the handle is opened for query only and its exit code inspected:
// STILL_ACTIVE means the process is there, anything else (or a handle that
// cannot be opened) means the supervisor that would have acted on a switch is
// gone.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
