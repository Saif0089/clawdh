package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"clawdh/internal/accounts"
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/service"
	"clawdh/internal/sessions"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// cmdExec is what an editor runs instead of Claude Code.
//
// The Claude Code extension can be told to launch Claude through another
// executable — `claudeCode.claudeProcessWrapper` — which it then runs with the
// real binary as the first argument. clawdh puts itself there, and from then
// on every conversation the editor starts is a supervised session, exactly
// like `clawdh <account>` in a terminal: it starts as the account the editor
// is set to (`clawdh editor <name>` — a local login or a gateway share), and
// `clawdh <name>` / `clawdh shared <name>` typed into the chat relaunches that
// conversation on the other account, resumed, while every other chat and every
// terminal stays where it was.
//
// For a while this was a plain launcher: the switching hook saw no supervisor
// behind an editor session and answered that it could not be switched. The
// supervisor is the same loop the terminal uses; what an editor changes is the
// plumbing around it — the extension talks stream-json over stdout, so
// nothing else may be written there, and it ends a session by signalling this
// process, so signals are passed down rather than absorbed (hostedByEditor).
//
// It is deliberately hard to break. Anything unexpected — no account, no
// config directory — falls through to running the real binary exactly as the
// editor would have. A user's editor must not stop working because clawdh had an
// opinion.
func cmdExec(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: clawdh exec <claude-binary> [args...]")
		return 2
	}
	bin, passthrough := args[0], args[1:]

	// An editor chat is a clawdh session like any other, and someone who only
	// ever works in an editor may not run a clawdh command for weeks — so this
	// is the one place their service gets looked in on.
	ensureServiceRunning()

	home, err := os.UserHomeDir()
	if err != nil {
		return runPlainClaude(bin, passthrough)
	}
	accountsFile, err := config.AccountsFile()
	if err != nil {
		return runPlainClaude(bin, passthrough)
	}
	accountsDir, err := config.AccountsDir()
	if err != nil {
		return runPlainClaude(bin, passthrough)
	}
	sharesPath, err := config.SharesFile()
	if err != nil {
		return runPlainClaude(bin, passthrough)
	}
	store := accounts.NewStore(accountsFile)
	claudeDir := sharedClaudeDir(home)
	claudeJSON := filepath.Join(home, ".claude.json")

	// Whichever account this editor is set to, and the user's default login
	// when it has never been set — which is what a plain `claude` would have
	// used, so an editor nobody has configured behaves exactly as before.
	target, ok := editorTarget(store, accountsDir, claudeJSON, sharesPath)
	if !ok {
		return runPlainClaude(bin, passthrough)
	}

	// The extension runs Claude Code's subcommands through the wrapper too —
	// `auth status --json` to show who is signed in, `auth logout`,
	// `design-login`. Those are one-shot commands, not conversations: they get
	// the account's environment and identity, and none of the supervision.
	// Supervising a probe minted a session id and a usage-ledger entry for a
	// session that never existed.
	if isSubcommand(passthrough) {
		if target.applyID != nil {
			target.applyID()
		}
		return runClaudeAs(bin, passthrough, target.env)
	}

	// The switch hook is what turns `clawdh <name>` typed in the chat into a
	// handoff (idempotent; a failure only disables in-session switching, so it
	// is a warning on stderr — never stdout — not a reason to refuse to start).
	if self, err := service.SelfPath(); err == nil {
		if err := switching.EnsureUserPromptSubmitHook(filepath.Join(claudeDir, "settings.json"), self); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: could not install switch hook:", err)
		}
	}
	ensureStatusLine(filepath.Join(claudeDir, "settings.json"))

	handoff := filepath.Join(accountsDir, fmt.Sprintf(".handoff-%d.json", os.Getpid()))
	ledger := switching.LedgerPath(home)
	resolve := func(h switching.Handoff) (sessionTarget, error) {
		return resolveHandoffTarget(h, store, accountsDir, claudeJSON, sharesPath)
	}
	hostedByEditor = true
	return superviseSession(bin, claudeDir, ledger, handoff, target, passthrough, resolve)
}

