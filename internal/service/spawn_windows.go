//go:build windows

package service

import (
	"os"
	"path/filepath"

	"clawdh/internal/config"
	"clawdh/internal/winproc"
	"golang.org/x/sys/windows"
)

// spawnDetached starts binaryPath with no console window (so nothing shows on
// screen at login or after an update) and output redirected to clawdh's log
// file, and returns its PID.
func spawnDetached(binaryPath string, args []string) (int, error) {
	logPath, err := config.LogFile()
	if err != nil {
		return 0, err
	}
	if err := config.EnsureDir(filepath.Dir(logPath)); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd := winproc.Command(binaryPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// No console window, and not the launcher's console either. The server used
	// to share the console of whatever started it — a terminal the person
	// typed `clawdh start` in, or the minimized window the Startup script made
	// for it — so closing that window ended clawdh, and a visible window stood
	// for the server's whole life. A console of its own with no window also
	// means everything the server starts inherits a window-less console.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	const stillActive = 259
	return exitCode == stillActive
}

func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
