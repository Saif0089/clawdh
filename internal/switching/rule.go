package switching

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// A Rule is a switch the session makes later: once the account it runs as has
// used AtPercent of Window, it moves to Account — between answers, never in the
// middle of one. Claude Code's Stop hook fires when an answer has finished, and
// that is the only place a rule is acted on (see `clawdh hook stop`), so the
// relaunch never cuts anything off; the conversation carries over exactly as it
// does for a switch typed by hand.
//
// A rule lives beside the handoff file of the supervisor it was set in, and is
// one-shot: it is removed as it fires, and when its supervisor exits.
type Rule struct {
	Account   string  `json:"account"`
	Shared    bool    `json:"shared,omitempty"`
	Window    string  `json:"window"` // WindowWeek or Window5h
	AtPercent float64 `json:"atPercent"`
	// For is the nonce of the supervisor the rule was set in. A rule outliving
	// its supervisor must not fire in an unrelated session that inherited the
	// pid, and with it the handoff path.
	For   string    `json:"for,omitempty"`
	SetAt time.Time `json:"setAt"`
}

// The windows a rule can watch: the plan's weekly allowance, and the rolling
// five-hour session allowance.
const (
	WindowWeek = "week"
	Window5h   = "5h"
)

// RulePath is where the rule for the supervisor behind handoffPath lives.
func RulePath(handoffPath string) string { return handoffPath + ".when" }

// WriteRule atomically records a rule, replacing any earlier one: a session
// has at most one pending move.
func WriteRule(handoffPath string, r Rule) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := RulePath(handoffPath) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, RulePath(handoffPath))
}

// ReadRule returns the pending rule, if there is one.
func ReadRule(handoffPath string) (Rule, bool) {
	data, err := os.ReadFile(RulePath(handoffPath))
	if err != nil {
		return Rule{}, false
	}
	var r Rule
	if err := json.Unmarshal(data, &r); err != nil || r.Account == "" {
		return Rule{}, false
	}
	return r, true
}

// ClearRule removes the pending rule. A missing file is not an error.
func ClearRule(handoffPath string) {
	_ = os.Remove(RulePath(handoffPath))
}

// Describe says what the rule will do, e.g. "move to saif at 5% of the week".
func (r Rule) Describe() string {
	window := "the week"
	if r.Window == Window5h {
		window = "the 5-hour window"
	}
	if r.AtPercent <= 0 {
		return "move to " + r.Account + " once this answer finishes"
	}
	return fmt.Sprintf("move to %s at %s%% of %s", r.Account, trimPercent(r.AtPercent), window)
}

// Badge is the rule in a few characters, for the status line: "→ saif 5%wk".
func (r Rule) Badge() string {
	if r.AtPercent <= 0 {
		return "→ " + r.Account + " next"
	}
	window := "wk"
	if r.Window == Window5h {
		window = "5h"
	}
	return "→ " + r.Account + " " + trimPercent(r.AtPercent) + "%" + window
}

func trimPercent(p float64) string {
	return strconv.FormatFloat(p, 'f', -1, 64)
}

// ParseRuleArgs reads what follows an account name when a rule is being set:
//
//	at 5%          -> the week, 5
//	at 5% 5h       -> the five-hour window, 5
//	at 80 week     -> the week, 80 (the % sign is optional)
//	at 0%          -> as soon as the current answer finishes
//
// Anything else is not a rule.
func ParseRuleArgs(args []string) (window string, atPercent float64, ok bool) {
	if len(args) < 2 || len(args) > 3 || !strings.EqualFold(args[0], "at") {
		return "", 0, false
	}
	p, err := strconv.ParseFloat(strings.TrimSuffix(args[1], "%"), 64)
	if err != nil || p < 0 || p > 100 {
		return "", 0, false
	}
	window = WindowWeek
	if len(args) == 3 {
		switch strings.ToLower(args[2]) {
		case "week", "weekly", "wk", "7d":
			window = WindowWeek
		case "5h", "session", "5-hour":
			window = Window5h
		default:
			return "", 0, false
		}
	}
	return window, p, true
}

// ParseRuleTrigger reports whether a submitted prompt sets or clears a rule:
//
//	clawdh <name> at <N>% [week|5h]
//	clawdh switch <name> at <N>% [week|5h]
//	clawdh shared <name> at <N>% [week|5h]
//	clawdh stay                              -> clear
//
// Like ParseTrigger, anything else passes through to the model untouched.
func ParseRuleTrigger(prompt string) (r Rule, clear bool, ok bool) {
	fields := strings.Fields(strings.TrimSpace(prompt))
	if len(fields) == 2 && fields[0] == "clawdh" && fields[1] == "stay" {
		return Rule{}, true, true
	}
	if len(fields) < 4 || fields[0] != "clawdh" {
		return Rule{}, false, false
	}
	rest := fields[1:]
	shared := false
	switch rest[0] {
	case "shared":
		shared, rest = true, rest[1:]
	case "switch":
		rest = rest[1:]
	}
	if len(rest) < 3 {
		return Rule{}, false, false
	}
	window, p, ok := ParseRuleArgs(rest[1:])
	if !ok {
		return Rule{}, false, false
	}
	return Rule{Account: rest[0], Shared: shared, Window: window, AtPercent: p}, false, true
}
