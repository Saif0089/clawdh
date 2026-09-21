package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/panel"
)

// A local account can only run while its Claude login is still on this
// machine. For a long time that was assumed: a switch relaunched the session
// onto whichever account was named, and a login that had gone missing showed
// up only afterwards — as a session that could do nothing, or in an editor as
// a chat that restarted itself and took every other chat in the window with
// it (the extension reads an identity it did not expect as "another account
// signed in outside this window" and refreshes every webview).
//
// A login goes missing for one ordinary reason. It was handed to the gateway,
// which refreshes it centrally, and a Claude refresh token is single-use: the
// gateway's first refresh rotates it, and the copy on this machine is dead from
// then on. Claude Code notices at its next refresh and empties the store. So a
// local target is checked before anything is staged or relaunched, and the
// answer says where the login went — to the share that now runs it, when this
// machine has one.

// hasLogin reports whether an account's Claude login is on this machine. It
// reads the store; nothing is written.
func hasLogin(acct accounts.Account) bool {
	_, err := accounts.CaptureLogin(acct.ConfigDir)
	return err == nil
}

// missingLogin says why a local account cannot run right now — one sentence
// per line, fit for a terminal or a hook's block message — or "" when it can.
// The account's identity stub tells a login that went missing from one that
// never existed, and the shares cache tells whether the same login now runs
// through the gateway.
func missingLogin(acct accounts.Account, accountsDir string) string {
	if hasLogin(acct) {
		return ""
	}
	name := displayName(acct)
	page := fmt.Sprintf("http://127.0.0.1:%d", config.DefaultPort)

	email := identityEmail(identityStubDir(acct, accountsDir))
	if email == "" {
		return fmt.Sprintf("%s isn't connected on this machine yet.\nConnect it on the clawdh page (%s), then run `clawdh %s`.", name, page, acct.Slug)
	}
	if sh, ok := shareForLogin(email); ok {
		return fmt.Sprintf("%s has no login on this machine any more.\nIts login (%s) is on the gateway as the shared account %s — `clawdh shared %s` runs it from there.\nTo use it here again, reconnect %s on the clawdh page (%s).",
			name, email, sh.Slug, sh.Slug, name, page)
	}
	return fmt.Sprintf("%s has no login on this machine any more — it was signed out, or its session expired.\nReconnect it on the clawdh page (%s).", name, page)
}

// identityStubDir is where an account's own oauthAccount lives: its config
// directory, or the snapshot clawdh keeps for the default login (see
// applyIdentity, which reads the same stub).
func identityStubDir(acct accounts.Account, accountsDir string) string {
	if acct.ConfigDir != "" {
		return acct.ConfigDir
	}
	return filepath.Join(accountsDir, "default")
}

// identityEmail is the email in an identity stub, or "" when there is none —
// which means the account was never connected.
func identityEmail(stubDir string) string {
	oa, err := accounts.ReadOAuthAccount(stubDir)
	if err != nil || oa == nil {
		return ""
	}
	email, _ := oa["emailAddress"].(string)
	return strings.TrimSpace(email)
}

// shareForLogin finds the gateway share that runs the login with this email,
// if this machine has one. A share carries the account's email since the panel
// started sending it; an older cache names the account by its panel name, which
// the web page's add flow sets to the email, so that is matched too.
func shareForLogin(email string) (panel.GatewayShare, bool) {
	for _, sh := range sharedAccounts() {
		if strings.EqualFold(sh.Email, email) || strings.EqualFold(sh.Account, email) {
			return sh, true
		}
	}
	return panel.GatewayShare{}, false
}

// printProblem writes a multi-line reason the way the rest of the CLI reports
// one: `clawdh:` on the first line, the rest indented under it.
func printProblem(reason string) {
	lines := strings.Split(reason, "\n")
	fmt.Fprintln(os.Stderr, "clawdh:", lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintln(os.Stderr, "      "+l)
	}
}
