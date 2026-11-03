// Package termlauncher opens a new, visible terminal window already
// scoped (via CLAUDE_SECURESTORAGE_CONFIG_DIR) to one account, for
// people who'd rather click a button than remember/copy a shell alias.
// The scoping covers the login only; the window shares the user's
// ~/.claude like any other session.
package termlauncher

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Launch opens a new terminal window running `claude` for one account.
//
// An empty configDir means the default account, which is reached by
// running plain `claude` with no CLAUDE_CONFIG_DIR at all — setting it
// to the default path is a different identity as far as the CLI is
// concerned.
func Launch(configDir, label string) error {
	switch runtime.GOOS {
	case "darwin":
		return launchDarwin(configDir, label)
	case "windows":
		return launchWindows(configDir, label)
	default:
		return launchLinux(configDir, label)
	}
}

func launchDarwin(configDir, label string) error {
	// Two layers of quoting, both needed: shellQuote so the path
	// survives as one word in the shell command Terminal.app runs
	// (home directories contain spaces), then appleScriptQuote so the
	// whole thing survives as one AppleScript string literal.
	script := fmt.Sprintf(
		`tell application "Terminal" to do script "%s; exec $SHELL" activate`,
		appleScriptQuote(unixClaudeCommand(configDir)),
	)
	_ = label // Terminal.app tab titles aren't reliably settable from here.
	return startAndReap(exec.Command("osascript", "-e", script))
}

// startAndReap starts cmd and gives it a moment to fail.
//
// cmd.Start() succeeding only means fork+exec happened — `osascript`
// denied by macOS's automation permissions, or a terminal emulator with
// no DISPLAY to open on, both exit non-zero a moment later, and
// reporting success for those told the user a window had opened when
// none had. It also reaps the child either way: clawdh is long-lived, so
// an un-waited child would linger as a zombie for the life of the
// service.
func startAndReap(cmd *exec.Cmd) error {
	// Capped: cmd.Wait() keeps draining stderr for as long as the
	// terminal window is open (hours), and an uncapped buffer would
	// accumulate everything it ever printed in clawdh's heap.
	stderr := &cappedBuffer{limit: 4 << 10}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("%s: %s", err, msg)
			}
			return err
		}
		return nil
	case <-time.After(1500 * time.Millisecond):
		// Still running after a moment: a terminal window that stays
		// open is the normal, successful case.
		go func() { <-done }()
		return nil
	}
}

// cappedBuffer keeps at most limit bytes and silently drops the rest.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return len(p), nil // report full consumption so the child never blocks
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// unixClaudeCommand is the shell command that runs claude for one
// account: scoped with env for a managed account, bare for the default.
//
// Only the credential store is scoped. CLAUDE_CONFIG_DIR is actively
// unset rather than merely left alone, so an inherited value cannot
// silently keep the session isolated from the shared ~/.claude.
func unixClaudeCommand(configDir string) string {
	if configDir == "" {
		return "claude"
	}
	return fmt.Sprintf("env -u CLAUDE_CONFIG_DIR CLAUDE_SECURESTORAGE_CONFIG_DIR=%s claude", shellQuote(configDir))
}

func appleScriptQuote(s string) string {
	out := ""
	for _, r := range s {
		switch r {
		case '\\', '"':
			out += `\` + string(r)
		default:
			out += string(r)
		}
	}
	return out
}

// linuxTerminals is tried in order; the first one found on PATH is used.
// Each entry's args open a new window running shellCmd.
var linuxTerminals = []struct {
	bin  string
	args func(shellCmd string) []string
}{
	{"x-terminal-emulator", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	{"gnome-terminal", func(c string) []string { return []string{"--", "sh", "-c", c} }},
	{"konsole", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	{"xfce4-terminal", func(c string) []string { return []string{"-e", c} }},
	{"xterm", func(c string) []string { return []string{"-e", c} }},
}

func launchLinux(configDir, label string) error {
	_ = label
	shellCmd := unixClaudeCommand(configDir) + "; exec $SHELL"
	var lastErr error
	for _, t := range linuxTerminals {
		if _, err := exec.LookPath(t.bin); err != nil {
			continue
		}
		cmd := exec.Command(t.bin, t.args(shellCmd)...)
		if err := startAndReap(cmd); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("no working terminal emulator found: %w", lastErr)
	}
	return fmt.Errorf("no terminal emulator found on PATH (tried x-terminal-emulator, gnome-terminal, konsole, xfce4-terminal, xterm)")
}

func shellQuote(s string) string {
	return "'" + replaceAll(s, "'", `'\''`) + "'"
}

func replaceAll(s, old, new string) string {
	out := ""
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			out += new
			i += len(old)
		} else {
			out += string(s[i])
			i++
		}
	}
	return out
}

func launchWindows(configDir, label string) error {
	_ = label
	psCmd := "claude"
	if configDir != "" {
		// Remove-Item rather than assigning $null or '': the CLI branches
		// on whether the name is present, so a variable left
		// defined-but-empty is not the same as an absent one.
		psCmd = fmt.Sprintf(
			`Remove-Item Env:CLAUDE_CONFIG_DIR -ErrorAction SilentlyContinue; `+
				`$env:CLAUDE_SECURESTORAGE_CONFIG_DIR='%s'; claude`, psQuote(configDir))
	}

	if wt, err := exec.LookPath("wt.exe"); err == nil {
		cmd := exec.Command(wt, "powershell", "-NoExit", "-Command", psCmd)
		if err := startAndReap(cmd); err == nil {
			return nil
		}
	}

	ps := exec.Command("powershell", "-NoExit", "-Command", psCmd)
	ownWindow(ps)
	return startAndReap(ps)
}

func psQuote(s string) string {
	return replaceAll(s, "'", "''")
}
