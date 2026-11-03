package service

import (
	"fmt"
	"strings"
)

// autostartScriptMarker is on the second line of every Startup script this build
// writes, so an earlier build's script can be told apart and replaced.
const autostartScriptMarker = "rem clawdh-autostart v2"

// isCurrentAutostartScript reports whether a Startup script is the kind this
// build writes.
func isCurrentAutostartScript(script string) bool {
	return strings.Contains(script, autostartScriptMarker)
}

// autostartScript is the batch file Windows runs at logon to start clawdh.
//
// It runs `clawdh start`, which launches the server with no console window and
// returns at once, so this script's own window is gone in an instant and
// nothing stays on screen. It used to be `start "" /min clawdh serve`: that
// put the server in a console window of its own for its whole life — a
// minimized terminal in the taskbar after every login, and one more thing a
// careless click could close, taking clawdh down with it.
//
// PATH is set first so clawdh can find `claude` (and the node it needs), which
// a logon-launched process would not otherwise have; the server inherits it
// from `clawdh start`. cmd.exe expands %VAR% as it parses the line, so a PATH
// containing a literal %SOMETHING% (common on Windows) would be silently
// mangled unless the percent signs are doubled.
func autostartScript(binaryPath string, port int) string {
	// Every interpolated value goes through batchEscape, not just PATH:
	// the home directory and the install path both sit under
	// C:\Users\<account name>, and % is a legal character in a Windows
	// account name. Left bare, cmd.exe would eat it at parse time and
	// silently start clawdh from the wrong directory — or not at all.
	return fmt.Sprintf("@echo off\r\n%s\r\nset \"PATH=%s\"\r\ncd /d \"%s\"\r\n\"%s\" start --port %d --no-wait >nul 2>&1\r\n",
		autostartScriptMarker, batchEscape(servicePATH()), batchEscape(serviceWorkingDir()), batchEscape(binaryPath), port)
}

// batchEscape makes a value safe to embed in a .cmd line: cmd.exe
// expands %VAR% at parse time, and a quote would terminate the set
// statement early.
func batchEscape(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	return strings.ReplaceAll(s, "%", "%%")
}
