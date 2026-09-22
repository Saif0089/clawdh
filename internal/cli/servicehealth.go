package cli

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"clawdh/internal/config"
	"clawdh/internal/service"
)

// Keeping the service alive from the outside.
//
// clawdh's background service is what serves the page, reads plan usage, checks
// in with a panel, and — the part that bites hardest when it is missing —
// updates the machine. Starting it at login is the OS's job: a LaunchAgent, a
// systemd user unit, a script in the Startup folder. All three can quietly not
// happen. A Startup folder redirected into OneDrive, a login item someone
// turned off in System Settings, a LaunchAgent written after the last reboot
// and so never loaded: the machine comes up, clawdh does not, and nothing says
// so. It stays that way until somebody notices they are months behind.
//
// What that failure never stops is clawdh itself being run. Every `clawdh
// <name>`, every shared session, every chat an editor starts goes through this
// binary. So each of those quietly makes sure the service is up. It costs one
// check of a port, it happens off to the side of launching Claude, and it means
// the longest a machine can be without its service is until the next time its
// owner actually uses clawdh — which is the only interval that matters.
//
// This is a safety net, not a replacement for autostart: `clawdh install` still
// registers the real thing, because the page should be there when someone opens
// the browser without having run anything.

// ensureServiceRunning starts the background service if nothing is serving.
//
// It never blocks the caller and never reports failure to the person: they
// asked to run Claude, not to administer a service. A failure is a log line for
// whoever goes looking.
func ensureServiceRunning() {
	go func() {
		defer func() {
			// A session must not die because the safety net threw.
			if r := recover(); r != nil {
				log.Printf("clawdh: could not check on the background service: %v", r)
			}
		}()
		if !isClawdhBinary() {
			return
		}
		if running, err := service.IsHTTPRunning(); err != nil || running {
			return
		}
		binaryPath, err := service.SelfPath()
		if err != nil {
			return
		}
		if err := service.New(binaryPath, servicePort()).Start(); err != nil {
			log.Printf("clawdh: the background service was not running and could not be started: %v", err)
		}
	}()
}

// isClawdhBinary reports whether the running executable is clawdh itself.
//
// Only a real clawdh may start a clawdh. Without this the net fires from
// anything that links this package — above all `go test`, where every case
// touching a session path spawns a detached server from the test binary, on
// the default port, against the machine's real service. It did exactly that:
// the run bred processes until the machine could not fork at all and had to be
// power-cycled. A name check is crude, and that is the point — it cannot fail
// open the way a build tag or an environment variable can.
func isClawdhBinary() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	name := strings.ToLower(filepath.Base(exe))
	return name == "clawdh" || name == "clawdh.exe"
}

// servicePort is the port the service should come up on: the one it last
// recorded, and otherwise the default. The recorded file is removed when the
// service shuts down cleanly, so on a machine that has just booted this is
// almost always the default — which is the port every ordinary install uses.
func servicePort() int {
	path, err := config.PortFile()
	if err != nil {
		return config.DefaultPort
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config.DefaultPort
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port <= 0 || port > 65535 {
		return config.DefaultPort
	}
	return port
}
