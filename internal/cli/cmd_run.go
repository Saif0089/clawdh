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
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/service"
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

	list, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	acct, ok := switching.ResolveAccount(list, startName)
	if auto {
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

	handoff := filepath.Join(accountsDir, fmt.Sprintf(".handoff-%d.json", os.Getpid()))
	ledger := switching.LedgerPath(home)
	sharesPath, _ := config.SharesFile()
	resolve := func(h switching.Handoff) (sessionTarget, bool) {
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
		ownerDir:  acct.ConfigDir,
		local:     true,
		applyID:   func() { applyIdentity(acct, accountsDir, claudeJSON) },
	}
}

// superviseSession runs claude for a target and relaunches it — same terminal,
// same conversation — whenever a switch to another target is staged, until the
// session ends. It is the one supervisor behind both `clawdh <account>` and
// `clawdh shared <slug>`; resolve turns a staged handoff into the next target,
// local or shared.
func superviseSession(claudeBin, claudeDir, ledger, handoff string, target sessionTarget, passthrough []string, resolve func(switching.Handoff) (sessionTarget, bool)) int {
	defer os.Remove(handoff)

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

	sessionArgs := launchArgs
	for {
		if target.applyID != nil {
			target.applyID()
		}
		switching.ClearHandoff(handoff)

		env := append(append([]string(nil), target.env...),
			switching.HandoffEnvVar+"="+handoff,
			fmt.Sprintf("%s=%d", switching.SupervisorEnvVar, os.Getpid()))

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
			next, ok := resolve(h)
			if !ok {
				switching.WriteOutcome(handoff, switching.Outcome{
					Message: "There is nothing called " + h.Account + " to switch to any more, so nothing was switched.",
				})
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
			if target.local && config.IsRevoked(target.accountID) {
				fmt.Printf("\nYour access to %s was withdrawn by your team. This session has stopped; your conversation is saved.\n", target.display)
			}
			return code
		}

		h, ok := switching.ReadHandoff(handoff)
		if !ok {
			return code
		}
		next, ok := resolve(h)
		if !ok {
			fmt.Fprintf(os.Stderr, "clawdh: cannot switch to %q\n", h.Account)
			return 1
		}
		// A relaunch onto the same shared account is the key-watcher recovering from
		// a revoke+re-grant, not a user switch — say so, since the user did not ask.
		if !next.local && !target.local && strings.EqualFold(next.display, target.display) {
			fmt.Printf("clawdh: your access to %s was renewed — reconnecting; your conversation continues.\n", next.display)
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
		resume := switching.ResumeArgs(h.SessionID, switching.HasTranscript(claudeDir, h.SessionID))
		sessionArgs = append(append([]string{}, passthrough...), resume...)
	}
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

// autoAccount is the account a plain `claude` would have run as: the one whose
// directory the shell already points at (someone who exported
// CLAUDE_SECURESTORAGE_CONFIG_DIR by hand, or one of clawdh's own aliases), and
// otherwise the default login — which is exactly what `claude` does with no
// variables set at all.
func autoAccount(list []accounts.Account) (accounts.Account, bool) {
	if dir := strings.TrimSpace(os.Getenv(accounts.SecureStorageEnvVar)); dir != "" {
		want := filepath.Clean(dir)
		for _, a := range list {
			if a.ConfigDir != "" && strings.EqualFold(filepath.Clean(a.ConfigDir), want) {
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

// runPlainClaude is the last resort for --auto: hand the terminal to Claude
// Code exactly as the shell would have, unsupervised, rather than refuse to
// start because clawdh has nothing registered.
func runPlainClaude(bin string, args []string) int {
	name, argv := claudebin.Invocation(bin, args)
	cmd := exec.Command(name, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return exitCodeOf(err)
	}
	return 0
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
	// signals and lets Claude Code handle them.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

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
				if applyInPlace != nil && applyInPlace(h) {
					continue
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
