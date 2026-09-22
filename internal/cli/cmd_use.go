package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"clawdh/internal/accounts"
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/service"
	"clawdh/internal/statusline"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// sharedSessionEnvVar marks a Claude Code process as a shared (gateway) session
// and carries the shared account's name, for the switch hook to read.
const sharedSessionEnvVar = "CLAWDH_SHARED_SESSION"

// cmdShared runs a gateway-shared account by its slug, reading the gateway URL
// and this person's key from clawdh's shares cache. Everything after the slug
// goes to Claude Code unchanged.
//
// Inside a running clawdh session it is a switch, not a new session: it stages a
// handoff and the supervisor relaunches the terminal on the share — exactly as
// `clawdh <account>` does for a local account.
//
//	clawdh shared <slug> [claude args...]
func cmdShared(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: clawdh shared <account> [claude args...]")
		return 2
	}
	slug, rest := args[0], args[1:]

	// The service is what keeps this machine's shares current and its build
	// up to date. Running a shared account with it dead is exactly the state
	// that goes unnoticed for weeks, so this is where it gets noticed.
	ensureServiceRunning()

	path, err := config.SharesFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	shares, err := panel.LoadShares(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	for _, sh := range shares {
		if !strings.EqualFold(sh.Slug, slug) {
			continue
		}
		// Inside a supervised session, a bare `clawdh shared <slug>` is a switch:
		// stage it (marked shared) and let the supervisor relaunch this terminal
		// on the share. Only the bare form — `clawdh shared X -p "…"` is a
		// deliberate one-shot — and only while the supervisor is still there.
		if handoffPath := os.Getenv(switching.HandoffEnvVar); handoffPath != "" && len(rest) == 0 {
			if supervisorAlive() {
				h := switching.Handoff{Account: sh.Slug, SessionID: os.Getenv(switching.SessionIDEnvVar), Shared: true}
				if err := switching.WriteHandoff(handoffPath, h); err != nil {
					fmt.Fprintln(os.Stderr, "clawdh: could not stage the switch:", err)
					return 1
				}
				fmt.Printf("Switching to %s…\n", sh.Slug)
				return 0
			}
			fmt.Fprintln(os.Stderr, "clawdh: the clawdh session this was launched from is gone, so there is nothing to switch.")
			return 1
		}
		return launchSharedSupervised(sh.Gateway, sh.Key, sh.Slug, rest)
	}

	fmt.Fprintf(os.Stderr, "clawdh: no shared account called %q on this machine.\n", slug)
	if len(shares) > 0 {
		names := make([]string, len(shares))
		for i, sh := range shares {
			names[i] = sh.Slug
		}
		fmt.Fprintf(os.Stderr, "      Shared with you: %s.\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintln(os.Stderr, "      Nothing is shared with this machine yet. Connect to a panel from the clawdh page.")
	}
	return 1
}

// launchSharedSupervised starts a supervised Claude Code session on a shared
// account — switchable in place, like `clawdh <account>`. label is the share's
// slug (or "a shared account" for a raw `clawdh use`).
func launchSharedSupervised(gatewayURL, key, label string, rest []string) int {
	// A fresh launch needs a terminal on stdin; without one Claude Code falls
	// back to --print and dies with "Input must be provided…". Inside a
	// supervised session a bare invocation is staged as a switch before it ever
	// reaches here, so this only catches a genuine fresh launch with no terminal.
	if len(rest) == 0 && !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "clawdh: this starts an interactive Claude Code session, and stdin is not a terminal.")
		if os.Getenv(claudeCodeEnvVar) != "" {
			fmt.Fprintln(os.Stderr, "      You're inside a Claude Code session (a `!` command), which has no terminal to attach to.")
		}
		fmt.Fprintln(os.Stderr, "      Run it in your own terminal, or pass Claude Code's own arguments (add `-p \"your prompt\"`).")
		return 1
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
	sharesPath, err := config.SharesFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	store := accounts.NewStore(accountsFile)
	claudeBin := claudebin.Resolve()
	claudeDir := sharedClaudeDir(home)
	claudeJSON := filepath.Join(home, ".claude.json")

	// Install the switch hook so `clawdh <name>` typed as a prompt switches this
	// shared session too (idempotent; a failure only disables in-session
	// switching, so it is a warning, not fatal).
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
	return superviseSession(claudeBin, claudeDir, ledger, handoff, sharedTarget(gatewayURL, key, label), rest, resolve)
}

// sharedTarget builds the supervisor target for a gateway-shared account: Claude
// pointed at the gateway (ANTHROPIC_BASE_URL) with this person's key
// (ANTHROPIC_AUTH_TOKEN). CLAUDE_CONFIG_DIR and the securestorage var are
// stripped so a stray local login can't take precedence, and — crucially —
// every provider/auth-source var (ANTHROPIC_API_KEY and friends) is stripped
// too: Claude Code treats those as taking precedence over ANTHROPIC_AUTH_TOKEN,
// so a leaked one (e.g. from a Claude Code session on API billing) would quietly
// bypass the gateway and the shared subscription it holds. Any inherited
// supervisor handoff is dropped; superviseSession adds this session's own.
func sharedTarget(gatewayURL, key, label string) sessionTarget {
	strip := append([]string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", switching.HandoffEnvVar, sharedSessionEnvVar,
		statusline.VersionEnvVar, statusline.AccountEnvVar},
		accounts.ProviderOverrideVars()...)
	env := append(accountsEnvWithout(os.Environ(), strip...),
		"ANTHROPIC_BASE_URL="+strings.TrimRight(gatewayURL, "/"),
		"ANTHROPIC_AUTH_TOKEN="+key,
		sharedSessionEnvVar+"="+label,
	)
	return sessionTarget{display: label, env: env, local: false, shareKey: key}
}

// resolveHandoffTarget turns a staged switch into the next target: a gateway
// share when the handoff is marked shared, otherwise a local account. Both
// stores are read fresh so a share added or an account renamed mid-session is
// seen. A local account also has to be able to sign in — a login that went to
// the gateway is dead here, and relaunching onto it is not a switch. The error
// is the sentence to show the person, since the hook that staged the switch
// reports it verbatim.
func resolveHandoffTarget(h switching.Handoff, store *accounts.Store, accountsDir, claudeJSON, sharesPath string) (sessionTarget, error) {
	if h.Shared {
		shares, err := panel.LoadShares(sharesPath)
		if err == nil {
			for _, sh := range shares {
				if strings.EqualFold(sh.Slug, h.Account) {
					return sharedTarget(sh.Gateway, sh.Key, sh.Slug), nil
				}
			}
		}
		return sessionTarget{}, fmt.Errorf("There is no shared account called %s on this machine any more, so nothing was switched.", h.Account)
	}
	list, err := store.Load()
	if err != nil {
		return sessionTarget{}, fmt.Errorf("clawdh could not read its accounts (%v), so nothing was switched.", err)
	}
	acct, ok := switching.ResolveAccount(list, h.Account)
	if !ok {
		return sessionTarget{}, fmt.Errorf("There is nothing called %s to switch to any more, so nothing was switched.", h.Account)
	}
	if reason := missingLogin(acct, accountsDir); reason != "" {
		return sessionTarget{}, errors.New(reason)
	}
	return localTarget(acct, accountsDir, claudeJSON), nil
}

// accountsEnvWithout drops the named vars (case-insensitive) from an env slice.
func accountsEnvWithout(env []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[strings.ToUpper(n)] = true
	}
	out := env[:0]
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && drop[strings.ToUpper(kv[:i])] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
