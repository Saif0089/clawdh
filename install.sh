#!/usr/bin/env sh
# Installs clawdh for the current user only: no sudo, no /usr/local, no
# system paths anywhere. Safe to pipe straight into sh:
#
#   curl -fsSL https://raw.githubusercontent.com/Saif0089/clawdh/main/install.sh | sh
set -eu

REPO="Saif0089/clawdh"

# An invite link makes install and join one paste: the invite page shows
#   curl ... | sh -s -- --join https://panel/i/<code>
# and the same works as CLAWDH_JOIN=<link>. clawdh joins right after it starts.
join="${CLAWDH_JOIN:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --join) shift; join="${1:-}" ;;
    --join=*) join="${1#--join=}" ;;
    *) echo "clawdh install: unknown option $1" >&2; exit 2 ;;
  esac
  [ $# -gt 0 ] && shift
done

os_name="$(uname -s)"
case "$os_name" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "clawdh: unsupported OS: $os_name (this script supports macOS and Linux; see install.ps1 for Windows)" >&2
    exit 1
    ;;
esac

arch_name="$(uname -m)"
case "$arch_name" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)
    echo "clawdh: unsupported architecture: $arch_name" >&2
    exit 1
    ;;
esac

version="${CLAWDH_VERSION:-latest}"
asset="clawdh_${os}_${arch}"
if [ "$version" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${version}/${asset}"
fi

install_dir="${CLAWDH_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$install_dir"

tmp="$(mktemp)"
sums="$(mktemp)"
trap 'rm -f "$tmp" "$sums"' EXIT

echo "Downloading clawdh ($os/$arch)..."
# --retry rides out a transient hiccup from GitHub's release CDN (a 502/504 or a
# dropped connection) rather than failing the whole install on the first blip.
curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused "$url" -o "$tmp"

# Verify against the checksums published alongside the binary. Skipped
# only if the release has none (older releases) or no sha256 tool exists.
if curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused "$(dirname "$url")/checksums.txt" -o "$sums" 2>/dev/null; then
  expected="$(grep " ${asset}\$" "$sums" | awk '{print $1}' | head -n 1)"
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "$tmp" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
    fi
    if [ -n "${actual:-}" ] && [ "$actual" != "$expected" ]; then
      echo "clawdh: checksum mismatch for ${asset}" >&2
      echo "  expected $expected" >&2
      echo "  actual   $actual" >&2
      exit 1
    fi
  fi
fi

# Stop any running clawdh before replacing the binary. Without this an
# upgrade silently keeps serving the old build: mv swaps the file, but
# the running process holds the old inode until something restarts it.
if [ -x "$install_dir/clawdh" ]; then
  "$install_dir/clawdh" stop >/dev/null 2>&1 || true
fi

chmod +x "$tmp"
mv "$tmp" "$install_dir/clawdh"
rm -f "$sums"
trap - EXIT

echo "Installed $install_dir/clawdh"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo ""
    echo "Note: $install_dir isn't on your PATH yet. Add this to your shell rc file:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    ;;
esac

# Cross over from a previous ccam install: let the old binary uninstall itself
# (it stops its service, strips its shell block, and removes itself), so it
# doesn't leave a second daemon fighting clawdh for the port. Account data is
# left in place for clawdh to import on first run. Best-effort; a machine with
# no ccam just skips it.
for old_ccam in "$HOME/.local/bin/ccam" "$install_dir/ccam"; do
  if [ -x "$old_ccam" ]; then
    echo "Removing the previous ccam install..."
    "$old_ccam" uninstall >/dev/null 2>&1 || true
  fi
done

echo ""
"$install_dir/clawdh" install

if [ -n "$join" ]; then
  echo ""
  "$install_dir/clawdh" join "$join"
fi
