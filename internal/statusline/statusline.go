// Package statusline puts a clawdh badge on Claude Code's status line —
// `clawdh main@7b506ea · HassanDH` — so a supervised session says which
// account it runs as and which clawdh build supervises it, in the one place
// Claude Code shows things about the running session.
//
// Claude Code has a single status-line command (settings.json `statusLine`),
// and many people already have one. clawdh does not replace it: it wraps it.
// The setting is pointed at `clawdh statusline`, the person's own command is
// saved beside clawdh's other state, and on every redraw clawdh runs their
// command with the same stdin Claude Code gave it and prints the badge in front
// of whatever they printed. A session clawdh did not start shows their line
// alone — the badge only exists where a supervisor exported who is running.
package statusline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// VersionEnvVar and AccountEnvVar are what a supervisor exports into the
// session so the badge knows the build that launched it and the account it
// runs as. Absent in a session nobody supervises, and then there is no badge.
const (
	VersionEnvVar = "CLAWDH_VERSION"
	AccountEnvVar = "CLAWDH_ACCOUNT"
)

// Command is the status-line command Claude Code runs, with the clawdh binary
// double-quoted so a path with spaces survives.
func Command(clawdhBinary string) string {
	return `"` + clawdhBinary + `" statusline`
}

// ours matches the command Command produces and nothing else: a person's own
// script that happens to take a "statusline" argument is not clawdh's.
var ours = regexp.MustCompile(`^"([^"]+)" statusline$`)

// isOurs reports whether a status-line command is clawdh's, and the binary
// it names.
func isOurs(command string) (string, bool) {
	m := ours.FindStringSubmatch(strings.TrimSpace(command))
	if m == nil {
		return "", false
	}
	// The last path element, whichever separator the path uses — a settings
	// file written on Windows is read as it is.
	base := m[1][strings.LastIndexAny(m[1], `/\`)+1:]
	if !strings.HasPrefix(base, "clawdh") {
		return "", false
	}
	return m[1], true
}

// saved is the file the person's own status line is kept in while clawdh's
// wraps it: their whole statusLine object, or null when they had none.
type saved struct {
	StatusLine map[string]any `json:"statusLine"`
}

// Ensure points Claude Code's status line at clawdh's, keeping the person's own
// one to run underneath it. It is idempotent and, in the steady state, writes
// nothing:
//   - clawdh's command already there, naming this binary: nothing is touched;
//   - clawdh's command naming an old binary path: the command is refreshed and
//     the saved status line left as it is;
//   - anything else — their own command, or none — is saved to savePath and
//     wrapped. Their padding is kept, since it is about their layout.
//
// A missing settings.json is created; an unparseable one is left alone and the
// error returned, never clobbered.
func Ensure(settingsPath, savePath, clawdhBinary string) error {
	cfg := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	current, _ := cfg["statusLine"].(map[string]any)
	command, _ := current["command"].(string)
	entry := map[string]any{"type": "command", "command": Command(clawdhBinary)}
	if _, ok := isOurs(command); ok {
		if command == Command(clawdhBinary) {
			return nil
		}
		if p, ok := current["padding"]; ok {
			entry["padding"] = p
		}
	} else {
		if err := writeJSON(savePath, saved{StatusLine: current}); err != nil {
			return err
		}
		if p, ok := current["padding"]; ok {
			entry["padding"] = p
		}
	}
	cfg["statusLine"] = entry
	return writeJSON(settingsPath, cfg)
}

// Restore puts the person's own status line back and forgets it, for `clawdh
// uninstall`. Only a statusLine that is clawdh's is replaced: one they changed
// since is theirs and stays. A missing or unparseable settings.json is left
// alone.
func Restore(settingsPath, savePath string) error {
	defer os.Remove(savePath)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	current, _ := cfg["statusLine"].(map[string]any)
	command, _ := current["command"].(string)
	if _, ok := isOurs(command); !ok {
		return nil
	}
	theirs, ok := Saved(savePath)
	if ok && theirs != nil {
		cfg["statusLine"] = theirs
	} else {
		delete(cfg, "statusLine")
	}
	return writeJSON(settingsPath, cfg)
}

// Saved is the person's own status line as Ensure kept it: nil when they had
// none, and false when nothing was ever saved.
func Saved(savePath string) (map[string]any, bool) {
	data, err := os.ReadFile(savePath)
	if err != nil {
		return nil, false
	}
	var s saved
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, false
	}
	return s.StatusLine, true
}

// Badge is what clawdh adds to the line: the build that launched this session
// and the account it runs as, from the environment a supervisor exported.
// installed is the build of the clawdh binary answering now; when it differs
// from the one that launched the session, an update has landed underneath the
// running supervisor and takes effect on the next session, so the badge says
// so. "" outside a supervised session.
func Badge(env func(string) string, installed string) string {
	version, account := env(VersionEnvVar), env(AccountEnvVar)
	if version == "" && account == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("clawdh")
	if version != "" {
		b.WriteString(" " + version)
		if installed != "" && installed != version {
			b.WriteString(" (update ready)")
		}
	}
	if account != "" {
		b.WriteString(" · " + account)
	}
	return b.String()
}

// runTimeout bounds the person's own status-line command. Claude Code redraws
// the line often and gives up on a slow command itself; this only keeps a hung
// one from hanging the badge with it.
const runTimeout = 5 * time.Second

// Render is one redraw: the person's own status line, run with the stdin Claude
// Code passed (the session's JSON), with the badge in front of its first line.
// Their command's failure is not the badge's: a command that errors or prints
// nothing leaves the badge on its own.
func Render(theirs map[string]any, stdin []byte, badge string) string {
	output := runTheirs(theirs, stdin)
	if badge == "" {
		return output
	}
	if strings.TrimSpace(output) == "" {
		return badge
	}
	first, rest, multi := strings.Cut(output, "\n")
	line := badge + " · " + first
	if multi {
		return line + "\n" + rest
	}
	return line
}

// runTheirs runs the saved status-line command through the shell, as Claude
// Code would have, and returns what it printed.
func runTheirs(theirs map[string]any, stdin []byte) string {
	command, _ := theirs["command"].(string)
	if strings.TrimSpace(command) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// writeJSON writes v as indented JSON, atomically, creating the directory.
func writeJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".clawdh-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
