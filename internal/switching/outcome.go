package switching

import (
	"encoding/json"
	"os"
	"time"
)

// An Outcome is what the supervisor actually did with a staged handoff, sent
// back to the hook that staged it.
//
// It exists because of where the two live. The supervisor runs Claude Code with
// stdio inherited — Claude Code owns the terminal — so anything the supervisor
// prints while a session is up is injected into the screen the TUI is painting,
// behind its back. The display is then wrong until something forces a full
// repaint, which is what toggling /tui does and why it appeared to fix it.
//
// The hook is on the right side of that boundary: Claude Code renders what a
// hook returns. So the supervisor writes the result here and the hook, which is
// still waiting, says it. Nothing is printed to the terminal at all.
type Outcome struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// outcomePath is where the result for a given handoff file goes. Deriving it
// from the handoff path keeps it per-supervisor, like the handoff itself.
func outcomePath(handoffPath string) string { return handoffPath + ".result" }

// WriteOutcome records what happened, for the hook that is waiting.
func WriteOutcome(handoffPath string, o Outcome) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp := outcomePath(handoffPath) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, outcomePath(handoffPath))
}

// ClearOutcome removes any previous result, so a hook cannot read the answer to
// an earlier switch as if it were the answer to this one.
func ClearOutcome(handoffPath string) {
	os.Remove(outcomePath(handoffPath))
	os.Remove(outcomePath(handoffPath) + ".tmp")
}

// OutcomePending reports whether a written outcome is still waiting to be read.
// AwaitOutcome removes it on pickup, so this going false is how the supervisor
// knows the hook has the answer and Claude Code is about to show it.
func OutcomePending(handoffPath string) bool {
	_, err := os.Stat(outcomePath(handoffPath))
	return err == nil
}

// AwaitOutcome waits for the supervisor to report, and says whether it did.
//
// A timeout is not a failure: the supervisor may have decided it cannot switch
// in place and be relaunching the session, which tears the screen down and
// rebuilds it anyway. The caller falls back to saying what it asked for rather
// than claiming an outcome it does not have.
func AwaitOutcome(handoffPath string, timeout, poll time.Duration) (Outcome, bool) {
	if poll <= 0 {
		poll = 25 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(outcomePath(handoffPath)); err == nil {
			var o Outcome
			if json.Unmarshal(data, &o) == nil {
				ClearOutcome(handoffPath)
				return o, true
			}
		}
		if time.Now().After(deadline) {
			return Outcome{}, false
		}
		time.Sleep(poll)
	}
}
