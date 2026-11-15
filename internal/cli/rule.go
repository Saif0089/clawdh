package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"clawdh/internal/config"
	"clawdh/internal/sessions"
	"clawdh/internal/statusline"
	"clawdh/internal/switching"
	"clawdh/internal/usage"
	"clawdh/panel"
)

// Moving a session later: "when you reach 5% of the week, move to saif".
//
// A switch typed by hand happens now; a rule happens once the account the
// session runs as has used enough of a window, and only when an answer has
// finished — Claude Code's Stop hook — so the relaunch never cuts anything off
// and the conversation carries over exactly as it does for a hand-typed switch.
//
// A rule is set from inside the session, three ways that all end up here:
//   - typed as the prompt: `clawdh shared saif at 5%` (the UserPromptSubmit hook);
//   - run as a command:    `!clawdh shared saif at 5%`, or by Claude when asked
//     in plain words — the SessionStart note tells it how;
//   - cancelled with `clawdh stay`.

// sharedReadingMaxAge is how old a shared account's reading may be and still be
// acted on. Shared readings arrive with the panel check-in; a rule never fires
// on numbers from before an outage.
const sharedReadingMaxAge = 15 * time.Minute

// thisSession is the register entry of the supervisor this process runs under:
// the account the session is on, and the nonce its rules are stamped with.
func thisSession() (sessions.Session, bool) {
	pid, err := strconv.Atoi(os.Getenv(switching.SupervisorEnvVar))
	if err != nil || pid <= 0 || !supervisorAlive() {
		return sessions.Session{}, false
	}
	dir, err := config.SessionsDir()
	if err != nil {
		return sessions.Session{}, false
	}
	return sessions.Get(dir, pid)
}

// setRule records a move for later in the session this runs under, and returns
// what to tell the person — or Claude, when it ran the command — or the reason
// it cannot.
func setRule(name string, shared bool, window string, atPercent float64) (message, problem string) {
	if window == switching.Window5h {
		return "", fiveHourGone + " Use `clawdh " + name + " at <N>%` to move at a share of the week, or `at 0%` to move after this answer."
	}
	handoff := os.Getenv(switching.HandoffEnvVar)
	sess, ok := thisSession()
	if handoff == "" || !ok {
		return "", "Only a session started with clawdh can move to another account later. Start one with `clawdh <account>` (or `clawdh shared <name>`), then set the move inside it."
	}
	if os.Getenv(switching.RulesEnvVar) == "" {
		restart := "clawdh " + sess.Slug + " --continue"
		if sess.Shared {
			restart = "clawdh shared " + sess.Slug + " --continue"
		}
		return "", "This session was started by an older clawdh, which cannot make the move after an answer — it would show as pending and never happen. " +
			"Restart the session with `" + restart + "` (the conversation carries over), then set the move there."
	}
	slug, label, problem := resolveSwitchTarget(name, shared)
	if problem != "" {
		return "", problem
	}
	if strings.EqualFold(slug, sess.Slug) && shared == sess.Shared {
		return "", fmt.Sprintf("This session already runs as %s.", label)
	}
	r := switching.Rule{Account: slug, Shared: shared, Window: window, AtPercent: atPercent, For: sess.Nonce, SetAt: time.Now()}
	if err := switching.WriteRule(handoff, r); err != nil {
		return "", "clawdh could not record the move: " + err.Error()
	}
	when := "once " + sess.Account + " has used " + strconv.FormatFloat(atPercent, 'f', -1, 64) + "% of its week"
	if atPercent <= 0 {
		when = "the next time an answer finishes"
	}
	return fmt.Sprintf("clawdh will move this session to %s %s — between answers, so nothing is cut off, and the conversation carries over. `clawdh stay` cancels it.", label, when), ""
}

// fiveHourGone is what anyone asking for the five-hour window is told.
const fiveHourGone = "Anthropic no longer has a five-hour usage limit, so there is nothing to wait for."

// setRuleCommand is `clawdh [shared] <name> at <N>% [week]` run as a
// command inside a session.
func setRuleCommand(name string, shared bool, window string, atPercent float64) int {
	message, problem := setRule(name, shared, window, atPercent)
	if problem != "" {
		fmt.Fprintln(os.Stderr, "clawdh:", problem)
		return 1
	}
	fmt.Println(message)
	return 0
}

// cmdStay cancels a pending move: `clawdh stay`.
func cmdStay(_ []string) int {
	handoff := os.Getenv(switching.HandoffEnvVar)
	if handoff == "" || !supervisorAlive() {
		fmt.Fprintln(os.Stderr, "clawdh: `clawdh stay` cancels a pending move in a session started with clawdh; this is not one.")
		return 1
	}
	fmt.Println(stayMessage(handoff))
	return 0
}

func stayMessage(handoff string) string {
	r, ok := switching.ReadRule(handoff)
	switching.ClearRule(handoff)
	if !ok {
		return "Nothing was pending; this session stays where it is."
	}
	return "Cancelled — this session will no longer " + r.Describe() + "."
}

