//go:build darwin

package service

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"clawdh/internal/config"
)

const launchAgentLabel = "com.clawdh.agent"

// legacyLaunchAgentLabel is the pre-clawdh (ccam) LaunchAgent, removed on
// upgrade so it does not start a second daemon that fights for the port.
const legacyLaunchAgentLabel = "com.ccam.agent"

// removeLegacyPlatform boots out and deletes the old ccam LaunchAgent, if any.
// `launchctl bootout` both stops and unloads it, so this also stops a running
// old daemon; everything is best-effort.
func removeLegacyPlatform() {
	// Guarded like the rest: the legacy label is global to this login too, so a
	// test under a temp HOME must not unload the real machine's old agent.
	if target, err := guiTarget(); err == nil && isRealLogin() {
		_ = exec.Command("launchctl", "bootout", target+"/"+legacyLaunchAgentLabel).Run()
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Remove(filepath.Join(home, "Library", "LaunchAgents", legacyLaunchAgentLabel+".plist"))
	}
}

type darwinService struct{ generic }

func newPlatformService(binaryPath string, port int) Service {
	return &darwinService{generic{binaryPath: binaryPath, port: port}}
}

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func (d *darwinService) Install(binaryPath string, port int) (string, error) {
	path, err := launchAgentPath()
	if err != nil {
		return "", err
	}
	logPath, err := config.LogFile()
	if err != nil {
		return "", err
	}
	if err := config.EnsureDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := config.EnsureDir(filepath.Dir(logPath)); err != nil {
		return "", err
	}

	// launchd starts login agents with a bare PATH (roughly
	// /usr/bin:/bin:/usr/sbin:/sbin), but clawdh has to run `claude`,
	// which normally lives under the user's home and is itself a Node
	// program needing more of the user's PATH. Bake in the PATH of the
	// shell that ran `clawdh install`, which is exactly the environment
	// where the user's `claude` works.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>--port</string>
		<string>%d</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchAgentLabel, xmlEscape(binaryPath), port, xmlEscape(servicePATH()), xmlEscape(serviceWorkingDir()), xmlEscape(logPath), xmlEscape(logPath))

	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return "", err
	}

	// If the user previously turned this off in System Settings →
	// General → Login Items, launchd remembers that as a persistent
	// disabled flag, and rewriting the identical plist would leave it
	// off forever with nothing to show why. `enable` only clears that
	// flag — unlike `bootstrap` it does not start anything, so it
	// can't reintroduce the race described below.
	if target, err := guiTarget(); err == nil && isRealLoginAgent(path) {
		_ = exec.Command("launchctl", "enable", target+"/"+launchAgentLabel).Run()
	}

	// Register it with launchd now, rather than leaving the file to be
	// picked up at the next login.
	//
	// Writing the plist really is enough for "starts at login" — macOS
	// loads ~/Library/LaunchAgents at each one — and this used to stop
	// there, to avoid racing the Start() that callers make straight
	// after Install(). The cost of that was invisible and worse than the
	// race: between installing clawdh and next rebooting, the machine has
	// no registered autostart at all. On a machine that had been up for
	// days, six of the seven agents in that folder were loaded and
	// clawdh's — written after the last boot — was not. Anything that
	// killed the service in that window (a crash, a closed terminal,
	// KeepAlive being false) left it down with nothing to bring it back,
	// and a machine with no service is a machine that never updates.
	//
	// The race is handled where it belongs instead: bootstrapping starts
	// the agent, and the installer waits for that before deciding whether
	// it still needs to start one itself.
	if target, err := guiTarget(); err == nil && isRealLoginAgent(path) {
		// Boot out any previous registration first, so this is idempotent
		// and a re-install re-reads the plist it has just rewritten
		// (bootstrap on an already-loaded label is an error, and one that
		// would otherwise leave the old binary's registration in place).
		_ = exec.Command("launchctl", "bootout", target+"/"+launchAgentLabel).Run()
		if out, err := exec.Command("launchctl", "bootstrap", target, path).CombinedOutput(); err != nil {
			// Never fatal. The plist is written and enabled, so the next
			// login loads it the way it always did; refusing to install
			// over a grumpy launchd would be trading a working install for
			// a registration that is only an optimisation on top of it.
			log.Printf("clawdh: wrote %s but launchd would not register it now (%v: %s); it will start at your next login",
				path, err, strings.TrimSpace(string(out)))
		}
	}
	return path, nil
}

// isRealLoginAgent reports whether path is this login account's own
// LaunchAgents plist, rather than one written somewhere else under a
// redirected HOME.
//
// launchd has one namespace per user and the label in it is global, so
// `bootout com.clawdh.agent` unloads whichever clawdh is registered — no matter
// whose plist asked for it. A test that repoints HOME at a temp directory and
// then installs would therefore reach out of its sandbox and stop the real
// clawdh this person is using. It did: a run of the install/uninstall test took
// down the live service and left the label pointing at a plist in /var/folders.
//
// The passwd entry is the right thing to compare against precisely because it
// ignores $HOME, which is the variable a test moves.
func isRealLogin() bool {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return false
	}
	home, err := os.UserHomeDir()
	return err == nil && filepath.Clean(home) == filepath.Clean(u.HomeDir)
}

func isRealLoginAgent(path string) bool {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return false
	}
	want := filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchAgentLabel+".plist")
	return filepath.Clean(path) == filepath.Clean(want)
}

// AutostartActive reports whether launchd actually knows about the agent —
// not merely whether the plist file exists, which is what IsInstalled
// answers and what made this failure invisible.
func (d *darwinService) AutostartActive() bool {
	target, err := guiTarget()
	if err != nil {
		return false
	}
	return exec.Command("launchctl", "print", target+"/"+launchAgentLabel).Run() == nil
}

func (d *darwinService) Uninstall() error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if target, err := guiTarget(); err == nil && isRealLoginAgent(path) {
		_ = exec.Command("launchctl", "bootout", target+"/"+launchAgentLabel).Run()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return d.generic.Stop()
}

func (d *darwinService) IsInstalled() (bool, error) {
	path, err := launchAgentPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func guiTarget() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return "gui/" + u.Uid, nil
}