// isSubcommand reports whether an editor's invocation is one of Claude Code's
// subcommands rather than a conversation. A conversation is always flags
// (`--output-format stream-json …`, or nothing at all); a subcommand is always
// a bare word first.
func isSubcommand(args []string) bool {
	return len(args) > 0 && !strings.HasPrefix(args[0], "-")
}

// runClaudeAs runs Claude Code once, unsupervised, with a target's environment
// (nil: the inherited one) — for the one-shot subcommands an editor sends
// through the wrapper, and for --auto's fall-through to a plain `claude`.
func runClaudeAs(bin string, args, env []string) int {
	name, argv := claudebin.Invocation(bin, args)
	cmd := exec.Command(name, argv...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return exitCodeOf(err)
	}
	return 0
}

// newSessionDefault is the account new sessions start as — an editor's chats
// and a plain `claude` alike (see sessions.Default). Reading it goes through
// one path so the page, the CLI and a launch can never disagree about it.
func newSessionDefault() sessions.Default {
	path, err := config.NewSessionDefaultFile()
	if err != nil {
		return sessions.Default{}
	}
	return sessions.ReadDefault(path)
}

// writeNewSessionDefault records the account new sessions start as.
func writeNewSessionDefault(d sessions.Default) error {
	path, err := config.NewSessionDefaultFile()
	if err != nil {
		return err
	}
	return sessions.WriteDefault(path, d)
}

// editorTarget is the session target an editor's new conversation starts as:
// the recorded share or local account, else the default login as a plain
// `claude` would use. A recorded share that is no longer shared with this
// machine, or a recorded account whose login is no longer here, falls through
// to the local default rather than failing the editor — with a word on stderr
// (never stdout, the editor's protocol channel) about why.
func editorTarget(store *accounts.Store, accountsDir, claudeJSON, sharesPath string) (sessionTarget, bool) {
	rec := newSessionDefault()
	if rec.Shared != "" {
		if t, err := resolveHandoffTarget(switching.Handoff{Account: rec.Shared, Shared: true}, store, accountsDir, claudeJSON, sharesPath); err == nil {
			return t, true
		}
	}
	list, err := store.Load()
	if err != nil {
		return sessionTarget{}, false
	}
	if rec.AccountID != "" {
		for _, a := range list {
			if a.ID != rec.AccountID {
				continue
			}
			if reason := missingLogin(a, accountsDir); reason != "" {
				printProblem(reason + "\nThis conversation starts on your default login instead.")
				break
			}
			return localTarget(a, accountsDir, claudeJSON), true
		}
	}
	for _, a := range list {
		if a.IsDefault() {
			return localTarget(a, accountsDir, claudeJSON), true
		}
	}
	return sessionTarget{}, false
}

// editorDefaultLabel says what the recorded default is, resolved against what
// exists now — the live account name and its command, as `clawdh list` shows
// them — so `clawdh editor` and `clawdh list` never disagree about a name.
func editorDefaultLabel(rec sessions.Default, list []accounts.Account, shares []panel.GatewayShare) string {
	switch {
	case rec.Shared != "":
		for _, sh := range shares {
			if strings.EqualFold(sh.Slug, rec.Shared) {
				return fmt.Sprintf("%s (clawdh shared %s)", sh.Account, sh.Slug)
			}
		}
		return fmt.Sprintf("%s — a shared account that is no longer shared with you, so the default login is used", rec.Shared)
	case rec.AccountID != "":
		for _, a := range list {
			if a.ID == rec.AccountID {
				if !hasLogin(a) {
					return fmt.Sprintf("%s (clawdh %s) — not signed in on this machine, so the default login is used until it is reconnected", displayName(a), a.Slug)
				}
				return fmt.Sprintf("%s (clawdh %s)", displayName(a), a.Slug)
			}
		}
		return fmt.Sprintf("%s — an account that no longer exists, so the default login is used", rec.Name)
	default:
		return "not set (your default login)"
	}
}