// hookStop is Claude Code's Stop hook: an answer has just finished, the one
// moment a pending rule may move the session. Every other time — no rule, or
// not enough used yet — it returns at once; it runs after every answer in
// every session sharing this settings.json, plain `claude` included.
func hookStop() int {
	handoff := os.Getenv(switching.HandoffEnvVar)
	if handoff == "" {
		return 0
	}
	if _, ok := switching.ReadRule(handoff); !ok {
		return 0 // the overwhelmingly common case: one missing file
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if data, err := io.ReadAll(os.Stdin); err == nil {
		_ = json.Unmarshal(data, &in)
	}
	sess, ok := thisSession()
	if !ok {
		return 0
	}
	if message := applyRule(handoff, in.SessionID, sess, usagePercent); message != "" {
		stopMessage(message)
	}
	return 0
}

// applyRule acts on the pending rule once an answer has finished: it moves the
// session when its account has used enough, and returns what to tell the
// person ("" when there is nothing to say yet). read is where usage comes from.
func applyRule(handoff, sessionID string, sess sessions.Session, read func(sessions.Session, string) (float64, bool)) string {
	message, _ := moveByRule(handoff, sessionID, sess, read, "")
	return message
}

// applyTurnFailure is applyRule for a turn that ended in an API error. Claude
// Code runs StopFailure for it and never Stop, so without this a move waiting on
// an account that has hit its limit is never made and the session halts there.
// A rate_limit error is the account itself saying it is out, a better witness
// than a usage reading that may lag, so it moves the session whatever the
// threshold or the reading; any other failure is judged like a finished answer.
func applyTurnFailure(handoff, sessionID string, sess sessions.Session, kind string, read func(sessions.Session, string) (float64, bool)) string {
	why := ""
	if kind == "rate_limit" {
		why = "hit its usage limit"
	}
	message, _ := moveByRule(handoff, sessionID, sess, read, why)
	return message
}

// limitReachedPercent is the usage at which an account is taken to be out:
// readings are rounded and arrive late, and a weekly limit stops answering at
// 100%, so a few tenths short of it is the limit for every practical purpose.
const limitReachedPercent = 99

// ruleWatchInterval is how often the supervisor looks at a pending move.
var ruleWatchInterval = 30 * time.Second

// ruleUsage is where the watcher reads usage from; tests stand in for it.
var ruleUsage = usagePercent

// watchRuleOnce is the supervisor's own look at a pending move, for when no hook
// can run: an account at its limit stops answering, so neither Stop nor
// StopFailure may ever fire for the session sitting on it. It acts only when the
// account is at its limit and the move is due — a move merely due is for the
// Stop hook, between answers, so an answer in the middle of being written is not
// cut off for it. It reports whether it staged the move.
func watchRuleOnce(handoff, sessionID string, sess sessions.Session, read func(sessions.Session, string) (float64, bool)) bool {
	rule, ok := switching.ReadRule(handoff)
	if !ok || (rule.For != "" && rule.For != sess.Nonce) || rule.Window == switching.Window5h {
		return false
	}
	used, ok := read(sess, rule.Window)
	if !ok || used < limitReachedPercent || used < rule.AtPercent {
		return false
	}
	_, moved := moveByRule(handoff, sessionID, sess, read, "hit its usage limit")
	return moved
}

// watchRule runs watchRuleOnce on a timer for as long as one launch of the
// session lives. It says nothing: Claude Code owns the terminal, and a line
// written here would corrupt it. The relaunch's first prompt tells the model.
func watchRule(handoff, sessionID string, sess sessions.Session, every time.Duration, read func(sessions.Session, string) (float64, bool), stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if watchRuleOnce(handoff, sessionID, sess, read) {
				return
			}
		}
	}
}

// moveByRule is the decision itself. why is "" for the ordinary case — move once
// enough is used — and otherwise the reason the account is known to be out, which
// moves the session whatever the threshold. moved says whether a switch was
// staged; the message is what to tell the person ("" when there is nothing to say).
func moveByRule(handoff, sessionID string, sess sessions.Session, read func(sessions.Session, string) (float64, bool), why string) (message string, moved bool) {
	rule, ok := switching.ReadRule(handoff)
	if !ok {
		return "", false
	}
	if rule.For != "" && rule.For != sess.Nonce {
		switching.ClearRule(handoff) // left by an earlier session that had this pid
		return "", false
	}
	if rule.Window == switching.Window5h {
		// Set before the window was removed: it can never reach its percentage,
		// and reading it as the week instead could move the session on a number
		// nobody asked about.
		switching.ClearRule(handoff)
		return "clawdh: this session had a move waiting on the 5-hour window. " + fiveHourGone +
			" The move was cancelled; set a new one with `clawdh <account> at <N>%`.", false
	}
	if rule.AtPercent > 0 && why == "" {
		used, ok := read(sess, rule.Window)
		if !ok || used < rule.AtPercent {
			return "", false
		}
	}
	// Relaunching into a folder Claude Code cannot read would end the session.
	// Keep the rule and say why; it fires after a later answer once fixed.
	if problem := cwdProblem(); problem != "" {
		return "clawdh did not move this session yet: " + problem, false
	}
	if sessionID == "" {
		sessionID = os.Getenv(switching.SessionIDEnvVar)
	}
	switching.ClearRule(handoff)
	if err := switching.WriteHandoff(handoff, switching.Handoff{Account: rule.Account, Shared: rule.Shared, SessionID: sessionID}); err != nil {
		return "clawdh could not move this session to " + rule.Account + ": " + err.Error(), false
	}
	if why != "" {
		return fmt.Sprintf("clawdh: %s %s — moving this session to %s.", sess.Account, why, rule.Account), true
	}
	if rule.AtPercent > 0 {
		return fmt.Sprintf("clawdh: %s reached %s%% of its %s — moving this session to %s.",
			sess.Account, strconv.FormatFloat(rule.AtPercent, 'f', -1, 64), "week", rule.Account), true
	}
	return "clawdh: moving this session to " + rule.Account + ".", true
}

