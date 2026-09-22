package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/buildinfo"
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/service"
	"clawdh/internal/sessions"
	"clawdh/internal/statusline"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// switchPollInterval is how often the supervisor checks for a pending switch
// while Claude Code runs. Fast enough that the redraw feels immediate, slow
// enough to be free.
var switchPollInterval = 150 * time.Millisecond

// claudeRunner launches Claude Code and reports (exit code, whether the
// supervisor terminated it for a switch). Swapped out in tests.
var claudeRunner = runClaudeOnce

// runClaudeOnce takes the running account's id so its poll loop can notice a
// revocation of that account and stop the session, the same way it notices a
// staged switch.

// onSwitch is called when a switch is staged while Claude Code runs. It
// returns true if it dealt with the handoff on its own — there is nothing to
// relaunch — and false if the session has to be relaunched on the other
// account.
type onSwitch func(switching.Handoff) bool

// claudeCodeEnvVar is set in every process Claude Code spawns, so it tells a
// `!clawdh ...` invocation that it is running inside a session even when that
// session has no clawdh supervisor to switch.
const claudeCodeEnvVar = "CLAUDECODE"

// stdinIsTTY reports whether the session would own a real terminal. Swapped
// out in tests, which run with stdin on a pipe.
var stdinIsTTY = func() bool { return isatty(os.Stdin.Fd()) }

