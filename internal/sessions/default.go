package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Default is the account new sessions start as: every chat an editor opens,
// and a plain `claude` in a terminal. It is not where `clawdh <name>` goes —
// that names an account outright and always wins.
//
// It used to mean editors alone, recorded by `clawdh editor <name>`, a command
// almost nobody found. One setting now covers both, so "start my work on this
// account" is one answer rather than two that could disagree.
//
// Exactly one of AccountID (one of this machine's own logins) and Shared (a
// gateway share's slug) is set. Name and ConfigDir are as they were when it
// was recorded, for anything that finds the file later; a launch never uses
// them, because the account is resolved live — so a rename, a re-login, or a
// share that has been taken back is seen rather than remembered.
type Default struct {
	AccountID string `json:"accountId,omitempty"`
	Shared    string `json:"shared,omitempty"`
	Name      string `json:"name"`
	ConfigDir string `json:"configDir,omitempty"`
}

// Set reports whether anything was chosen. An unset default means new sessions
// run as this machine's own default login, exactly as a plain `claude` would.
func (d Default) Set() bool { return d.AccountID != "" || d.Shared != "" }

// ReadDefault returns the recorded default, or the zero value when there is
// none — or it cannot be read, which for a launch is the same thing.
//
// It also adopts the file `clawdh editor <name>` wrote before this setting
// covered terminals too, so upgrading does not silently move an editor back
// onto the machine's default login.
func ReadDefault(path string) Default {
	var d Default
	data, err := os.ReadFile(path)
	if err != nil {
		if legacy := legacyPath(path); legacy != "" {
			if old, err := os.ReadFile(legacy); err == nil {
				if json.Unmarshal(old, &d) == nil && d.Set() {
					_ = WriteDefault(path, d)
					os.Remove(legacy)
				}
			}
		}
		return d
	}
	_ = json.Unmarshal(data, &d)
	return d
}

// WriteDefault records the account new sessions start as.
func WriteDefault(path string, d Default) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ClearDefault puts new sessions back on this machine's default login.
func ClearDefault(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// legacyPath is where `clawdh editor <name>` recorded its choice.
func legacyPath(path string) string {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return ""
	}
	return filepath.Join(dir, "editors", "default.json")
}
