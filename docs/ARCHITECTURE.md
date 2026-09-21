# Architecture

clawdh is a single Go binary (`cmd/clawdh`) with no runtime dependencies and no
cgo, so it cross-compiles trivially for darwin/linux/windows ×
amd64/arm64 (`CGO_ENABLED=0`).

## Packages

- **`internal/accounts`** — the source of truth for account metadata. Each
  account is a row in `~/.clawdh/accounts.json` plus a directory under
  `~/.clawdh/accounts/<slug>/` used as that account's `CLAUDE_CONFIG_DIR`.
  `Prober` checks whether a directory already holds a successful login: it
  looks for a `.credentials.json` file first, and falls back to a short
  headless `claude -p ... --max-turns 1` probe for the case where a backend
  (e.g. the macOS Keychain) doesn't write a file — see the note on Claude
  Code's credential storage below.

- **`internal/ptyio`** — starts a child process attached to a pseudo
  terminal. `claude`'s interactive login flow needs a real TTY to render (it
  hangs on a plain pipe), so this is unavoidable. Unix and Windows need
  genuinely different OS APIs — a real pty (`github.com/creack/pty`) vs the
  ConPTY API (`github.com/UserExistsError/conpty`) — so this is the one
  package where that split lives; everything else only sees the `Session`
  interface.

- **`internal/claudebin`** — finds the `claude` executable. Started at login
  by launchd/systemd/a Startup entry, clawdh has almost no `PATH` (launchd
  hands out roughly `/usr/bin:/bin:/usr/sbin:/sbin`) while `claude` lives
  under the user's home, so `exec.LookPath` alone works from a terminal and
  fails after every reboot. Checks `CLAWDH_CLAUDE_BIN`, then `PATH`, then the
  known install locations.

- **`internal/ptyauth`** — drives one login attempt: spawns
  `claude auth login --claudeai` via `ptyio`, parses its output into a
  virtual terminal screen (`github.com/hinshun/vt10x` — needed because
  `claude`'s TUI repaints/clears lines rather than printing plain scrolling
  text), regex-scans the rendered screen for the OAuth URL, and polls
  `accounts.Prober` for completion. Emits a small `Event` stream:
  `url` → `linked`/`failed`/`timeout`, and can type a pasted
  authorization code back into the waiting process.

  Three things here were learned the hard way, by running the real CLI
  (`internal/ptyauth/realclaude_test.go` is the probe that found them):

  1. **`auth login --claudeai`, not bare `claude`.** A bare `claude` in a
     fresh `CLAUDE_CONFIG_DIR` opens the interactive first-run flow — a
     theme picker, then a login-method picker — and waits for keystrokes.
     Nothing ever prints a URL, so the login just hangs.
  2. **A very wide pseudo-terminal.** The OAuth URL is ~450–600 characters;
     at a normal 80/120-column width `claude` hard-wraps it across five
     screen lines and a scraper gets a truncated, broken URL.
  3. **The `Event` JSON tags are load-bearing.** They are the wire format
     the browser reads; without them Go emits `Type`/`URL` while the UI
     reads `type`/`url`, and every field is `undefined`.

- **`internal/shellrc`** — maintains one idempotent, clearly delimited block
  (`# >>> clawdh accounts >>> ... <<< clawdh accounts <<<`) inside each shell's rc
  file, never touching anything outside it. One alias per account, e.g.
  `alias claude-work='CLAUDE_CONFIG_DIR="..." claude'`. Covers bash, zsh,
  fish, and PowerShell (Core, on every OS, plus Windows PowerShell's default
  profile path).

- **`internal/termlauncher`** — opens a new terminal window already scoped
  to one account, per OS (`osascript`/Terminal.app, a
  `gnome-terminal`/`konsole`/`xterm` fallback chain, `wt.exe`/PowerShell).

- **`internal/service`** — per-user autostart registration and process
  lifecycle. Only autostart-artifact registration differs per OS (a
  LaunchAgent plist, a systemd `--user` unit or XDG autostart `.desktop`
  entry, or a Startup-folder script / non-elevated Scheduled Task); `Start`,
  `Stop`, and `IsRunning` are identical everywhere (`generic.go`), built on a
  pidfile plus a check that the HTTP server actually answers on its recorded
  port. None of the OS service managers are configured to supervise/restart
  clawdh — they're used only to start it once at login — so there's no risk of
  a service manager silently reviving a process this package just stopped.

