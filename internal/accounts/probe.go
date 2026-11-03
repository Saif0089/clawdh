package accounts

import (
	"clawdh/internal/winproc"
	"context"
	"encoding/json"
	"os"
	"time"

	"clawdh/internal/claudebin"
)

// Prober reports whether an account's config directory holds a valid
// login, by asking `claude` itself: `claude auth status --json` prints
// {"loggedIn": true|false, ...} for whatever CLAUDE_CONFIG_DIR it's
// given.
//
// Asking the CLI (rather than looking for a credentials file) is the
// only approach that's correct on every OS: on macOS the credentials
// may live in the Keychain with no file on disk at all, and a file that
// does exist may hold an expired or revoked token. It's also cheap —
// `auth status` is a local check, unlike the `claude -p ping` call this
// used to make, which spent real tokens on every poll.
type Prober struct {
	// ClaudeBinary is the executable to invoke, normally resolved via
	// claudebin.Resolve. Overridable in tests.
	ClaudeBinary string
	// Timeout bounds each probe so a hung process can't block a caller.
	Timeout time.Duration
}

// NewProber returns a Prober using the "claude" binary on PATH.
func NewProber() *Prober {
	return &Prober{ClaudeBinary: "claude"}
}

type authStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
}

// IsLinked reports whether configDir is authenticated.
func (p *Prober) IsLinked(ctx context.Context, configDir string) bool {
	status, err := p.Status(ctx, configDir)
	if err != nil {
		return false
	}
	return status.LoggedIn
}

// Status runs `claude auth status --json` against configDir and returns
// the parsed result.
func (p *Prober) Status(ctx context.Context, configDir string) (authStatus, error) {
	binary := p.ClaudeBinary
	if binary == "" {
		binary = "claude"
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := claudebin.Invocation(binary, []string{"auth", "status", "--json"})
	cmd := winproc.CommandContext(ctx, name, args...)
	// An empty configDir means the default account, which is reached by
	// *removing* the variable — see EnvForConfigDir.
	cmd.Env = EnvForConfigDir(configDir)
	// Never inherit clawdh's working directory: started by launchd at
	// login that is "/", and claude treats its working directory as the
	// project directory.
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}

	out, err := cmd.Output()
	if err != nil {
		return authStatus{}, err
	}

	var status authStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return authStatus{}, err
	}
	return status, nil
}
