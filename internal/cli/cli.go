// Package cli wires clawdh's subcommands. It's the only package that
// knows about os.Args, exit codes, and signal handling — every
// subcommand is a thin adapter onto internal/httpserver,
// internal/service, internal/accounts, and internal/shellrc.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"clawdh/internal/buildinfo"
	"clawdh/internal/switching"
)

// Run executes the subcommand named by args[0] and returns a process
// exit code.
func Run(args []string) int {
	if len(args) == 0 {
		// A bare `clawdh` is someone asking what this is. Answer with the full
		// help on stdout, not a terse error — this is the front door.
		printUsage(os.Stdout)
		return 0
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "install":
		return cmdInstall(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	case "start":
		return cmdStart(args[1:])
	case "stop":
		return cmdStop(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "hook":
		return cmdHook(args[1:])
	case "statusline":
		return cmdStatusLine(args[1:])
	case "shared":
		return cmdShared(args[1:])
	case "list", "ls", "accounts":
		return cmdList(args[1:])
	case "join":
		return cmdJoin(args[1:])
	case "remote":
		return cmdRemote(args[1:])
	case "panel":
		return cmdPanel(args[1:])
	case "prune":
		return cmdPrune(args[1:])
	case "exec":
		return cmdExec(args[1:])
	case "editor", "vscode", "code":
		return cmdEditor(args[1:])
	case "version", "--version", "-v":
		fmt.Println(buildinfo.Version)
		return 0
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return 0
	default:
		// Bare `clawdh <account>` is shorthand for `clawdh run <account>`: it
		// starts a switchable session for that account. Only a name that
		// actually resolves to an account is treated this way; anything else
		// is an unknown command.
		if list, err := loadAccounts(); err == nil {
			if _, ok := switching.ResolveAccount(list, args[0]); ok {
				return cmdRun(args)
			}
		}
		// An editor calls its wrapper as `<wrapper> <real-claude-binary>
		// [args...]`, with no subcommand to put `exec` in front of — the
		// setting is one executable path and nothing else. So a first argument
		// that is an executable file, rather than a word, IS that invocation.
		// Without this the Claude Code extension got clawdh's usage text on
		// stderr and exit 1, which it reports as "Claude Code process exited
		// with code 1" and no chat at all.
		if isExecutablePath(args[0]) {
			return cmdExec(args)
		}
		// Almost everything that reaches here is a mistyped or removed ACCOUNT,
		// not a mistyped subcommand — `clawdh <account>` is the command people
		// type all day. Answering it with "unknown command" and the full usage
		// text buried the one fact that helps, so say which accounts exist and
		// leave the usage to `clawdh help`.
		if list, err := loadAccounts(); err == nil {
			fmt.Fprintf(os.Stderr, "clawdh: no account called %q. Accounts on this machine: %s.\n", args[0], accountNames(list))
			fmt.Fprintln(os.Stderr, "      Add one at the clawdh web UI, or run `clawdh help` for the list of commands.")
			return 1
		}
		fmt.Fprintf(os.Stderr, "clawdh: unknown command %q\n\n", args[0])
		printUsage(os.Stderr)
		return 1
	}
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `clawdh — run and share Claude Code accounts

EVERYDAY
  clawdh list                  Every account you can run here, and the command for each
  clawdh <name> [args...]      Run Claude as one of your accounts (args go to claude)
  clawdh shared <name> [args]  Run Claude on an account someone shared with you
  clawdh status                Is the clawdh service running, and on what URL

ACCOUNTS live on the web page clawdh opens — add, connect, and remove them there:
  clawdh install [--port N]    Start clawdh at login and open the page (do this once)
  clawdh start | stop          Start or stop the clawdh service now
  clawdh editor [shared] [name] Point VS Code / Cursor at an account or a share (blank: show which)
  clawdh prune [--yes] [id...] Reclaim disk from old accounts (previews unless --yes)

SHARING one account with other people (needs a panel + gateway):
  clawdh join <invite-link>    Connect this machine to a panel from an invite link
  clawdh remote [on|off]       Let the panel ask this machine to diagnose itself (off by default)
  clawdh panel <command>       Run or manage the panel — see `+"`clawdh panel help`"+`

OTHER
  clawdh uninstall             Remove the service, autostart, shell aliases, and the status-line badge
  clawdh serve [--port N]      Run the server in the foreground (what the service runs)
  clawdh statusline            What Claude Code's status line runs in a clawdh session (your line + the badge)
  clawdh version               Print the version

The web page is where accounts are managed; the commands above are the shortcuts.
`)
}

// isExecutablePath reports whether arg names a program on disk rather than a
// clawdh subcommand or an account. Subcommands are single words and account names
// are slugs, so neither ever contains a path separator; the file also has to
// exist and be executable, which keeps a stray argument from being run.
func isExecutablePath(arg string) bool {
	if arg == "" || !strings.ContainsRune(arg, filepath.Separator) {
		return false
	}
	info, err := os.Stat(arg)
	if err != nil || info.IsDir() {
		return false
	}
	// Windows has no executable bit; there the extension is the only thing
	// passing a path, and the extension only passes its own binary.
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
