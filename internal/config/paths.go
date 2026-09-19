// Package config resolves the per-user, per-OS filesystem locations clawdh
// uses. Everything lives under the user's home directory — no path here
// ever requires elevated permissions to create or write.
package config

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Env reads a clawdh environment variable by its suffix (e.g. Env("PANEL_KEY")
// reads CLAWDH_PANEL_KEY), falling back to the historical CCAM_ prefix so a
// gateway or panel still running with the old variable names keeps working until
// it is redeployed. New deployments set the CLAWDH_ names.
func Env(suffix string) string {
	if v := os.Getenv("CLAWDH_" + suffix); v != "" {
		return v
	}
	return os.Getenv("CCAM_" + suffix)
}

// DefaultPort is the port clawdh listens on unless overridden. Chosen to
// be memorable-ish and unlikely to collide with anything else already
// running on a dev machine.
const DefaultPort = 47932

// legacyDirName is the pre-clawdh base directory (~/.ccam). Its metadata is
// imported into ~/.clawdh once, on first use — see migrateFromLegacy.
const legacyDirName = ".ccam"

// baseDirName is the clawdh base directory (~/.clawdh).
const baseDirName = ".clawdh"

var (
	migMu      sync.Mutex
	migratedTo = map[string]bool{}
)

// HomeDir returns the clawdh base directory: ~/.clawdh on every OS. Using a
// single dotdir (rather than OS-specific "proper" locations) keeps the
// install/uninstall and e2e-test logic identical across platforms.
//
// The first time a given base is resolved, a previous ~/.ccam install's metadata
// (accounts, panel enrolment, shares) is imported into it. Account directories
// are deliberately left where they are: each account's CLAUDE_CONFIG_DIR is
// stored absolutely in accounts.json and, on macOS, the Keychain item holding
// its login is keyed by a hash of that path — so moving the directory would sign
// every account out. The path is recomputed from $HOME each call (never cached),
// so a test that repoints HOME sees its own directory; only the one-time
// migration per base is guarded.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, baseDirName)
	migrateOnce(filepath.Join(home, legacyDirName), base)
	return base, nil
}

// migrateOnce runs migrateFromLegacy at most once per base directory in this
// process, so the per-call HomeDir stays cheap without caching the path itself.
func migrateOnce(legacy, current string) {
	migMu.Lock()
	defer migMu.Unlock()
	if migratedTo[current] {
		return
	}
	migratedTo[current] = true
	migrateFromLegacy(legacy, current)
}

// migrateFromLegacy copies a previous ~/.ccam install's metadata files into
// ~/.clawdh, once. It is best-effort and idempotent: a file already present in
// the new directory is left untouched, a file absent from the old one is
// skipped, and the account directories the metadata points at are never moved.
// A machine with no ~/.ccam (a fresh clawdh install) does nothing.
func migrateFromLegacy(legacy, current string) {
	if legacy == current {
		return
	}
	if _, err := os.Stat(legacy); err != nil {
		return // no previous install
	}
	if err := os.MkdirAll(current, 0o700); err != nil {
		return
	}
	// Metadata only — never the accounts/ directory (see HomeDir).
	for _, name := range []string{
		"accounts.json", "panel-client.json", "shares.json", "usage.json",
		"port", "panel.json", "panel.key",
	} {
		dst := filepath.Join(current, name)
		if _, err := os.Stat(dst); err == nil {
			continue // already migrated, or written since
		}
		copyFilePreserving(filepath.Join(legacy, name), dst)
	}
}

// copyFilePreserving copies src to dst 0600, best-effort, doing nothing if src
// is absent. Both files here can hold secrets (tokens, keys), hence 0600.
func copyFilePreserving(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return
	}
	out.Close()
}

// AccountsDir returns ~/.clawdh/accounts, the parent of every per-account
// CLAUDE_CONFIG_DIR created under clawdh. Accounts carried over from a previous
// ccam install keep their original ~/.ccam/accounts/<id> paths (see HomeDir).
func AccountsDir() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "accounts"), nil
}

// AccountsFile returns the path to the account metadata store.
func AccountsFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "accounts.json"), nil
}

// UsageCacheFile returns the path the last successful plan-usage read is
// kept at, per account.
//
// It holds numbers, never credentials: percentages and reset times, the
// same things the page shows. Its whole job is that a restart — and clawdh
// restarts itself whenever it updates — does not leave a card blank
// while Anthropic is refusing to answer.
func UsageCacheFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "usage.json"), nil
}

// PanelClientFile is where this machine remembers the panel it is enrolled
// with: the server URL, this machine's device token, and the person it enrolled
// as. It holds a token, so it is written 0600.
func PanelClientFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "panel-client.json"), nil
}

// SharesFile is where this machine caches the gateway shares it was granted:
// for each shared account, the gateway URL and this person's key. It holds
// keys, so it is written 0600. `clawdh shared <slug>` reads it to run a shared
// account, and the shell aliases point here rather than baking a key into a
// dotfile.
func SharesFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "shares.json"), nil
}

// SharedSessionsFile is clawdh's own record of which conversations on this
// machine ran through a shared account, and which one: an append-only TSV of
// session id, share slug, epoch seconds. It is the whole basis for what the
// panel's remote help may see — only sessions in it are ever listed or sent —
// so a person's own sessions (their personal login, or a local account they
// manage themselves) stay invisible to the panel. No secrets.
func SharedSessionsFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "shared-sessions.tsv"), nil
}

// RemoteRequestsFile is where the service keeps remote-help requests that are
// waiting for this machine's owner to allow or deny them, plus the current
// allow window and recent decisions — so a restart forgets nothing unanswered.
func RemoteRequestsFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "remote-requests.json"), nil
}

// NoticesSeenFile is where the running service remembers which per-person
// notices (a quota warning, a broken login) it has already shown, keyed by the
// event's ID, so a standing condition riding every check-in is announced once
// rather than every thirty seconds. It holds no secrets, only short IDs.
func NoticesSeenFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "notices-seen.json"), nil
}

// LogFile returns the path clawdh's background service writes its own
// stdout/stderr to, so install issues are debuggable without a terminal.
func LogFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "clawdh.log"), nil
}

// PortFile returns the path clawdh writes its listening port to, so the CLI
// (and the installer's health check) can find a running instance without
// hardcoding a port.
func PortFile() (string, error) {
	base, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "port"), nil
}

// InstallDir returns the default per-user directory the install script
// copies the clawdh binary into. Never a system location (no /usr/local, no
// Program Files) — nothing under here ever needs admin/root to write.
func InstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(local, "clawdh", "bin"), nil
	default:
		return filepath.Join(home, ".local", "bin"), nil
	}
}

// EnsureDir creates dir (and parents) if missing, with permissions that
// only the owning user can read/write.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