// cmdRun is the switchable session supervisor. It launches Claude Code for one
// account and stays resident: when the in-session `clawdh <name>` hook records a
// switch, it relaunches Claude Code as the new account with the conversation
// resumed, in the same terminal. When Claude Code exits on its own, so does it.
func cmdRun(args []string) int {
	// --auto is how the `claude` shell wrapper calls in: the user did not name
	// an account, so clawdh supervises whichever one a plain `claude` would have
	// used. Everything after it belongs to Claude Code.
	auto := len(args) > 0 && args[0] == "--auto"
	if auto {
		args = args[1:]
	}
	if !auto && len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: clawdh run <account> [claude args...]")
		return 2
	}
	var startName string
	passthrough := args
	if !auto {
		startName = args[0]
		passthrough = args[1:]
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	accountsFile, err := config.AccountsFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	accountsDir, err := config.AccountsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	// Someone is using clawdh, so its service should be up (see
	// servicehealth.go). Off to the side; nothing below waits for it.
	ensureServiceRunning()

	store := accounts.NewStore(accountsFile)
	claudeBin := claudebin.Resolve()
	claudeDir := sharedClaudeDir(home)
	claudeJSON := filepath.Join(home, ".claude.json")
	settings := filepath.Join(claudeDir, "settings.json")

	// Install the switch hook into the shared settings.json (idempotent). A
	// failure only means in-session switching won't work; the session still
	// runs, so it is a warning, not fatal.
	if self, err := service.SelfPath(); err == nil {
		if err := switching.EnsureUserPromptSubmitHook(settings, self); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: could not install switch hook:", err)
		}
	}
	ensureStatusLine(settings)

	list, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	acct, ok := switching.ResolveAccount(list, startName)
	if auto {
		// A plain `claude` starts on whatever new sessions are set to, which may
		// be an account shared through the gateway — a target no local account
		// lookup can express, so it is run the way `clawdh shared` runs it, and
		// supervised just the same.
		if sh, ok := autoShare(); ok {
			return launchSharedSupervised(sh.Gateway, sh.Key, sh.Slug, passthrough)
		}
		acct, ok = autoAccount(list)
		if !ok {
			// Nothing to supervise (no default account registered yet): let
			// the caller fall back to plain Claude Code rather than fail.
			return runPlainClaude(claudeBin, passthrough)
		}
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "clawdh: no account %q\n", startName)
		return 1
	}
	if startName == "" {
		startName = acct.Slug
	}

	// Inside a session this supervisor is already running, `clawdh <account>` is
	// a switch, not a new session: stage the handoff and let the loop below
	// relaunch the terminal on the other account. This is the path `!clawdh
	// <name>` takes — Claude Code runs it as a plain shell command, so it
	// never reaches the UserPromptSubmit hook, but it does inherit both the
	// handoff path and the session id from the session it was typed in.
	//
	// Two things have to hold. Only the bare form is a switch: `clawdh ehti -p
	// "..."` inside a session is a deliberate one-shot on another account, and
	// staging a switch would kill the live session and throw those arguments
	// away. And the supervisor has to still be there: CLAWDH_HANDOFF is
	// inherited by anything a session spawned, including processes that
	// outlive it, and staging a handoff nobody will read reported a switch
	// that never happened.
	if handoffPath := os.Getenv(switching.HandoffEnvVar); handoffPath != "" && len(passthrough) == 0 && !auto {
		if supervisorAlive() {
			// Relaunching the conversation onto an account that cannot sign in
			// is not a switch, it is a broken session — say why before, not after.
			if reason := missingLogin(acct, accountsDir); reason != "" {
				printProblem(reason)
				return 1
			}
			h := switching.Handoff{Account: acct.Slug, SessionID: os.Getenv(switching.SessionIDEnvVar)}
			if err := switching.WriteHandoff(handoffPath, h); err != nil {
				fmt.Fprintln(os.Stderr, "clawdh: could not stage the switch:", err)
				return 1
			}
			fmt.Printf("Switching to %s…\n", displayName(acct))
			return 0
		}
		fmt.Fprintln(os.Stderr, "clawdh: the clawdh session this was launched from is gone, so there is nothing to switch.")
		return 1
	}

	// No supervisor. Starting a session here needs a terminal on stdin, and
	// without one Claude Code falls back to --print and dies with "Input must
	// be provided either through stdin or as a prompt argument" — an error
	// about a flag nobody typed. Say what is actually wrong instead.
	// Passthrough args mean the caller is driving Claude Code deliberately
	// (`clawdh ehti -p "..."`), so those are left alone.
	if len(passthrough) == 0 && !stdinIsTTY() {
		if os.Getenv(claudeCodeEnvVar) != "" {
			fmt.Fprintf(os.Stderr, "clawdh: this Claude Code session was not started by clawdh, so `!clawdh %s` cannot switch it.\n", startName)
			fmt.Fprintln(os.Stderr, "      An account is fixed when claude starts; switching in place means relaunching")
			fmt.Fprintln(os.Stderr, "      the session, which only clawdh's supervisor can do.")
			fmt.Fprintf(os.Stderr, "      Start sessions as `clawdh <account> [claude flags...]` — then `!clawdh %s`\n", startName)
			fmt.Fprintln(os.Stderr, "      switches the running session, conversation and all.")
			return 1
		}
		fmt.Fprintf(os.Stderr, "clawdh: `clawdh %s` starts an interactive Claude Code session, and stdin is not a terminal.\n", startName)
		fmt.Fprintf(os.Stderr, "      Run it in your terminal, or pass Claude Code's own arguments (`clawdh %s -p \"...\"`).\n", startName)
		return 1
	}

	// An account someone named has to be able to sign in; a session on one
	// that cannot is a login prompt with the account's name on it, and when
	// the login went to the gateway the right command is a different one.
	// --auto is exempt: it is the plain `claude` wrapper, and a machine whose
	// default login is not signed in yet signs in through exactly that session.
	if !auto {
		if reason := missingLogin(acct, accountsDir); reason != "" {
			printProblem(reason)
			return 1
		}
	}

	handoff := filepath.Join(accountsDir, fmt.Sprintf(".handoff-%d.json", os.Getpid()))
	ledger := switching.LedgerPath(home)
	sharesPath, _ := config.SharesFile()
	resolve := func(h switching.Handoff) (sessionTarget, error) {
		return resolveHandoffTarget(h, store, accountsDir, claudeJSON, sharesPath)
	}
	return superviseSession(claudeBin, claudeDir, ledger, handoff, localTarget(acct, accountsDir, claudeJSON), passthrough, resolve)
}

