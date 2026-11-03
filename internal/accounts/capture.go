package accounts

import (
	"clawdh/internal/winproc"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// errNoLogin means the account has no Claude login anywhere clawdh can read —
// neither a credentials file nor the macOS Keychain. Usually it just was never
// signed in.
var errNoLogin = errors.New("no Claude login is stored for this account")

// CaptureLogin reads an account's Claude login so it can be handed to the panel.
//
// This READS a credential; it never writes one. That distinction is the whole
// lesson of this project: writing a credential store is what destroyed real
// logins (the 4 KB truncation), and clawdh does not do it. Reading one — the same
// thing `security find-generic-password` or a plain `cat` does — is safe, and it
// is the only way to get a login off a machine and into the panel, because on
// macOS Claude Code keeps it in the Keychain, not a file.
//
// configDir is the account's directory, or "" for the default login. A file is
// tried first (Linux, Windows, and macOS when the Keychain is not in use); the
// Keychain is the fallback, read-only.
func CaptureLogin(configDir string) ([]byte, error) {
	path := credentialsFilePath(configDir)
	if raw, err := os.ReadFile(path); err == nil && hasClaudeLogin(raw) {
		return raw, nil
	}
	if runtime.GOOS == "darwin" {
		if raw, err := readKeychainItem(keychainItemName(configDir)); err == nil && hasClaudeLogin(raw) {
			return raw, nil
		}
	}
	return nil, errNoLogin
}

// credentialsFilePath is where Claude Code writes the credentials file when it
// is not using a keychain.
func credentialsFilePath(configDir string) string {
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ".credentials.json"
		}
		return filepath.Join(home, ".claude", ".credentials.json")
	}
	return filepath.Join(configDir, ".credentials.json")
}

// keychainItemName is the macOS Keychain service name Claude Code derives from a
// config directory: the bare name for the default login, and a name suffixed
// with the first eight hex of sha256(dir) for every managed account. This is
// Claude Code's own derivation, verified against a real install; a miss just
// means "no login found", never a wrong one.
func keychainItemName(configDir string) string {
	const base = "Claude Code-credentials"
	if configDir == "" {
		return base
	}
	sum := sha256.Sum256([]byte(configDir))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// readKeychainItem shells out to /usr/bin/security to READ one item. It is a
// read: find-generic-password with -w prints the secret. Nothing here writes.
// Going through the same tool a person would use by hand inherits that tool's
// access to an item Claude Code created, rather than prompting under clawdh's own
// unfamiliar name.
func readKeychainItem(service string) ([]byte, error) {
	out, err := winproc.Command("/usr/bin/security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("reading this account's login from the Keychain: %w", err)
	}
	return out, nil
}

// hasClaudeLogin reports whether raw is a credentials blob with an access token.
func hasClaudeLogin(raw []byte) bool {
	var stored struct {
		ClaudeAIOAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	return json.Unmarshal(raw, &stored) == nil && stored.ClaudeAIOAuth.AccessToken != ""
}

// RemoveLogin deletes an account's login from this machine, wherever it lives:
// the credentials file, and on macOS the Keychain item too. This is what makes
// revocation real. A lent login starts as a file, but Claude Code migrates it
// into the Keychain on its first refresh, so deleting only the file would leave
// the real credential behind after the member had used the account once.
//
// The Keychain delete is targeted at the one item for this account, by the name
// Claude Code derives. It is a delete of a specific credential clawdh is giving
// back, not a write of credential data — the write path is what corrupted real
// logins, and it stays gone. Best-effort: a missing item is success.
func RemoveLogin(configDir string) {
	_ = os.Remove(credentialsFilePath(configDir))
	if runtime.GOOS == "darwin" {
		// delete-generic-password removes exactly the named item; nothing is
		// written. Ignore "not found" — that is the desired end state anyway.
		_ = winproc.Command("/usr/bin/security", "delete-generic-password", "-s", keychainItemName(configDir)).Run()
	}
}