- **`internal/usage`** — reads an account's stored OAuth record and reports
  what the page shows next to it: plan limits with their reset times, how
  long the login lasts, and the account's live state
  (`linked`/`expired`/`signed-out`/`unknown`). Results are cached for 60s per
  account. Access tokens are used to call the API and are never logged or
  sent to the browser — a test asserts the response body doesn't contain
  one.

- **`internal/panel`** — the account-lending server, and the client half that
  answers to it. Flat by design: one admin, no teams, no roles. It keeps its
  state in one JSON file rather than a database — tens of rows, one writer, and
  clawdh already stores accounts this way — and holds the invariant that matters
  (an account is with at most one person) by keeping the mutex across
  read-decide-write, which for a single writer is what a unique index would buy.
  Logins it lends are sealed with a key beside the file, so a copy of the file
  alone is not a working set of credentials. A machine asks every thirty
  seconds what it is entitled to and is answered with a complete list, so
  anything it holds and is not told about is given back: nothing has to reach a
  machine to take an account away from it.

  The panel keeps its whole state as one blob behind a small `Backend`
  interface: a JSON file for `clawdh panel serve` on one machine, and Postgres
  (`panelpg`, a separate package so its driver never links into the
  client binary) for a panel hosted as several instances at once. The one-holder
  rule that a single writer gets from a mutex, several writers get from a
  compare-and-swap on a version column — the losing writer re-runs its decision
  against the winner's result, so two people are never handed the same account.
  Admin sessions are a cookie sealed with the panel key rather than anything held
  in memory, for the same reason: any instance can check it.

- **`internal/httpserver`** — the REST + SSE API and the embedded web UI
  (`internal/httpserver/webui`, plain HTML/CSS/JS via `embed.FS`, no build
  step). Binds `127.0.0.1` only.

- **`internal/cli`** — subcommand wiring (`install`, `uninstall`, `start`,
  `stop`, `status`, `serve`, `version`). The only package that touches
  `os.Args`, exit codes, or signal handling.

## `~/.clawdh/accounts.json` is a contract, not an internal file

clawdh is the only thing on a machine that knows how many Claude accounts
exist and where each one lives, so other tools read its store to find out —
the Claude usage monitor parses it to attribute usage per account. That
makes the JSON field names an external interface even though nothing in Go
enforces it: a rename in `accounts.Account` compiles, passes every other
test, and silently sends another tool's numbers to the wrong account.

What a reader can rely on: a top-level `accounts` array, each entry with
`id`, `name`, `slug`, `kind`, `configDir`, `isolation`, `alias`, `status`,
`createdAt` and `lastUsedAt`. `slug` is the stable short id — a rename
changes `name` and `alias`, never `slug`, `id`, or `configDir`.
`configDir` is the account's absolute private directory: the value clawdh
exports to scope that account, and the exact string hashed into its
credential store's name. It is empty for exactly one row: the
`kind: "default"` account, which is reached by *removing* the variable
rather than setting it, so its data is in the user's `~/.claude` instead.
New fields may appear — a reader should ignore what it doesn't recognise —
but the ones above don't move.

`isolation` says what `configDir` actually contains, and a reader that
attributes usage per account **must** branch on it:

- `"config-dir"` — the original scheme. clawdh exports `CLAUDE_CONFIG_DIR`,
  so the directory holds that account's own `.claude.json` *and* its
  `projects/` transcripts. **An absent or unrecognised value means this**,
  which is how a file written by an older clawdh still reads correctly.
- `"credentials-only"` — what clawdh writes for a managed account today.
  clawdh exports `CLAUDE_SECURESTORAGE_CONFIG_DIR` and leaves
  `CLAUDE_CONFIG_DIR` unset, so only the login lives in `configDir`; the
  sessions, MCP servers, skills, plugins, hooks and `projects/`
  transcripts all come from the user's shared `~/.claude`. Scanning
  `configDir` for transcripts reports zero usage for a busy account —
  they are pooled under `~/.claude/projects` with every other
  credentials-only account's, and a transcript carries no account tag of
  its own.

`configDir` still holds that account's `.claude.json` under either scheme,
because clawdh runs `claude auth login` with both variables set: the
credential lands in the per-account store while the CLI writes
`oauthAccount` into the account's private directory, where it has always
been. That is deliberate — it keeps identity discovery working unchanged
for readers that have not been updated, including ones that cannot be.
`internal/accounts/contract_test.go` asserts all of that against the bytes
the store actually writes, and says in its failure message why it exists, so
whoever renames a field finds out here rather than from a bug report.

## Why `claude auth status`, and no Keychain code at all

