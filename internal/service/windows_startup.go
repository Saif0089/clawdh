//go:build windows

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"clawdh/internal/config"
	"clawdh/internal/winproc"
)

const startupScriptName = "clawdh-autostart.cmd"

// legacyStartupScriptName / the "ccam" scheduled task are the pre-clawdh
// autostart entries, removed on upgrade so they do not start a second daemon
// that fights for the port.
const legacyStartupScriptName = "ccam-autostart.cmd"

// removeLegacyPlatform deletes the old ccam Startup script and scheduled task,
// if any. All best-effort.
func removeLegacyPlatform() {
	_ = winproc.Command("schtasks", "/Delete", "/TN", "ccam", "/F").Run()
	if folder, err := startupFolder(); err == nil {
		_ = os.Remove(filepath.Join(folder, legacyStartupScriptName))
	}
	// The old Scheduled-Task fallback kept its script under the old ~/.ccam home.
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Remove(filepath.Join(home, ".ccam", legacyStartupScriptName))
	}
}

type windowsService struct{ generic }

func newPlatformService(binaryPath string, port int) Service {
	return &windowsService{generic{binaryPath: binaryPath, port: port}}
}

// startupFolder returns the per-user Startup folder — anything placed
// here runs automatically at login, with no elevation/admin required.
func startupFolder() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup"), nil
}

func startupScriptPath() (string, error) {
	folder, err := startupFolder()
	if err != nil {
		return "", err
	}
	return filepath.Join(folder, startupScriptName), nil
}

func (w *windowsService) Install(binaryPath string, port int) (string, error) {
	path, err := startupScriptPath()
	if err != nil {
		return "", err
	}

	// Clear a Scheduled Task left by an earlier install that had to
	// fall back to one; otherwise both it and the Startup script fire
	// at logon and one of the two clawdhs loses the port.
	_ = winproc.Command("schtasks", "/Delete", "/TN", "clawdh", "/F").Run()

	script := autostartScript(binaryPath, port)

	if err := config.EnsureDir(filepath.Dir(path)); err != nil {
		return w.installScheduledTaskFallback(binaryPath, port)
	}
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return w.installScheduledTaskFallback(binaryPath, port)
	}
	return path, nil
}

// installScheduledTaskFallback covers the rare case where the per-user
// Startup folder isn't writable (some managed/redirected profiles). A
// Scheduled Task run at logon with /RL LIMITED still needs no
// admin/elevation — it runs at the standard user's own privilege level.
func (w *windowsService) installScheduledTaskFallback(binaryPath string, port int) (string, error) {
	// The task runs the same script the Startup folder would, rather
	// than an inline command line. Inlining it put the whole PATH into
	// /TR, which schtasks truncates at 261 characters — so on the very
	// managed profiles this fallback exists for, registration failed.
	// It also needs the same PATH and working directory, or this path
	// reproduces the bug where clawdh cannot find claude at all.
	scriptPath, err := fallbackScriptPath()
	if err != nil {
		return "", err
	}
	if err := config.EnsureDir(filepath.Dir(scriptPath)); err != nil {
		return "", fmt.Errorf("installing autostart: %w", err)
	}
	if err := os.WriteFile(scriptPath, []byte(autostartScript(binaryPath, port)), 0o644); err != nil {
		return "", fmt.Errorf("installing autostart: %w", err)
	}

	cmd := winproc.Command("schtasks", "/Create", "/F",
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/TN", "clawdh",
		"/TR", `"`+scriptPath+`"`,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("installing autostart (Startup folder and Scheduled Task both failed): %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return "schtasks:clawdh", nil
}

// fallbackScriptPath is where the Scheduled Task's script lives, in
// clawdh's own per-user directory rather than the Startup folder that was
// unwritable.
func fallbackScriptPath() (string, error) {
	base, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, startupScriptName), nil
}

func (w *windowsService) Uninstall() error {
	path, err := startupScriptPath()
	if err == nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
	}
	// Removing a task that doesn't exist is a no-op error we can ignore.
	_ = winproc.Command("schtasks", "/Delete", "/TN", "clawdh", "/F").Run()
	if scriptPath, err := fallbackScriptPath(); err == nil {
		_ = os.Remove(scriptPath)
	}
	return w.generic.Stop()
}

// IsInstalled reports whether a start-at-login entry of the current kind is in
// place. A Startup script written by an earlier build does not count: it starts
// clawdh in a console window that then stays on screen for the server's whole
// life, so ensureAutostart replaces it, and the first start after an update
// does the replacing.
func (w *windowsService) IsInstalled() (bool, error) {
	path, err := startupScriptPath()
	if err != nil {
		return false, err
	}
	if script, readErr := os.ReadFile(path); readErr == nil && isCurrentAutostartScript(string(script)) {
		return true, nil
	}
	// The Scheduled Task fallback runs a script of its own, which has to be
	// the current kind too.
	if fallback, fbErr := fallbackScriptPath(); fbErr == nil {
		if script, readErr := os.ReadFile(fallback); readErr == nil && isCurrentAutostartScript(string(script)) {
			return winproc.Command("schtasks", "/Query", "/TN", "clawdh").Run() == nil, nil
		}
	}
	return false, nil
}
