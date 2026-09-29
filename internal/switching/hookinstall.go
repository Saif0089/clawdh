package switching

import (
	"encoding/json"
	"os"
	"strings"
)

// clawdh runs one hook per Claude Code event it needs. Each is told apart from
// the user's own hooks by the tail of its command line plus the binary's name,
// so it is found wherever the clawdh binary lives, and a hook of the user's
// that merely ends in the same words is never mistaken for it.
type hookSpec struct {
	event     string
	signature string // the command-line tail, e.g. "hook stop"
	timeout   int    // seconds
}

var clawdhHooks = []hookSpec{
	// A typed `clawdh <name>` switches the session now, or sets a rule for later.
	{"UserPromptSubmit", hookSignature, 5},
	// An answer has finished: the one moment a pending rule may move the session.
	// It may read usage from the local service, so it gets a little longer.
	{"Stop", "hook stop", 10},
	// Tells Claude, in a clawdh session only, how to set such a rule when asked.
	{"SessionStart", "hook session-start", 5},
}

// hookSignature identifies clawdh's UserPromptSubmit hook.
const hookSignature = "hook user-prompt-submit"

// HookCommand is the command string Claude Code runs for the switch hook,
// with the clawdh binary path double-quoted so a path with spaces survives.
func HookCommand(clawdhBinary string) string { return hookCommand(clawdhBinary, hookSignature) }

func hookCommand(clawdhBinary, signature string) string {
	return `"` + clawdhBinary + `" ` + signature
}

// isClawdhHook reports whether command is clawdh's hook with this signature.
func isClawdhHook(command, signature string) bool {
	command = strings.TrimSpace(command)
	return strings.HasSuffix(command, " "+signature) && strings.Contains(strings.ToLower(command), "clawdh")
}

// EnsureHooks makes sure the user's shared settings.json runs every clawdh
// hook, WITHOUT disturbing any other hooks they have (the usage-monitor and
// rule-reminder hooks live under the same events). It is idempotent and, in
// the steady state, writes nothing:
//   - a clawdh hook already present with the right command is left alone, so a
//     normal launch never reformats or churns settings.json;
//   - one that is missing, or points at an old binary path, is added or
//     refreshed, preserving every other key and hook.
//
// A missing settings.json is created; an unparseable one is left alone (an
// error is returned) rather than clobbered.
func EnsureHooks(settingsPath, clawdhBinary string) error {
	cfg := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, spec := range clawdhHooks {
		if ensureHook(hooks, spec, hookCommand(clawdhBinary, spec.signature)) {
			changed = true
		}
	}
	if !changed {
		return nil // already correct: no rewrite, no reformat
	}
	cfg["hooks"] = hooks
	return writeSettings(settingsPath, cfg)
}

// ensureHook puts one clawdh hook under its event, replacing a stale clawdh
// entry in place or appending, and reports whether anything changed.
func ensureHook(hooks map[string]any, spec hookSpec, command string) bool {
	entries, _ := hooks[spec.event].([]any)
	for _, e := range entries {
		if cmd, ok := entryCommand(e); ok && cmd == command {
			return false
		}
	}
	ours := map[string]any{
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": command,
			"timeout": spec.timeout,
		}},
	}
	for i, e := range entries {
		if cmd, ok := entryCommand(e); ok && isClawdhHook(cmd, spec.signature) {
			entries[i] = ours
			hooks[spec.event] = entries
			return true
		}
	}
	hooks[spec.event] = append(entries, ours)
	return true
}

// RemoveHooks deletes every clawdh hook from settings.json, leaving every other
// hook untouched. Used by `clawdh uninstall`. A missing or unparseable file, or
// absent hooks, is a no-op.
func RemoveHooks(settingsPath string) error {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil // don't touch a file we can't parse
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	removed := false
	for _, spec := range clawdhHooks {
		entries, _ := hooks[spec.event].([]any)
		kept := entries[:0:0]
		for _, e := range entries {
			if cmd, ok := entryCommand(e); ok && isClawdhHook(cmd, spec.signature) {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			if _, had := hooks[spec.event]; had && len(entries) > 0 {
				delete(hooks, spec.event)
			}
		} else {
			hooks[spec.event] = kept
		}
	}
	if !removed {
		return nil
	}
	return writeSettings(settingsPath, cfg)
}

func writeSettings(settingsPath string, cfg map[string]any) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := settingsPath + ".clawdh-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath)
}

// entryCommand pulls the command string out of one hook entry
// ({"hooks":[{"type":"command","command":"…"}]}), if it has one.
func entryCommand(entry any) (string, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return "", false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return "", false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok {
			return cmd, true
		}
	}
	return "", false
}
