package usage

import (
	"clawdh/internal/config"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is where Claude Code reads plan usage from — the same
// numbers the `/usage` slash command shows.
//
// It is not a documented, versioned API, so everything here degrades
// rather than breaks: a changed shape or an error means the page says
// usage is unavailable, and nothing else in clawdh is affected.
const DefaultEndpoint = "https://api.anthropic.com/api/oauth/usage"

// Limit is one usage window: the five-hour session allowance, the
// weekly allowance, or a weekly allowance scoped to one model.
type Limit struct {
	Kind     string     `json:"kind"`
	Group    string     `json:"group"`
	Label    string     `json:"label"`
	Percent  float64    `json:"percent"`
	Severity string     `json:"severity"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
	Active   bool       `json:"active"`
}

// Report is everything clawdh knows about one account's plan usage.
type Report struct {
	Limits     []Limit   `json:"limits"`
	FetchedAt  time.Time `json:"fetchedAt"`
	Plan       string    `json:"plan,omitempty"`
	ExtraUsage bool      `json:"extraUsage"`
}

// apiResponse mirrors the endpoint's payload. Only the normalised
// `limits` array is read: the per-window fields alongside it carry the
// same numbers, and several have opaque code names.
type apiResponse struct {
	Limits []struct {
		Kind     string  `json:"kind"`
		Group    string  `json:"group"`
		Percent  float64 `json:"percent"`
		Severity string  `json:"severity"`
		ResetsAt *string `json:"resets_at"`
		IsActive bool    `json:"is_active"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
	ExtraUsage struct {
		IsEnabled bool `json:"is_enabled"`
	} `json:"extra_usage"`
}

// ErrLoginRejected means the stored token is no longer accepted: the
// account needs reconnecting, which is a different thing to say than
// "usage is unavailable".
var ErrLoginRejected = errors.New("this account's login was rejected, reconnect it")

// IsUnauthorized reports whether an error from Fetch means Anthropic refused
// the token itself (revoked or rotated away), as opposed to a transport or
// rate-limit failure.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrLoginRejected) }

// RateLimited means Anthropic accepted the token and refused the
// request anyway: too many of them, too fast.
//
// This endpoint publishes no rate-limit budget — there are no
// anthropic-ratelimit-* headers on it, and the Retry-After it does send
// with a 429 has been observed as "0", which is not a delay anyone can
// wait. So the number that matters is decided here, not by the server:
// RetryAfter is only set when the header names a real one.
type RateLimited struct {
	RetryAfter time.Duration
}

func (e *RateLimited) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("Anthropic is rate-limiting plan usage; it asked to wait %s", e.RetryAfter)
	}
	return "Anthropic is rate-limiting plan usage"
}

// Client fetches usage reports.
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
}

// NewClient returns a Client with sensible defaults.
//
// CLAWDH_USAGE_ENDPOINT redirects it, so the end-to-end tests can exercise
// the whole path — server, handler, page — against a stub instead of
// calling Anthropic from CI.
func NewClient() *Client {
	endpoint := config.Env("USAGE_ENDPOINT")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Fetch reports the plan usage for the account holding creds.
func (c *Client) Fetch(ctx context.Context, creds Credentials) (*Report, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Anthropic to read plan usage: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, ErrLoginRejected
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &RateLimited{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("plan usage is unavailable right now (HTTP %d)", resp.StatusCode)
	}

	var payload apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("could not read the plan usage response: %w", err)
	}

	report := &Report{
		FetchedAt:  time.Now().UTC(),
		Plan:       creds.SubscriptionType,
		ExtraUsage: payload.ExtraUsage.IsEnabled,
	}
	for _, l := range payload.Limits {
		limit := Limit{
			Kind:     l.Kind,
			Group:    l.Group,
			Percent:  l.Percent,
			Severity: l.Severity,
			Active:   l.IsActive,
		}
		model := ""
		if l.Scope != nil && l.Scope.Model != nil {
			model = l.Scope.Model.DisplayName
		}
		limit.Label = labelFor(l.Kind, model)

		if l.ResetsAt != nil && *l.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, *l.ResetsAt); err == nil {
				utc := t.UTC()
				limit.ResetsAt = &utc
			}
		}
		report.Limits = append(report.Limits, limit)
	}

	// Session first, then the weekly windows: that is the order they
	// run out in, and the order someone deciding "can I start this
	// now?" reads them.
	sort.SliceStable(report.Limits, func(i, j int) bool {
		return groupRank(report.Limits[i].Group) < groupRank(report.Limits[j].Group)
	})
	return report, nil
}

// retryAfter reads the header in both forms the spec allows — a count
// of seconds, or an HTTP date — and returns zero for anything that is
// not a delay worth waiting, including the "0" this endpoint sends.
func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}

func groupRank(group string) int {
	switch group {
	case "session":
		return 0
	case "weekly":
		return 1
	default:
		return 2
	}
}

// labelFor turns the API's kind into words a person reads, rather than
// showing names like "weekly_scoped".
func labelFor(kind, model string) string {
	switch kind {
	case "session":
		return "Current session"
	case "weekly_all":
		return "This week, all models"
	case "weekly_scoped":
		if model != "" {
			return "This week, " + model
		}
		return "This week, one model"
	default:
		pretty := strings.ReplaceAll(kind, "_", " ")
		if model != "" {
			return pretty + ", " + model
		}
		return pretty
	}
}