// sessionTarget is one account the supervisor runs and can switch between — a
// local account or a gateway-shared account. Everything the loop needs that
// differs between the two is captured here; the switching itself is identical.
type sessionTarget struct {
	display   string   // for the switch message and the revocation notice
	env       []string // the environment to launch claude with (before the supervisor vars)
	accountID string   // for the revocation poll; "" for a share (never locally revoked)
	slug      string   // the name it is run by: an account's slug, or a share's
	ownerDir  string   // the usage-ledger owner (an account's ConfigDir), recorded only when local
	local     bool     // a local account (record ownership, apply identity, poll for revocation) vs a share
	applyID   func()   // point ~/.claude.json at this account for /status; nil for a share
	shareKey  string   // the gateway key a share launched with; watched for a mid-session change (a revoke+re-grant mints a new one). "" for a local account.
}

// localTarget builds the supervisor target for a local account.
func localTarget(acct accounts.Account, accountsDir, claudeJSON string) sessionTarget {
	return sessionTarget{
		display:   displayName(acct),
		env:       accounts.EnvForSharedConfig(acct.ConfigDir),
		accountID: acct.ID,
		slug:      acct.Slug,
		ownerDir:  acct.ConfigDir,
		local:     true,
		applyID:   func() { applyIdentity(acct, accountsDir, claudeJSON) },
	}
}

