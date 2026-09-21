# clawdh — Claude Code Account Manager

Manage and use multiple [Claude Code](https://claude.com/claude-code) (`claude`
CLI) accounts on one machine, from a local web UI backed by a small
background service. No admin/root/elevation required for a normal install,
on Windows, macOS, or Linux, any architecture — and when a privileged
location genuinely is involved, it asks rather than failing.

Each account gets its own directory, exported as `CLAUDE_SECURESTORAGE_CONFIG_DIR`,
which scopes **the login and nothing else**: `CLAUDE_CONFIG_DIR` is left unset, so
every account shares your own `~/.claude` and keeps your sessions, MCP servers,
skills, plugins, hooks and `CLAUDE.md`. Switching account does not switch your
setup, and `--continue` picks up the conversation you were already in.

This started as an automation of the technique from
["Setting Up Multiple Claude Code Accounts on Your Local Machine"](https://medium.com/@buwanekasumanasekara/setting-up-multiple-claude-code-accounts-on-your-local-machine-f8769a36d1b1),
which gave each account its own `CLAUDE_CONFIG_DIR` and isolated everything with
it. Claude Code derives its credential store from either variable by the same
hash, so moving to the narrower one keeps every existing login working. clawdh:

- lets you add a new account by logging in right from the browser (no manual
  `CLAUDE_CONFIG_DIR=... claude` typing),
- runs any account with one command — `clawdh <name>` — on every OS, with no shell
  aliases to install, keep in sync, or get wrong (`clawdh list` shows them all),
- switches the account a running session is on: type `clawdh <name>` (or
  `clawdh shared <name>` for an account shared with you) at the prompt, or run
  `!clawdh <name>` as a shell command, and the session comes back on the other
  account **with the conversation resumed**. It is a genuine relaunch, so the
  conversation survives but anything running inside the old process —
  subagents, workflows, background tasks — does not.

  This used to happen in place, with nothing restarted: clawdh gave each session a
  private copy of the login and a switch rewrote it. Keeping those copies meant
  clawdh had to *write* Claude Code's credential store, and that is what destroyed
  two real logins — a store over 4 KB was truncated into a valid-looking
  fragment and copied over the account's own. clawdh now reads logins and never
  writes them, and pays for it with the restart.

  Switching works in a session you started with `clawdh <account>` *and* in one you
  started by just typing `claude`: clawdh puts a small `claude` function in your
  shell rc that runs the same supervisor. It steps aside — running Claude Code
  directly — inside an existing session, with no terminal (scripts, pipes, CI),
  when clawdh is not on PATH, or with `CLAWDH_WRAP=0` set.
- runs as a per-user background service that starts at login and serves the
  UI at `http://127.0.0.1:47932`.

Switching accounts is always per-terminal (via its alias), never a global
"current account" setting — you can have several accounts' terminals open
side by side.

## Install

**macOS / Linux:**

```sh
curl -fsSL https://raw.githubusercontent.com/Saif0089/clawdh/main/install.sh | sh
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/Saif0089/clawdh/main/install.ps1 | iex
```

Both scripts install a single binary to a per-user directory (`~/.local/bin`
or `%LOCALAPPDATA%\clawdh\bin`), register it to start at login, start it, and
print the URL to open. Nothing is written outside your own user profile —
no `sudo`, no `/usr/local`, no `Program Files`, no `HKLM`.

Prerequisite: the [`claude` CLI](https://claude.com/claude-code) itself must
already be installed and on `PATH` — clawdh manages *accounts* for it, it
doesn't install Claude Code.

## Using it

Open `http://127.0.0.1:47932`. Click **Sign in another account**, give it a
name, and open the URL it shows you to finish logging in — the account flips to
"linked" automatically once you do. Each account's card shows the command that
runs it (`clawdh <name>`); type it in any terminal, or copy it from the card.

Accounts are no longer isolated from each other beyond their login, and clawdh
never writes a credential store — it reads `<account dir>/.credentials.json`
and otherwise asks `claude auth status` what it thinks. On a machine where
Claude Code keeps its credentials in the macOS Keychain there is no file to
read, so those cards say plan usage is unavailable rather than guessing.

Each card also shows that account's plan usage — the same numbers as
`/usage` inside Claude Code — with a live countdown to each reset, and two
separate clocks for the login itself: how long you stay signed in (weeks),
and the short-lived access token that Claude Code renews on its own (hours).
The dot beside the name is checked live rather than remembered: `linked`,
`login expired`, `signed out`, or `unknown` when Anthropic can't be reached.

The page keeps itself current — it re-reads every few seconds, so there is
no refresh button to press and nothing to reload. Those reads are answered
by clawdh itself; it asks Anthropic for fresh numbers at most once a minute
per account. That endpoint is not a documented API and publishes no rate
limit, so when it does refuse, clawdh waits — a minute, then two, four,
eight, up to fifteen — and keeps showing the last numbers it read rather
than emptying the card. Those numbers are kept in `~/.clawdh/usage.json`
(percentages and reset times, never a credential), so a restart in the
middle of a rate limit still has something true to show; anything older
than six hours is discarded, because by then the shortest window on the
page has rolled over. The build answering on
that port is named in the top-right corner, which is how you tell a fix
that shipped from a fix that is actually running.

## Sharing accounts with other people

clawdh on its own is a single-machine tool. To let *other* people use one of your
accounts, clawdh has two more pieces: a small self-hosted **panel** where you add
logins and give people access, and a **gateway** that holds the subscription and
serves everyone through it. One login can serve many people at once — the point
the whole design turns on — because the gateway refreshes the token centrally, so
no one else ever holds the login and two machines never invalidate each other.

The everyday flow lives in the web page, no terminal required:

- On the machine where an account is signed in, open clawdh and choose **Add to
  panel** on that account. Its login is sealed and stored on the panel.
- On the panel, **Invite someone** — they get a link that expires in about an
  hour. They open it, connect in a click, and whatever you share appears on their
  machine, ready to run as `clawdh shared <name>`.
- **Give access** shares an account with a person; the ⨯ next to their name takes
  it back. Access stops within seconds — the gateway simply stops honouring their
  key.

The same actions exist on the command line for anyone who prefers it:

```sh
clawdh panel serve                          # run the panel (or host it — see below)
clawdh panel push work http://host:47933    # add an account's login to the panel
clawdh join <invite-link>                   # connect a machine from an invite link
clawdh shared work                          # run an account shared with you
```

The panel deploys to Vercel as one serverless function with its state in
Postgres, and the gateway runs on a small VPS — see
[docs/DEPLOY_VERCEL.md](docs/DEPLOY_VERCEL.md) and [deploy/](deploy/). Every
machine runs the ordinary clawdh client, which self-updates on each release.

What this does and does not do, plainly. Members never hold the Claude login —
it stays sealed on the panel and is only ever used by the gateway — so there is
no copy on a member's disk to leak, and taking access away is immediate and
total. A member does hold a scoped gateway key (in `~/.clawdh/shares.json`, 0600);
revoking their share makes it stop working on the next request. The panel binds
`127.0.0.1` unless you give it `--addr`, and both the panel and the gateway
should be behind TLS.

## VS Code, Cursor, and the rest

Nothing to run. If an editor has the Claude Code extension installed, clawdh
configures it by setting the extension's `claudeCode.claudeProcessWrapper` to
clawdh, so clawdh launches Claude on the extension's behalf. You open the editor as
usual and start a chat as usual.

What that buys is the same thing the terminal gets: **every chat is a
supervised session of its own**, so typing `clawdh <name>` — or `clawdh shared
<name>` — in one chat restarts that chat on the other account with the
conversation resumed, and leaves every other chat and every terminal where it
was. As in a terminal, anything running inside the chat at that moment does not
survive the restart. New chats start on the account `clawdh editor <name>` last
set — one of your logins, or a share (`clawdh editor shared <name>` when a login
and a share go by the same name) — or your default login if it was never set;
`clawdh editor` on its own says what each editor is set to. Chats already open
keep the account they started with until you switch or restart them.

Editors without the extension are left alone, `settings.json` keeps its comments
and formatting (one value is edited in place), and `CLAWDH_MANAGE_EDITORS=0` in the
service's environment turns the whole thing off.

Two things the wrapper changes, both from the extension's own code: it stops
doing its update check while a wrapper is configured, and a conversation started
with no explicit permission mode gets `default` rather than the mode the CLI
would have resolved. If you set `claudeCode.initialPermissionMode`, that is
passed through and unaffected.

## Where the credentials live

Claude Code keeps an account's login in the macOS Keychain, in the Windows
Credential Manager where one is available, and in a `.credentials.json` file
otherwise — and clawdh reads whichever it finds. The stores clawdh writes for
sessions and editors follow the same rule, with one addition: a keychain it
cannot write falls back to the file, which is what Claude Code itself does with
the same pair. After writing, clawdh reads the store back; if the write did not
land where Claude Code will look for it, the switch is reported as failed and
the session is relaunched instead.

How quickly a switch shows up depends on which of those the session is reading,
and the difference is worth knowing: Claude Code re-reads the credentials *file*
on every request, so a switch lands on the next one, but it caches Keychain
reads for thirty seconds, so on macOS the session may answer once or twice more
as the old account before it flips. clawdh says so when it switches.

Set `CLAWDH_CREDENTIALS_FILE=1` to keep clawdh out of the Keychain entirely and use
the file store everywhere — which also makes switches immediate. Claude Code reads it when its keychain item is
absent, so nothing breaks — the trade is that a copy of the token sits in a
`0600` file for as long as that session or editor exists.

## Token usage, per account

If the machine also runs the Claude usage monitor, its agent reports every
clawdh account separately: each login is its own account on the dashboard,
under the one device, rather than every account's tokens piling up in a
single number for the machine. There is nothing to configure per account
and nothing to re-run after adding one. The agent re-reads
`~/.clawdh/accounts.json` on every sync — so an account you add now starts
reporting within a sync tick, and an account you delete simply stops.

Because accounts share one `~/.claude`, their transcripts pool into a single
`projects/` tree and nothing inside a transcript records which login paid for
it. The monitor's hook closes that gap from inside the session: it runs while
the session is alive, reads the account directory it inherited, and writes
`session id → owner` to its own ledger, which is what the scanner attributes
by. Sessions that predate the ledger are credited to nobody rather than to
whichever account happens to be signed in.

That is the reason `accounts.json` is treated as a contract rather than an
internal file (see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)): renaming
one of its fields still compiles and still passes clawdh's own tests, but it
quietly sends another tool's numbers to the wrong account.

## Staying up to date

clawdh updates itself. The running service checks the published release every
five minutes, verifies the download against the checksums published beside
it, replaces its own binary and restarts into it — then says so with a
desktop notification, on macOS, Windows and Linux alike. A fix pushed to
main is running on your machine minutes later; nothing to run, nothing to
remember.

Two rules keep that safe to leave alone:

- A download whose SHA-256 doesn't match the release's `checksums.txt` is
  discarded, not installed.
- A release published *before* the binary you are running was written is
  never installed over it — so a build you made yourself from a working
  tree that is ahead of the release is left alone.

Set `CLAWDH_AUTO_UPDATE=0` in the service's environment to turn it off, and
`CLAWDH_NOTIFY=0` to keep the notifications quiet. To update by hand at any
time, re-run the install command above.

## Uninstalling

```sh
clawdh uninstall
```

Stops the service, removes the autostart registration, strips every
managed shell block, and removes the installed binary. Your accounts'
login data under `~/.clawdh/accounts` (and `~/.ccam/accounts`, if carried over
from a previous ccam install) is left in place — remove those directories
yourself for a full wipe.

## Development

```sh
go build ./...
go vet ./...
go test ./...
go test ./test/e2e/...   # full install → login → uninstall lifecycle

cd test/browser && npm install && npx playwright install chromium webkit
npx playwright test        # drives the real web UI in Chromium and WebKit
```

The browser layer is not optional cover: the Go tests drive the HTTP API
directly and never load the page, so a UI that was broken in every
browser once passed CI while failing on every real machine.

### Releasing

[`.github/workflows/pipeline.yml`](.github/workflows/pipeline.yml) does it
all: every push to `main` runs the full test + e2e suite on macOS/Linux/
Windows, and if that's green, builds all 6 targets and republishes the
rolling `latest` GitHub Release — the one `install.sh`/`install.ps1` pull
from by default. So shipping a change is just `git push`. Pushing a
`vX.Y.Z` tag instead builds the same way but creates a proper pinned
release (`CLAWDH_VERSION=vX.Y.Z` selects it in either install script). A
pull request runs test + e2e only, as a pre-merge gate.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit
together, and [docs/MANUAL_VERIFICATION.md](docs/MANUAL_VERIFICATION.md) for
the one thing CI can't cover: a real Anthropic login, once per OS, before
tagging a release.

## License

MIT — see [LICENSE](LICENSE).
