// Package switching implements in-session account switching: the user types
// `clawdh <name>` at the Claude prompt, a UserPromptSubmit hook intercepts it
// and records a handoff, and the `clawdh run` supervisor relaunches Claude Code
// as the named account with the conversation resumed. Nothing here spawns a
// process; it is the pure trigger/handoff/decision logic the two CLI commands
// share, so it can be tested on its own.
package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"clawdh/internal/accounts"
)

// HandoffEnvVar names the file the hook writes and the supervisor reads. The
// supervisor picks the path and exports it into Claude Code's environment, so
// the hook (a child of claude) finds it without guessing.
const HandoffEnvVar = "CLAWDH_HANDOFF"

// SupervisorEnvVar carries the `clawdh run` supervisor's pid into the session it
// runs, so a switch staged from a shell command can tell a live supervisor
// from an inherited environment variable left over by one that has exited.
const SupervisorEnvVar = "CLAWDH_SUPERVISOR"

// SessionIDEnvVar is the session Claude Code exports into every process it
// spawns — hooks and the shell commands a user runs with `!`. It is how a
// switch staged from a shell command knows which conversation to carry over,
// since only the hook payload carries it otherwise.
const SessionIDEnvVar = "CLAUDE_CODE_SESSION_ID"

// Handoff is a pending switch: the account the user asked for and the session
// to resume as it. Shared marks the target as a gateway-shared account (resolved
// from the shares cache) rather than a local account, so `clawdh shared <slug>`
// can switch a running session the same way `clawdh <account>` does.
type Handoff struct {
	Account   string `json:"account"`
	SessionID string `json:"sessionId"`
	Shared    bool   `json:"shared,omitempty"`
	// For is the nonce of the supervisor this switch was aimed at, set by a
	// writer that picked its target out of the session register rather than by
	// being inside the session — the local page, in practice. A supervisor
	// ignores a handoff stamped for someone else, so a switch aimed at a
	// session that ended before it was read cannot be acted on by an unrelated
	// supervisor that inherited its pid. Empty from every in-session writer,
	// which is already talking to its own supervisor and needs no stamp.
	For string `json:"for,omitempty"`
}

// ParseTrigger reports whether a submitted prompt is a switch command and, if
// so, the account name in it and whether it names a gateway-shared account.
// Accepted forms, whitespace-trimmed:
//
//	clawdh <name>          -> (name, shared=false)
//	clawdh switch <name>   -> (name, shared=false)
//	clawdh shared <name>   -> (name, shared=true)
//
// Anything else — extra words, a leading slash (Claude Code routes "/…" to
// command resolution before the hook ever runs), a sentence that merely starts
// with "clawdh" — is not a trigger and passes through to the model untouched.
func ParseTrigger(prompt string) (name string, shared bool, ok bool) {
	fields := strings.Fields(strings.TrimSpace(prompt))
	switch {
	case len(fields) == 2 && fields[0] == "clawdh":
		return fields[1], false, true
	case len(fields) == 3 && fields[0] == "clawdh" && fields[1] == "switch":
		return fields[2], false, true
	case len(fields) == 3 && fields[0] == "clawdh" && fields[1] == "shared":
		return fields[2], true, true
	default:
		return "", false, false
	}
}

// ResolveAccount finds the account a typed name refers to, matching (case-
// insensitively) its slug, id, alias, or the alias with the "claude-" prefix
// stripped — so `clawdh ehti`, `clawdh claude-ehti`, and `clawdh default` all work.
// Returns the account and true, or false if nothing matches.
func ResolveAccount(list []accounts.Account, name string) (accounts.Account, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, a := range list {
		candidates := []string{a.Slug, a.ID, a.Alias, strings.TrimPrefix(a.Alias, "claude-")}
		for _, c := range candidates {
			if c != "" && strings.EqualFold(c, n) {
				return a, true
			}
		}
	}
	return accounts.Account{}, false
}