// superviseSession runs claude for a target and relaunches it — same terminal,
// same conversation — whenever a switch to another target is staged, until the
// session ends. It is the one supervisor behind both `clawdh <account>` and
// `clawdh shared <slug>`; resolve turns a staged handoff into the next target,
// local or shared, or the reason (fit to show the person) it cannot.
func superviseSession(claudeBin, claudeDir, ledger, handoff string, target sessionTarget, passthrough []string, resolve func(switching.Handoff) (sessionTarget, error)) int {
	defer os.Remove(handoff)
	defer switching.ClearOutcome(handoff) // an outcome nobody collected (a `!clawdh` switch has no hook waiting)
	// The hook writes the handoff here; on a machine that has only ever
	// joined shares the directory may not exist yet, and a hook that cannot
	// write is a switch that never happens.
	if err := os.MkdirAll(filepath.Dir(handoff), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: in-session switching unavailable:", err)
	}

	// Mint the session id up front so usage is attributed from the first token,
	// not the first switch. Only on a fresh launch, and kept out of passthrough
	// so a relaunch's --resume never collides with a minted --session-id.
	launchArgs := passthrough
	var sessionID string // the conversation to resume on a relaunch (switch or re-key)
	if hasSessionArgs(passthrough) {
		sessionID = sessionIDFromArgs(passthrough)
	} else if id, err := newSessionID(); err == nil {
		sessionID = id
		launchArgs = append([]string{"--session-id", id}, passthrough...)
		if target.local {
			if err := switching.AppendOwnership(ledger, id, target.ownerDir); err != nil {
				fmt.Fprintln(os.Stderr, "clawdh: could not record this session for the usage monitor:", err)
			}
		}
	}

	// A session on a shared account is recorded in clawdh's shared-session ledger:
	// the one thing remote help may ever look at. Personal sessions never enter it.
	recordSharedSession(target, sessionID)

	// Publish this session so the page can show it and move it. The nonce goes
	// with it: a switch staged from the page carries the nonce of the session it
	// was aimed at, so one aimed at a session that has since ended can never be
	// picked up by an unrelated supervisor that inherited its pid.
	registerDir, _ := config.SessionsDir()
	nonce := sessions.NewNonce()
	publish := func(t sessionTarget, id string) {
		if registerDir == "" {
			return
		}
		_ = sessions.Publish(registerDir, describeSession(t, id, nonce, handoff))
	}
	defer sessions.Withdraw(registerDir, os.Getpid())

	// One signal channel for the life of the supervisor, not one per launch. A
	// signal that landed between two launches — during a switch, while the
	// hook's message was still on its way to the screen — met Go's default
	// handler and took the supervisor down mid-switch, or sat unread in a
	// channel about to be dropped: an editor closing a chat in that window got
	// a relaunch instead, and the closed chat's Claude Code lived on.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	supervisorSignals = sigs
	defer func() { supervisorSignals = nil }()

	// In an editor, the shared ~/.claude.json names the account the editor is
	// set to, and only that one. The Claude Code extension watches the file and
	// takes any other identity appearing in it for "another account signed in
	// outside this window" — on which it refreshes every webview and restarts
	// every chat in the window, not just the one that switched. Each chat is
	// its own supervisor here, so the first launch (the editor's account) may
	// assert the identity and a switch inside a chat may not; the cost is that
	// /status inside a switched chat still names the editor's account. A
	// terminal has one session per window and keeps asserting on every launch.
	assertIdentity := true
	sessionArgs := launchArgs
	for {
		publish(target, sessionID)
		if target.applyID != nil && assertIdentity {
			target.applyID()
		}
		assertIdentity = !hostedByEditor
		switching.ClearHandoff(handoff)

		// The badge on Claude Code's status line reads these: the build that
		// launched this session and the account it runs as (see statusline).
		env := append(append([]string(nil), target.env...),
			switching.HandoffEnvVar+"="+handoff,
			fmt.Sprintf("%s=%d", switching.SupervisorEnvVar, os.Getpid()),
			statusline.VersionEnvVar+"="+buildinfo.Compact(),
			statusline.AccountEnvVar+"="+target.display)

		// A shared session's gateway key can change under it — a revoke then a
		// re-grant mints a new one — and the frozen key would 401 forever. Watch
		// shares.json (the background check-in keeps it current) and, when this
		// account's key changes, stage the ordinary switch back to it, so the loop
		// below relaunches with the fresh key and the same conversation. It only
		// reads shares.json; nothing writes a credential or touches the keychain.
		var stopWatch chan struct{}
		if !target.local && target.shareKey != "" {
			if sp, err := config.SharesFile(); err == nil {
				stopWatch = make(chan struct{})
				go watchShareKey(sp, target.display, target.shareKey, sessionID, handoff, stopWatch)
			}
		}

		// Nothing in this callback prints. Claude Code owns the terminal while it
		// runs, so a write here corrupts the TUI mid-paint; the reply goes back
		// through the outcome file to the hook that staged the switch.
		code, switched := claudeRunner(claudeBin, sessionArgs, env, handoff, target.accountID, func(h switching.Handoff) bool {
			if h.For != "" && h.For != nonce {
				return true // staged for a session that has ended, not for this one
			}
			next, err := resolve(h)
			if err != nil {
				switching.WriteOutcome(handoff, switching.Outcome{Message: err.Error()})
				return true // not a reason to restart the session
			}
			switching.WriteOutcome(handoff, switching.Outcome{
				Message: "Switching to " + next.display + " restarts this session. The conversation comes with it; anything running inside it does not.",
			})
			return false
		})
		if stopWatch != nil {
			close(stopWatch) // the watcher stops here whether or not it staged a switch
		}
		if !switched {
			// stderr, not stdout: in a terminal both are the screen, but when an
			// editor hosts this session stdout is the message channel the extension
			// parses, and a sentence there is a protocol error.
			if target.local && config.IsRevoked(target.accountID) {
				fmt.Fprintf(os.Stderr, "\nYour access to %s was withdrawn by your team. This session has stopped; your conversation is saved.\n", target.display)
			}
			return code
		}

		h, ok := switching.ReadHandoff(handoff)
		if !ok {
			return code
		}
		// The editor closed this chat while the switch was under way: stop here
		// rather than start a Claude Code nobody is talking to.
		if stopRequested(sigs) {
			return code
		}
		next, err := resolve(h)
		if err != nil {
			printProblem(err.Error())
			return 1
		}
		// A relaunch onto the same shared account is the key-watcher recovering from
		// a revoke+re-grant, not a user switch — say so, since the user did not ask.
		if !next.local && !target.local && strings.EqualFold(next.display, target.display) {
			fmt.Fprintf(os.Stderr, "clawdh: your access to %s was renewed — reconnecting; your conversation continues.\n", next.display)
		}
		target = next
		sessionID = h.SessionID
		recordSharedSession(target, sessionID)
		// The conversation keeps its id across the switch; the monitor resolves
		// ownership by interval, so work done before it stays with the account
		// that did it. A share's usage is metered by the gateway, so only a local
		// target records local ownership.
		if target.local {
			if err := switching.AppendOwnership(ledger, h.SessionID, target.ownerDir); err != nil {
				fmt.Fprintln(os.Stderr, "clawdh: could not record the switch for the usage monitor:", err)
			}
		}
		// The relaunch names the conversation it carries over, so whatever the
		// launch said about sessions (an editor's --session-id=<id>, a user's
		// --resume) has to go — two session flags is an error, and the wrong one
		// winning is a conversation silently left behind.
		resume := switching.ResumeArgs(h.SessionID, switching.HasTranscript(claudeDir, h.SessionID))
		sessionArgs = append(switching.WithoutSessionArgs(passthrough), resume...)
	}
}

