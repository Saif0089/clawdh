//go:build darwin

package notify

import (
	"strings"
)

// notifyCommand uses osascript, which is part of macOS itself. The
// alternative everyone reaches for, terminal-notifier, is a Homebrew
// install clawdh has no business requiring.
func notifyCommand(body string) (string, []string, error) {
	script := "display notification " + appleScriptString(body) +
		" with title " + appleScriptString(title)
	return "/usr/bin/osascript", []string{"-e", script}, nil
}

// appleScriptString quotes text for AppleScript, where the only escapes
// inside a double-quoted string are \" and \\.
func appleScriptString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