// ResumeArgs are the Claude Code flags that carry the conversation across a
// switch, given the staged session id and whether that session has anything
// recorded (see HasTranscript). The three cases are genuinely different:
//
// The session is resumed, not forked. Forking was a holdover from when a switch
// was applied in place: it made a second conversation out of every switch, so
// /resume filled up with near-duplicates, and the fork's new id is minted by
// Claude Code, which means clawdh never learns it and cannot record who owns it.
// Resuming keeps one conversation with one id, and the usage monitor resolves
// ownership by interval — the last ledger entry at or before a line's timestamp
// — so the same id being account A's before the switch and account B's after is
// exactly what that ledger is built to express.
//
//   - a recorded session is resumed exactly as it was, keeping its id;
//   - a session id with nothing recorded is one switched before its first
//     message. There is no conversation to carry, and --resume on it makes
//     Claude Code exit with "No conversation found with session ID", taking
//     the terminal down with it — so start clean, but under the same id: the
//     usage ledger already attributes that id, and an editor that launched the
//     session by id is still calling it that;
//   - no session id at all (a Claude Code that does not export it) falls back
//     to --continue, the most recent conversation in this directory, which is
//     the one just terminated.
func ResumeArgs(sessionID string, recorded bool) []string {
	switch {
	case strings.TrimSpace(sessionID) == "":
		return []string{"--continue"}
	case recorded:
		return []string{"--resume", sessionID}
	default:
		return []string{"--session-id", sessionID}
	}
}

// WithoutSessionArgs drops the flags that name a conversation — --session-id,
// --resume/-r, --continue/-c, --fork-session, in both `--flag value` and
// `--flag=value` forms — so a relaunch can name the one it is carrying over
// without contradicting the launch's own. An editor launches every session as
// `--session-id=<id>`; relaunching with that still present beside --resume is
// two answers to the same question.
func WithoutSessionArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session-id" || a == "--resume" || a == "-r":
			i++ // and its value
		case a == "--continue" || a == "-c" || a == "--fork-session":
		case strings.HasPrefix(a, "--session-id=") || strings.HasPrefix(a, "--resume="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// HasTranscript reports whether sessionID has a conversation recorded under
// claudeDir. Claude Code stores each one as <session id>.jsonl in a per-working-
// directory folder under projects/, so a glob answers this without reproducing
// how it slugifies a path — and a session that has not been written yet simply
// matches nothing.
func HasTranscript(claudeDir, sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(claudeDir, "projects", "*", sessionID+".jsonl"))
	return err == nil && len(matches) > 0
}

// WriteHandoff atomically writes a pending switch to path.
func WriteHandoff(path string, h Handoff) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadHandoff returns the pending switch at path, or ok=false if there is none
// (the common case — the file only exists in the instant between the hook
// writing it and the supervisor consuming it).
func ReadHandoff(path string) (Handoff, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Handoff{}, false
	}
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil || h.Account == "" {
		return Handoff{}, false
	}
	return h, true
}

// ClearHandoff removes a consumed handoff. A missing file is not an error.
func ClearHandoff(path string) {
	_ = os.Remove(path)
}

// blockDecision is the UserPromptSubmit hook output that stops the typed
// trigger from reaching the model and omits it from the transcript. The field
// names and shape are Claude Code's contract for this event.
type blockDecision struct {
	Decision           string             `json:"decision"`
	Reason             string             `json:"reason"`
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName          string `json:"hookEventName"`
	SuppressOriginalPrompt bool   `json:"suppressOriginalPrompt"`
}

// BlockDecisionJSON is what the hook prints to intercept a switch trigger:
// decision "block" (the prompt is never sent to the model) with
// suppressOriginalPrompt (the trigger word is not echoed into the
// conversation).
func BlockDecisionJSON(reason string) ([]byte, error) {
	return json.Marshal(blockDecision{
		Decision: "block",
		Reason:   reason,
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:          "UserPromptSubmit",
			SuppressOriginalPrompt: true,
		},
	})
}
