#!/usr/bin/env bash
# Real end-to-end check on Linux, inside a container.
#
# CI's Linux e2e runs on a GitHub runner, which has no systemd user bus
# and inherits the runner's full PATH — so the two things most likely to
# break for an actual Linux user (the XDG autostart fallback, and a
# service started with a login-shell PATH that lacks ~/.local/bin) never
# get exercised there. This does exercise them: it installs as an
# ordinary non-root user, checks the autostart entry that gets written,
# starts the service from that entry's own command line, drives a full
# account + login through the HTTP API, and uninstalls.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok: $*"; }

cd /src
export HOME=/home/tester
export PATH="$HOME/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

mkdir -p "$HOME/.local/bin"
# -buildvcs=false: the repo is bind-mounted and owned by another uid,
# so git refuses to report status ("dubious ownership") and Go turns
# that into a build failure. The version stamp is irrelevant here.
go build -buildvcs=false -o "$HOME/.local/bin/clawdh" ./cmd/clawdh
go build -buildvcs=false -o "$HOME/.local/bin/claude" ./testdata/fakeclaude
ok "built clawdh and the fake claude into ~/.local/bin"

PORT=47955

# --- install, as a plain user with no root ---
clawdh install --port "$PORT" > /tmp/install.log 2>&1 || { cat /tmp/install.log; fail "clawdh install"; }
cat /tmp/install.log
grep -q "clawdh is running" /tmp/install.log || fail "install did not report a running service"
ok "installed without root"

# --- the autostart entry: no systemd user bus here, so it must be XDG ---
DESKTOP="$HOME/.config/autostart/clawdh.desktop"
[ -f "$DESKTOP" ] || fail "no XDG autostart entry at $DESKTOP"
grep -q "^Exec=" "$DESKTOP" || fail "autostart entry has no Exec"
ok "XDG autostart entry written (no systemd user bus available)"

# The entry must carry a working directory, and its Exec must be quoted.
grep -q "^Path=" "$DESKTOP" || fail "autostart entry has no Path (working directory)"
ok "autostart entry carries a working directory"

# --- the service answers, and identifies itself ---
curl -fsS "http://127.0.0.1:$PORT/api/status" | grep -q '"service":"clawdh"' || fail "status does not identify as clawdh"
ok "service answers on 127.0.0.1:$PORT"

# --- full account lifecycle over the API ---
ACCOUNT=$(curl -fsS -X POST "http://127.0.0.1:$PORT/api/accounts" \
  -H 'Content-Type: application/json' -d '{"name":"Work"}')
echo "$ACCOUNT" | grep -q '"alias":"claude-work"' || fail "unexpected account payload: $ACCOUNT"
ok "account created"

# Accounts no longer get a shell alias — they run as `clawdh <name>` — so the
# managed block in a shell rc file carries only the `claude` wrapper (which
# makes a plain `claude` a switchable session) and never a per-account alias.
for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.config/fish/config.fish"; do
  if [ -f "$rc" ] && grep -q "claude-work" "$rc"; then
    fail "a shell alias was written to $rc; accounts run as 'clawdh <name>' now"
  fi
done
grep -q "clawdh run --auto" "$HOME/.bashrc" || fail "the managed block in ~/.bashrc lacks the claude wrapper"
ok "shell rc carries the claude wrapper and no per-account alias"

# --- login: URL then linked, over SSE ---
curl -fsS -X POST "http://127.0.0.1:$PORT/api/accounts/work/login" -o /dev/null
timeout 60 curl -sN "http://127.0.0.1:$PORT/api/accounts/work/login/events" > /tmp/sse.txt || true

grep -q '"type":"url"' /tmp/sse.txt || { cat /tmp/sse.txt; fail "no url event"; }
grep -q '"type":"linked"' /tmp/sse.txt || { cat /tmp/sse.txt; fail "no linked event"; }
ok "login produced url then linked events"

URL_LEN=$(python3 -c "
import json,sys
for line in open('/tmp/sse.txt'):
    if line.startswith('data: '):
        ev=json.loads(line[6:])
        if ev['type']=='url':
            print(len(ev['url'])); break
")
[ "$URL_LEN" -gt 400 ] || fail "OAuth URL truncated at $URL_LEN chars"
ok "OAuth URL arrived whole ($URL_LEN chars)"

# Onboarding must be marked so the first real claude run skips the wizard.
for _ in $(seq 1 40); do
  [ -f "$HOME/.clawdh/accounts/work/.claude.json" ] && break
  sleep 0.5
done
python3 -c "
import json
d=json.load(open('$HOME/.clawdh/accounts/work/.claude.json'))
assert d.get('hasCompletedOnboarding') is True, d
" || fail "onboarding not marked complete"
ok "onboarding marked complete"

curl -fsS "http://127.0.0.1:$PORT/api/accounts" | grep -q '"status":"linked"' || fail "account not linked"
ok "account reported linked"

# --- starting from the autostart entry's own command line ---
clawdh stop >/dev/null
EXEC_LINE=$(grep "^Exec=" "$DESKTOP" | cut -d= -f2-)
echo "autostart Exec: $EXEC_LINE"
# Strip the quotes the Desktop Entry spec requires, then run it as the
# session would, with the bare PATH a login-started process gets.
eval "env -i HOME=$HOME PATH=/usr/bin:/bin $EXEC_LINE" >/tmp/autostart.log 2>&1 &
for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:$PORT/api/status" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "http://127.0.0.1:$PORT/api/status" | grep -q '"service":"clawdh"' \
  || { cat /tmp/autostart.log; cat "$HOME/.clawdh/clawdh.log" 2>/dev/null; fail "autostart command line did not bring up the service"; }
ok "service starts from the autostart entry with a bare login PATH"

# And with that bare PATH it must still find claude, which is the whole
# point of baking PATH into the entry.
curl -fsS -X POST "http://127.0.0.1:$PORT/api/accounts/work/login" -o /dev/null
timeout 60 curl -sN "http://127.0.0.1:$PORT/api/accounts/work/login/events" > /tmp/sse2.txt || true
grep -q '"type":"url"' /tmp/sse2.txt || { cat /tmp/sse2.txt; cat "$HOME/.clawdh/clawdh.log"; fail "login failed under the autostart environment (claude not found?)"; }
ok "claude still resolves under the autostart environment"

# --- uninstall leaves nothing behind ---
clawdh uninstall > /tmp/uninstall.log 2>&1 || { cat /tmp/uninstall.log; fail "clawdh uninstall"; }
cat /tmp/uninstall.log
[ -f "$DESKTOP" ] && fail "autostart entry survived uninstall"
grep -q "Managed by clawdh" "$HOME/.bashrc" 2>/dev/null && fail "a managed shell block survived uninstall"
[ -f "$HOME/.local/bin/clawdh" ] && fail "binary survived uninstall"
[ -d "$HOME/.clawdh/accounts/work" ] || fail "account data was deleted (it should be kept)"
ok "uninstall removed the autostart entry and binary, and kept account data"

echo
echo "ALL LINUX CHECKS PASSED"
