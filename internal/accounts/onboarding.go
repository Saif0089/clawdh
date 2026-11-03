package accounts

import (
	"clawdh/internal/winproc"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"clawdh/internal/claudebin"
)

// MarkOnboarded records that Claude Code's first-run wizard is done for
// this account's config directory.
//
// Logging in and completing onboarding are separate things to Claude
// Code, and `claude auth login` only does the first. Without this, the
// first time the user actually runs `claude` under a freshly linked
// account they get the whole first-run wizard — pick a theme, then
// "Select login method" — on an account that is already signed in. It
// looks exactly like the login silently failed.
//
// The file is Claude Code's own config (`.claude.json` inside
// CLAUDE_CONFIG_DIR); everything already in it is preserved.
func MarkOnboarded(configDir, claudeBinary string) error {
	path := filepath.Join(configDir, ".claude.json")

	config := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		// A file we can't parse is left alone rather than clobbered.
		if err := json.Unmarshal(data, &config); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	config["hasCompletedOnboarding"] = true
	if version := claudeVersion(claudeBinary); version != "" {
		// Claude Code re-runs onboarding when this is older than the
		// running version, so record the version that just logged in.
		config["lastOnboardingVersion"] = version
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".clawdh-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// versionPattern pulls "2.1.263" out of `claude --version`, which
// prints e.g. "2.1.263 (Claude Code)".
var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

func claudeVersion(claudeBinary string) string {
	if claudeBinary == "" {
		claudeBinary = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	name, args := claudebin.Invocation(claudeBinary, []string{"--version"})
	out, err := winproc.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return versionPattern.FindString(strings.TrimSpace(string(out)))
}
