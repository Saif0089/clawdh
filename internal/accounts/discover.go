package accounts

import (
	"encoding/json"
	"os"
	"time"
)

// DiscoveredLogin is a Claude login found on this machine, identified by the
// email it belongs to — so a person picks a real account instead of typing a
// name that has to match. ConfigDir is where its credential lives (empty for
// the default ~/.claude login), for capturing it later.
type DiscoveredLogin struct {
	Email     string `json:"email"`
	Plan      string `json:"plan"`
	ConfigDir string `json:"configDir"`
	IsDefault bool   `json:"isDefault"`
	// EndsAt is when this sign-in runs out (zero: the credential carries no
	// date). A Claude sign-in lasts about four weeks and refreshing never
	// extends it; the page compares it with the panel's before handing it up,
	// because handing up the same sign-in again changes nothing.
	EndsAt time.Time `json:"endsAt,omitzero"`
}

// DiscoverLogins finds every authenticated Claude login on this machine: the
// default ~/.claude one and each clawdh-managed account, whether the credential
// lives in a file or the macOS Keychain. Each is paired with the email and plan
// Claude stored alongside it.
//
// Entries are keyed by where the credential lives, NOT by email: the same email
// can legitimately be signed into more than one account — your own login in the
// default ~/.claude and again in a named managed account — and each must be
// discoverable by its own config dir, because that is how the page pairs a login
// with the account it belongs to. De-duping by email silently dropped the second
// one, so an account signed in with your own email looked as if it had no login
// (its "add to panel" stayed disabled and no usage showed for it).
func DiscoverLogins(list []Account) []DiscoveredLogin {
	var out []DiscoveredLogin
	seen := map[string]bool{}

	add := func(configDir string, isDefault bool) {
		raw, err := CaptureLogin(configDir)
		if err != nil {
			return // no login here
		}
		if seen[configDir] {
			return
		}
		seen[configDir] = true
		email, plan := identityFor(configDir, raw)
		out = append(out, DiscoveredLogin{Email: email, Plan: plan, ConfigDir: configDir, IsDefault: isDefault, EndsAt: signInEnd(raw)})
	}

	add("", true) // the default ~/.claude login
	for _, a := range list {
		if a.ConfigDir != "" {
			add(a.ConfigDir, false)
		}
	}
	return out
}

// identityFor reads the email (from the oauthAccount Claude writes) and the plan
// (from the captured credential). The email lives in ~/.claude.json for the
// default login and in <configDir>/.claude.json for a managed one.
func identityFor(configDir string, cred []byte) (email, plan string) {
	// The default login's identity lives in ~/.claude.json (in the home dir); a
	// managed account's lives in <configDir>/.claude.json.
	identityDir := configDir
	if identityDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			identityDir = home
		}
	}
	if oa, err := ReadOAuthAccount(identityDir); err == nil && oa != nil {
		if e, ok := oa["emailAddress"].(string); ok {
			email = e
		}
	}
	var c struct {
		O struct {
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(cred, &c) == nil {
		plan = c.O.SubscriptionType
	}
	return email, plan
}

// signInEnd reads when a captured credential's refresh token runs out. Only the
// date leaves this function, never a token.
func signInEnd(cred []byte) time.Time {
	var c struct {
		O struct {
			RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(cred, &c) != nil || c.O.RefreshTokenExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.O.RefreshTokenExpiresAt)
}