// hookStopFailure is Claude Code's StopFailure hook: the turn ended in an API
// error, which runs this instead of Stop. Like Stop it returns at once unless a
// move is pending. Claude Code ignores what a StopFailure hook prints, so there
// is nothing to say here; the relaunch's first prompt carries the news.
func hookStopFailure() int {
	handoff := os.Getenv(switching.HandoffEnvVar)
	if handoff == "" {
		return 0
	}
	if _, ok := switching.ReadRule(handoff); !ok {
		return 0
	}
	sessionID, kind := readStopFailure(os.Stdin)
	sess, ok := thisSession()
	if !ok {
		return 0
	}
	applyTurnFailure(handoff, sessionID, sess, kind, usagePercent)
	return 0
}

// readStopFailure reads the StopFailure hook's payload: the session, and the
// kind of error that ended the turn ("rate_limit", "overloaded", …).
func readStopFailure(r io.Reader) (sessionID, kind string) {
	var in struct {
		SessionID string `json:"session_id"`
		Error     string `json:"error"`
	}
	if data, err := io.ReadAll(r); err == nil && json.Unmarshal(data, &in) == nil {
		return in.SessionID, in.Error
	}
	return "", ""
}

// stopMessage shows the person a line from the Stop hook.
func stopMessage(text string) {
	out, _ := json.Marshal(map[string]string{"systemMessage": text})
	fmt.Println(string(out))
}

// sessionStartNote tells Claude, in a clawdh session only, how to set a move
// when asked in plain words. It is the only way Claude knows the command
// exists; without it "switch to saif at 5%" gets an apology.
const sessionStartNote = "This Claude Code session runs as the account %[1]s, through clawdh. " +
	"When the user asks to move it to another account later — for example once usage reaches some level — run " +
	"`clawdh <account> at <N>%%` for one of their own accounts, or `clawdh shared <account> at <N>%%` for one shared with them. " +
	"It watches %[1]s's weekly usage. " +
	"The move happens after an answer finishes and keeps this conversation; `at 0%%` moves as soon as the current answer finishes. " +
	"`clawdh stay` cancels a pending move and `clawdh list` names the accounts. " +
	"A bare `clawdh <account>` restarts the session at once and cuts off the current answer, so do not use it for this."

// hookSessionStart is Claude Code's SessionStart hook. Outside a clawdh
// session it says nothing.
func hookSessionStart() int {
	if os.Getenv(switching.HandoffEnvVar) == "" {
		return 0
	}
	account := os.Getenv(statusline.AccountEnvVar)
	if account == "" {
		account = "this account"
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":     "SessionStart",
		"additionalContext": fmt.Sprintf(sessionStartNote, account),
	}})
	fmt.Println(string(out))
	return 0
}

// usagePercent is how much of window the session's account has used, 0-100,
// from a reading fresh enough to act on. False when there is no such reading:
// a rule never fires on a guess.
func usagePercent(sess sessions.Session, window string) (float64, bool) {
	if sess.Shared {
		for _, w := range panel.LoadWindows() {
			if !strings.EqualFold(w.Slug, sess.Slug) {
				continue
			}
			if time.Since(w.UpdatedAt) > sharedReadingMaxAge {
				return 0, false
			}
			return w.SevenD * 100, true
		}
		return 0, false
	}
	// One of the machine's own accounts: ask the service, which re-reads the
	// account when its reading is stale — the same numbers the page shows.
	if sess.AccountID == "" {
		return 0, false
	}
	return localUsagePercent(fmt.Sprintf("http://127.0.0.1:%d", servicePort()), sess.AccountID, window)
}

func localUsagePercent(base, accountID, window string) (float64, bool) {
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(base + "/api/accounts/" + url.PathEscape(accountID) + "/usage")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var snap usage.Snapshot
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&snap) != nil || snap.Usage == nil {
		return 0, false
	}
	for _, l := range snap.Usage.Limits {
		if l.Kind == "weekly_all" {
			return l.Percent, true
		}
	}
	return 0, false
}