Claude Code's credential storage is file-based on Linux and Windows, and on
macOS may additionally use the Keychain, keyed off the config directory so
that two accounts never collide. clawdh does not reimplement any of that. It
asks the CLI: `claude auth status --json` prints `{"loggedIn": true|false, …}`
for whatever config directory it is given. That is correct regardless of which
backend a given OS or version uses, costs nothing (a local check, unlike the
`claude -p ping` this once did, which spent real tokens on every poll), and
keeps `CGO_ENABLED=0` viable everywhere.

`internal/usage` was once the exception, because reporting plan usage means
reading the access token rather than asking whether one works. It derived the
Keychain item name — `Claude Code-credentials-<first 8 hex of sha256(configDir)>`
— and read it through `/usr/bin/security`.

That exception is gone, along with the `internal/credstore` package that wrote
the same items. Both existed to support switching an account in place, and the
write half destroyed two real logins: `security -i` truncates its input at 4095
bytes, a credential store passes that size once MCP logins are in it, and the
truncated prefix was *still* a valid `add-generic-password`, so the item was
replaced with a fragment. Reads preferred the Keychain over the file, so the
fragment shadowed the good copy, and a mirror then wrote it over the account's
own store.

So the rule now is flat, and `test/guard` enforces it against the whole tree:
**clawdh never reads or writes an OS keychain, and never writes a credential
store of any kind.** `internal/usage` reads `<configDir>/.credentials.json`
and nothing else; where Claude Code has put its credentials in the Keychain
instead, usage reports itself unavailable, which is true rather than a guess.
`internal/accounts` corrects a stale `linked` status by probing with
`claude auth status`, cached for 30s and refreshed in the background so the
HTTP layer never waits on a subprocess (`internal/accounts/loginstate.go`).

Two traps are still worth writing down, because `EnvForConfigDir` still has to
get them right. The CLI branches on whether the variable is **present**, not on
what it holds, so `CLAUDE_SECURESTORAGE_CONFIG_DIR=""` resolves to the bare
`Claude Code-credentials` — the user's real default login — and the next token
refresh would rotate that login's single-use refresh token. clawdh therefore
never emits either name with an empty value; the default account is reached by
removing both. And the two branches normalise differently — the securestorage
path is NFC-normalised, the config-dir path is hashed raw — so a non-ASCII
account directory would resolve to two different items if login and sessions
took different branches. That is why `EnvForConfigDir` sets both variables.

That single-use refresh token is also why a login handed to the gateway dies
on the machine it came from. The gateway refreshes the login it holds
(`cmd/clawdh-server/db.go`, `managerFor`/`persist`), which rotates the token;
the local copy fails its next refresh with "OAuth refresh token is no longer
valid" and Claude Code empties the store. Nothing in clawdh writes that
store, so nothing can put it back: the person reconnects the account on the
page, or runs the login through its share. Every place a local account is
chosen — `clawdh <name>`, a switch staged in a session or by the hook, the
supervisor's own resolve, an editor's default — therefore checks the login is
still there first (`internal/cli/login_check.go`) and names the share that now
runs it, matched by the email the panel sends with each share. The check is a
read, like `DiscoverLogins`; on a Mac it is the same `security
find-generic-password` read.

The bundle is plain-text JS inside the binary. Grep it with `/usr/bin/grep -a`
or python — a shell whose `grep` is aliased to `ugrep` fails on a bounded
`{0,240}` window and prints nothing, which reads as "not found".

## Testing strategy

Four layers, because each one has a blind spot that let a real bug through:

1. **Unit tests** per package.
2. **`test/e2e`** drives the real built `clawdh` binary through
   install → add account → log in → uninstall against `testdata/fakeclaude`,
   on all three OSes.
3. **`test/browser`** loads the actual web UI in Chromium and WebKit
   (Playwright) against a real server. This layer exists because layers 1–2
   drive the HTTP API directly and never load the page — so a UI that was
   completely broken in every browser (an empty `202` body parsed as JSON,
   and SSE field names that didn't match what `app.js` read) passed CI while
   failing on every real machine.
4. **Manual verification** — [`docs/MANUAL_VERIFICATION.md`](MANUAL_VERIFICATION.md)
   — for the one thing none of the above can do: a real Anthropic login.

`testdata/fakeclaude` deliberately mirrors the *real* CLI's contract as
verified by `internal/ptyauth/realclaude_test.go` (a manual probe, run with
`CLAWDH_REAL_CLAUDE=1`): it implements `auth status --json` and
`auth login --claudeai`, prints a ~600-character URL so truncation is
caught, offers a paste-a-code path, and — importantly — emulates the
interactive theme picker for a bare `claude`, so that if clawdh ever goes back
to spawning bare `claude` the tests hang exactly the way a real machine did.
