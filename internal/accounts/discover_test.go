package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLogin plants a credential (what CaptureLogin reads) and an identity (what
// the email is read from) for a config dir, so DiscoverLogins finds a login there.
func writeLogin(t *testing.T, credDir, identityDir, email string) {
	t.Helper()
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := `{"claudeAiOauth":{"accessToken":"tok-` + email + `","subscriptionType":"max"}}`
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	if err := os.WriteFile(filepath.Join(identityDir, ".claude.json"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A managed account signed in with the SAME email as the default login must still
// be discovered — keyed by its own config dir — so the page can pair it with the
// right account. De-duping by email dropped it, leaving the account looking
// login-less: "add to panel" disabled and no usage shown.
func TestDiscoverLoginsKeepsSharedEmailAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir on unix
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	// Default ~/.claude login with your own email (credential in ~/.claude,
	// identity in ~/.claude.json).
	writeLogin(t, filepath.Join(home, ".claude"), home, "me@dev.co")
	// A managed account signed in with the SAME email, in its own config dir.
	mine := filepath.Join(t.TempDir(), "accounts", "mine")
	writeLogin(t, mine, mine, "me@dev.co")
	// A managed account with a different email.
	other := filepath.Join(t.TempDir(), "accounts", "other")
	writeLogin(t, other, other, "other@dev.co")

	byDir := map[string]DiscoveredLogin{}
	for _, l := range DiscoverLogins([]Account{{ConfigDir: mine}, {ConfigDir: other}}) {
		byDir[l.ConfigDir] = l
	}

	if l, ok := byDir[""]; !ok || l.Email != "me@dev.co" || !l.IsDefault {
		t.Errorf("default login = %+v (ok=%v), want me@dev.co and IsDefault", l, ok)
	}
	// The regression: sharing the default's email must not drop this account.
	if l, ok := byDir[mine]; !ok || l.Email != "me@dev.co" {
		t.Errorf("managed account sharing the default email was dropped: %+v (ok=%v)", l, ok)
	}
	if l, ok := byDir[other]; !ok || l.Email != "other@dev.co" {
		t.Errorf("other managed account = %+v (ok=%v)", l, ok)
	}
}

// A sign-in's end date is read from the captured credential so the page can
// compare it with the panel's; only the date leaves, never a token.
func TestSignInEndIsReadFromTheCredential(t *testing.T) {
	ends := time.Date(2026, 11, 23, 10, 52, 0, 0, time.UTC)
	cred := []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"secret","refreshToken":"secret2","refreshTokenExpiresAt":%d}}`, ends.UnixMilli()))
	if got := signInEnd(cred); !got.Equal(ends) {
		t.Errorf("signInEnd = %v, want %v", got, ends)
	}
	if got := signInEnd([]byte(`{"claudeAiOauth":{"accessToken":"a"}}`)); !got.IsZero() {
		t.Errorf("a credential with no date gave %v, want zero", got)
	}
	if got := signInEnd([]byte("not json")); !got.IsZero() {
		t.Errorf("garbage gave %v, want zero", got)
	}
}