// hostedByEditor is set when an editor, not a terminal, owns this supervisor
// (see cmdExec). Two things differ. The terminal delivers ctrl-c to Claude Code
// itself, so a terminal supervisor absorbs signals; an editor delivers them
// to the supervisor — the only process it knows — and expects the whole
// session to end, so an editor supervisor passes them down. And the editor
// has no screen to rebuild: it parses stdout, so nothing extra may be written
// there (superviseSession writes to stderr for that reason regardless).
var hostedByEditor bool

// supervisorSignals is where ctrl-c and SIGTERM arrive for as long as
// superviseSession runs; each launch listens on it rather than on a channel of
// its own, so no signal falls into the gap between two launches (see the note
// where it is opened). Nil outside superviseSession.
var supervisorSignals chan os.Signal

// stopRequested reports whether a signal has arrived that means the session
// should end rather than go on to its next launch. Only an editor's does: it
// signals the supervisor to close the chat. A terminal supervisor absorbs
// signals — Claude Code got the same ctrl-c from the tty — so there the answer
// is always no, and the signal is simply drained.
func stopRequested(sigs <-chan os.Signal) bool {
	select {
	case <-sigs:
		return hostedByEditor
	default:
		return false
	}
}

// switchGrace is how long the supervisor gives the hook to collect the outcome
// it wrote before ending the session, and switchSettle how long after that it
// lets Claude Code show it. Ending the session the instant the switch is
// staged killed the very turn that was reporting it: in a terminal that was a
// flash of half-drawn screen, and in an editor a turn that never ended, since
// the process died before it could say so.
var (
	switchGrace  = 1500 * time.Millisecond
	switchSettle = 250 * time.Millisecond
)

// awaitOutcomeRead returns once the hook has picked up the outcome, or after
// switchGrace when nothing does — a `!clawdh <name>` staged from a shell
// command has no hook waiting, and that must not stall the switch for long.
func awaitOutcomeRead(handoff string) {
	deadline := time.Now().Add(switchGrace)
	for switching.OutcomePending(handoff) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(switchSettle)
}

// shareKeyPollInterval is how often a shared session checks whether its gateway
// key changed under it — well under the check-in interval that refreshes the file.
var shareKeyPollInterval = time.Second

// watchShareKey stages a switch back to the same shared account when its gateway
// key changes under the running session. A revoke-then-re-grant mints a new key,
// and the session's frozen ANTHROPIC_AUTH_TOKEN would 401 until it is replaced;
// staging the ordinary switch makes the supervisor relaunch with the fresh key
// and the same conversation, exactly like an in-session switch. It only reads
// shares.json (kept current by the background check-in) — no credential is
// written and the keychain is never touched, since a share is a bearer token in
// an env var, not a login. It returns once it has staged a switch or is stopped.
func watchShareKey(sharesPath, slug, launchedKey, sessionID, handoff string, stop <-chan struct{}) {
	t := time.NewTicker(shareKeyPollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			shares, err := panel.LoadShares(sharesPath)
			if err != nil {
				continue // an unreadable/absent file is a revoked window, not a new key
			}
			for _, sh := range shares {
				if strings.EqualFold(sh.Slug, slug) && sh.Key != "" && sh.Key != launchedKey {
					_ = switching.WriteHandoff(handoff, switching.Handoff{Account: slug, SessionID: sessionID, Shared: true})
					return
				}
			}
		}
	}
}

// recordSharedSession notes a shared-account session in clawdh's ledger — the
// privacy boundary for remote help (see switching.RecordSharedSession). Local
// accounts are the person's own business and are never recorded. Best-effort:
// a failed write must never stop a session from launching.
func recordSharedSession(target sessionTarget, sessionID string) {
	if target.local || sessionID == "" {
		return
	}
	if path, err := config.SharedSessionsFile(); err == nil {
		_ = switching.RecordSharedSession(path, sessionID, target.display)
	}
}

