package httpserver

import (
	"os"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/usage"
	"clawdh/panel"
)

// Usage for a login the gateway holds.
//
// Once a login is added to a panel, the gateway refreshes it; the copy still on
// this machine goes stale by design (refreshing it here would rotate the token
// out from under the gateway — the very collision the gateway exists to
// prevent). So this machine must not probe that login for usage. Instead every
// check-in brings the gateway's own captured 5h/weekly utilisation for each
// account shared with this person, and a local login whose email matches one
// of them shows that reading. The same numbers /usage shows, from the source
// that actually serves the account — and the same on every card.

// windowStaleAfter is how old a cached gateway reading may be before it is
// treated as absent (the machine has been offline, or the panel lost metering).
const windowStaleAfter = 24 * time.Hour

// gatewayUsageFor is the usage service's Gateway hook: the reading for the
// login in configDir, if the gateway holds an account with the same email.
func gatewayUsageFor(configDir string) (*usage.Report, string, bool) {
	email := ""
	if oa, err := accounts.ReadOAuthAccount(identityDir(configDir)); err == nil && oa != nil {
		email, _ = oa["emailAddress"].(string)
	}
	if email == "" {
		return nil, "", false
	}
	for _, w := range panel.LoadWindows() {
		if strings.EqualFold(w.Email, email) && time.Since(w.UpdatedAt) < windowStaleAfter {
			return windowReport(w), "Read through the gateway — this login is shared, so it's the gateway that refreshes it now.", true
		}
	}
	return nil, "", false
}

// identityDir is where a login's identity (.claude.json) lives: the config dir
// for a managed account, the home dir for the default login.
func identityDir(configDir string) string {
	if configDir != "" {
		return configDir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// windowReport turns a gateway reading into the same shape a probe produces,
// so the page draws it with the same bars.
func windowReport(w panel.ShareWindow) *usage.Report {
	r := &usage.Report{FetchedAt: w.UpdatedAt}
	add := func(kind, group, label string, frac float64, reset time.Time) {
		l := usage.Limit{Kind: kind, Group: group, Label: label, Percent: frac * 100, Severity: severityFor(frac), Active: true}
		if !reset.IsZero() {
			t := reset
			l.ResetsAt = &t
		}
		r.Limits = append(r.Limits, l)
	}
	add("session", "session", "Current session", w.FiveH, w.FiveHReset)
	add("weekly_all", "weekly", "This week, all models", w.SevenD, w.SevenDReset)
	return r
}

func severityFor(frac float64) string {
	switch {
	case frac >= 0.95:
		return "critical"
	case frac >= 0.75:
		return "warning"
	}
	return "normal"
}

// windowForSlug is the cached reading for a shared account, for its card.
func windowForSlug(slug string) (panel.ShareWindow, bool) {
	for _, w := range panel.LoadWindows() {
		if strings.EqualFold(w.Slug, slug) && time.Since(w.UpdatedAt) < windowStaleAfter {
			return w, true
		}
	}
	return panel.ShareWindow{}, false
}
