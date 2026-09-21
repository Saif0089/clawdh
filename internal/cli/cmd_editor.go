package cli

import (
	"fmt"
	"os"
	"strings"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/editors"
	"clawdh/internal/service"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// cmdEditor points VS Code and its relatives at a clawdh account.
//
// The Claude Code extension never sees the user's shell — it spawns Claude
// itself — so aliases and clawdh's `claude` function do nothing for it. What it
// does read is its own `claudeCode.claudeProcessWrapper` setting: the
// executable it launches Claude through. clawdh puts itself there (cmdExec),
// and this command records which account that wrapper starts new
// conversations as — one of your own logins, or an account shared with you
// through the gateway:
//
//	clawdh editor                  what each editor is set to
//	clawdh editor <name>           a local account, or a share when no local account has that name
//	clawdh editor shared <name>    a share, explicitly
//
// Names are the ones `clawdh list` prints and the run commands take.
func cmdEditor(args []string) int {
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
	list, err := accounts.NewStore(accountsFile).Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	shares := sharedAccounts()

	installed := editors.Installed(home)
	var withExt []editors.Editor
	for _, ed := range installed {
		if ed.HasExtension {
			withExt = append(withExt, ed)
		}
	}
	if len(installed) == 0 {
		fmt.Println("No VS Code-family editor found on this machine.")
		return 0
	}
	if len(withExt) == 0 {
		fmt.Println("No editor here has the Claude Code extension installed, so there is nothing to point at an account.")
		return 0
	}

	if len(args) == 0 {
		reportEditors(installed, readEditorDefault(accountsDir), list, shares)
		return 0
	}

	rec, label, ok := resolveEditorChoice(args, list, shares)
	if !ok {
		name := strings.Join(args, " ")
		fmt.Fprintf(os.Stderr, "clawdh: no account called %q.\n", name)
		fmt.Fprintf(os.Stderr, "      Your accounts: %s. Shared with you: %s.\n", accountNames(list), shareNames(shares))
		fmt.Fprintln(os.Stderr, "      `clawdh list` shows them all with the name each one goes by.")
		return 1
	}
	self, err := service.SelfPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: cannot find my own binary:", err)
		return 1
	}

	// The account every new conversation starts as. The wrapper reads this
	// each time the editor launches Claude.
	if err := writeEditorDefault(accountsDir, rec); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	failed := false
	for _, ed := range withExt {
		if err := editors.PointAtWrapper(ed.Settings, self); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: could not update settings.json: %v\n", ed.Name, err)
			failed = true
			continue
		}
		fmt.Printf("  %s → %s\n", ed.Name, label)
	}
	for _, ed := range installed {
		if !ed.HasExtension {
			fmt.Printf("  %s: skipped, no Claude Code extension installed\n", ed.Name)
		}
	}
	if failed {
		return 1
	}
	fmt.Println("\nNew conversations start on this account; ones already open keep the account they")
	fmt.Println("started with. In any conversation, type `clawdh <name>` (or `clawdh shared <name>`)")
	fmt.Println("to move that conversation — and only that one — to another account. It restarts")
	fmt.Println("there with the conversation resumed; anything running inside it does not survive.")
	return 0
}

// resolveEditorChoice turns the words after `clawdh editor` into a recorded
// default and the label to confirm it by. A local account wins a bare name;
// `shared <name>` asks for the share outright.
func resolveEditorChoice(args []string, list []accounts.Account, shares []panel.GatewayShare) (editorDefault, string, bool) {
	name, sharedOnly := args[0], false
	if len(args) >= 2 && args[0] == "shared" {
		name, sharedOnly = args[1], true
	}
	if !sharedOnly {
		if acct, ok := switching.ResolveAccount(list, name); ok {
			return editorDefault{AccountID: acct.ID, Name: displayName(acct), ConfigDir: acct.ConfigDir},
				fmt.Sprintf("%s (clawdh %s)", displayName(acct), acct.Slug), true
		}
	}
	for _, sh := range shares {
		if strings.EqualFold(sh.Slug, name) {
			return editorDefault{Shared: sh.Slug, Name: sh.Account},
				fmt.Sprintf("%s (clawdh shared %s)", sh.Account, sh.Slug), true
		}
	}
	return editorDefault{}, "", false
}

// reportEditors says what each editor is set to.
func reportEditors(installed []editors.Editor, rec editorDefault, list []accounts.Account, shares []panel.GatewayShare) {
	def := editorDefaultLabel(rec, list, shares)
	for _, ed := range installed {
		switch {
		case !ed.HasExtension:
			fmt.Printf("  %-18s no Claude Code extension installed\n", ed.Name)
		case editors.WrapperPath(ed.Settings) == "":
			fmt.Printf("  %-18s not managed by clawdh (uses your default login)\n", ed.Name)
		default:
			fmt.Printf("  %-18s new conversations start as %s\n", ed.Name, def)
		}
	}
}

// shareNames lists the shares this machine can run, for an error message.
func shareNames(shares []panel.GatewayShare) string {
	if len(shares) == 0 {
		return "none yet"
	}
	names := make([]string, 0, len(shares))
	for _, sh := range shares {
		names = append(names, sh.Slug)
	}
	return strings.Join(names, ", ")
}