// sessionIDFromArgs pulls the conversation id out of session args the caller
// already set, so a watched relaunch can resume the same conversation. Empty for
// --continue/-c, which name no id.
func sessionIDFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case (a == "--session-id" || a == "--resume" || a == "-r") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--session-id="):
			return a[len("--session-id="):]
		case strings.HasPrefix(a, "--resume="):
			return a[len("--resume="):]
		}
	}
	return ""
}

// sharedClaudeDir is the ~/.claude every account shares. It deliberately
// ignores an inherited CLAUDE_CONFIG_DIR: EnvForSharedConfig strips that
// variable from the child, so ~/.claude is where Claude Code will actually
// read settings and write transcripts no matter what the launching shell had
// set. Honouring it here instead installed the switch hook in a settings.json
// Claude Code never reads, and looked for transcripts in the wrong tree.
func sharedClaudeDir(home string) string {
	return filepath.Join(home, ".claude")
}

// autoAccount is the account a plain `claude` runs as, in the order that
// respects what the person most recently and most specifically said:
//
//   - the account the shell already points at, because someone exported
//     CLAUDE_SECURESTORAGE_CONFIG_DIR by hand or is inside a `clawdh <name>`
//     session — naming an account outright always wins;
//   - whatever new sessions are set to on the page, when its login is still
//     here. A login that has gone (handed to the gateway, signed out) falls
//     through rather than starting a session that can do nothing;
//   - the machine's default login, which is what `claude` does with no
//     variables set at all.
func autoAccount(list []accounts.Account) (accounts.Account, bool) {
	if dir := strings.TrimSpace(os.Getenv(accounts.SecureStorageEnvVar)); dir != "" {
		want := filepath.Clean(dir)
		for _, a := range list {
			if a.ConfigDir != "" && strings.EqualFold(filepath.Clean(a.ConfigDir), want) {
				return a, true
			}
		}
	}
	if rec := newSessionDefault(); rec.AccountID != "" {
		for _, a := range list {
			if a.ID == rec.AccountID && hasLogin(a) {
				return a, true
			}
		}
	}
	for _, a := range list {
		if a.IsDefault() {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// autoShare is the gateway share a plain `claude` runs as, when new sessions
// are set to one and it is still shared with this machine. A shell that
// already points at a local account has named one outright, so it wins — the
// same order autoAccount reads in.
func autoShare() (panel.GatewayShare, bool) {
	if strings.TrimSpace(os.Getenv(accounts.SecureStorageEnvVar)) != "" {
		return panel.GatewayShare{}, false
	}
	rec := newSessionDefault()
	if rec.Shared == "" {
		return panel.GatewayShare{}, false
	}
	for _, sh := range sharedAccounts() {
		if strings.EqualFold(sh.Slug, rec.Shared) {
			return sh, true
		}
	}
	return panel.GatewayShare{}, false
}

// runPlainClaude is the last resort for --auto: hand the terminal to Claude
// Code exactly as the shell would have, unsupervised, rather than refuse to
// start because clawdh has nothing registered.
func runPlainClaude(bin string, args []string) int {
	return runClaudeAs(bin, args, nil)
}

// applyIdentity makes the shared ~/.claude.json name the account about to run,
// so /status and the statusline are correct. For a managed account the
// oauthAccount comes from its own stub; for the default account it comes from
// the snapshot clawdh took on first boot.
func applyIdentity(acct accounts.Account, accountsDir, claudeJSON string) {
	stubDir := acct.ConfigDir
	if stubDir == "" {
		stubDir = filepath.Join(accountsDir, "default")
	}
	oa, err := accounts.ReadOAuthAccount(stubDir)
	if err != nil || oa == nil {
		return // best-effort: a missing stub just leaves the identity as-is
	}
	_ = accounts.SetActiveIdentity(claudeJSON, oa)
}

// runClaudeOnce launches Claude Code with stdio inherited (it owns the
// terminal) and watches for a pending switch. It returns when Claude Code
// exits — either on its own, or because a switch was staged and the supervisor
// terminated it.
func runClaudeOnce(bin string, args, env []string, handoff, accountID string, applyInPlace onSwitch) (int, bool) {
	name, argv := claudebin.Invocation(bin, args)
	cmd := exec.Command(name, argv...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: launching claude:", err)
		return 1, false
	}

	// The terminal delivers ctrl-c to Claude Code directly (same process
	// group), so the supervisor must not die on it — it absorbs the common
	// signals and lets Claude Code handle them. An editor is the other way
	// round: it signals the supervisor, the only process it spawned, and a
	// supervisor that shrugged that off left Claude Code running after the
	// chat was closed, until the extension gave up and SIGKILLed the tree.
	// The channel belongs to the supervisor as a whole (supervisorSignals);
	// only a launch outside one listens for itself.
	sigCh := supervisorSignals
	if sigCh == nil {
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
	}

	stop := make(chan struct{})
	switched := make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(switchPollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-sigCh:
				if hostedByEditor {
					terminate(cmd) // the editor is closing this session: end it, don't relaunch
					return
				}
				// absorb — Claude Code already received it from the tty
			case <-t.C:
				// The account was taken back by the panel: stop this session.
				// switched stays unsent, so runClaudeOnce returns "not switched"
				// and cmdRun exits with the revocation message rather than
				// relaunching.
				if config.IsRevoked(accountID) {
					terminate(cmd)
					return
				}
				h, ok := switching.ReadHandoff(handoff)
				if !ok {
					continue
				}
				switching.ClearHandoff(handoff)
				// The callback settles anything that needs no relaunch — an
				// account that has gone away, an unreadable list — and reports
				// it to the waiting hook itself.
				if applyInPlace != nil {
					if applyInPlace(h) {
						continue
					}
					// The callback just told the hook what is about to happen; let
					// that reach the screen before the session goes.
					awaitOutcomeRead(handoff)
					// …unless the editor closed the chat meanwhile: then the session
					// ends here, and switched stays unsent so nothing is relaunched.
					if stopRequested(sigCh) {
						terminate(cmd)
						return
					}
				}
				if err := switching.WriteHandoff(handoff, h); err == nil {
					select {
					case switched <- struct{}{}:
					default:
					}
				}
				terminate(cmd)
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	close(stop)

	select {
	case <-switched:
		return 0, true
	default:
		return exitCodeOf(waitErr), false
	}
}

// terminate ends the child Claude Code so the supervisor can relaunch it. The
// two platforms need genuinely different endings — see terminate_unix.go and
// terminate_windows.go. cmd.Wait() in the caller reaps it either way.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	endChild(cmd)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// newSessionID mints the uuid Claude Code will use for the session, so clawdh
// knows it before the session exists.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// hasSessionArgs reports whether the caller already decided which conversation
// this is — resuming one, continuing one, or naming an id. clawdh must not mint
// an id over the top of any of those.
func hasSessionArgs(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--session-id", a == "--resume", a == "-r", a == "--continue", a == "-c":
			return true
		case strings.HasPrefix(a, "--session-id="), strings.HasPrefix(a, "--resume="):
			return true
		}
	}
	return false
}

// describeSession is this session as the register holds it: what is running,
// as whom, and where it is being typed. A local account is named by its id and
// a share by its slug, which is also how a reader tells the two apart.
func describeSession(t sessionTarget, sessionID, nonce, handoff string) sessions.Session {
	s := sessions.Session{
		PID:       os.Getpid(),
		Nonce:     nonce,
		SessionID: sessionID,
		Account:   t.display,
		Slug:      t.slug,
		Handoff:   handoff,
		StartedAt: time.Now(),
		Host:      sessions.HostTerminal,
	}
	if t.local {
		s.AccountID = t.accountID
	} else {
		s.Shared = true
		if s.Slug == "" {
			s.Slug = t.display // a share is run by its slug, which is its display name
		}
	}
	if hostedByEditor {
		s.Host = sessions.HostEditor
		s.Editor = sessions.EditorName()
	}
	if dir, err := os.Getwd(); err == nil {
		s.Dir = dir
	}
	return s
}
