package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// realPayload is a trimmed copy of a real response from the endpoint,
// kept verbatim in shape so a change upstream shows up here first.
const realPayload = `{
  "five_hour": {"utilization": 5, "resets_at": "2026-09-07T05:29:59.999999+00:00"},
  "seven_day": {"utilization": 59, "resets_at": "2026-09-08T13:59:59.999999+00:00"},
  "limits": [
    {"kind": "session", "group": "session", "percent": 5, "severity": "normal",
     "resets_at": "2026-09-07T05:29:59.999999+00:00", "is_active": true},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 6, "severity": "normal",
     "resets_at": "2026-09-08T13:59:59.999999+00:00", "is_active": true,
     "scope": {"model": {"display_name": "Fable"}}},
    {"kind": "weekly_all", "group": "weekly", "percent": 59, "severity": "normal",
     "resets_at": "2026-09-08T13:59:59.999999+00:00", "is_active": true}
  ],
  "extra_usage": {"is_enabled": false}
}`

func TestFetchParsesLimits(t *testing.T) {
	var gotAuth, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	report, err := c.Fetch(context.Background(), Credentials{AccessToken: "tok", SubscriptionType: "max"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotBeta != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q", gotBeta)
	}
	if report.Plan != "max" {
		t.Errorf("Plan = %q, want max", report.Plan)
	}
	// The payload still carries the five-hour "session" entry Anthropic used to
	// send; that window is gone, so it is dropped and nothing shows or acts on it.
	if len(report.Limits) != 2 {
		t.Fatalf("got %d limits, want the 2 weekly ones", len(report.Limits))
	}
	for _, l := range report.Limits {
		if l.Kind == "session" || l.Label == "Current session" {
			t.Errorf("the removed five-hour window survived: %+v", l)
		}
	}

	labels := []string{report.Limits[0].Label, report.Limits[1].Label}
	want := map[string]bool{"This week, all models": true, "This week, Fable": true}
	for _, label := range labels {
		if !want[label] {
			t.Errorf("unexpected weekly label %q", label)
		}
	}

	reset := report.Limits[0].ResetsAt
	if reset == nil {
		t.Fatal("weekly limit has no reset time")
	}
	if got := reset.Format(time.RFC3339); got != "2026-09-08T13:59:59Z" {
		t.Errorf("reset = %s", got)
	}
}

func TestFetchRejectedLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Fetch(context.Background(), Credentials{AccessToken: "stale"})
	if err == nil {
		t.Fatal("want an error for a rejected token")
	}
	// The UI keys "login expired" off this wording, and a person reads it
	// verbatim, so both care that it stays plain.
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %q, want it to say the login was rejected", err)
	}
}

func TestFetchUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	if _, err := c.Fetch(context.Background(), Credentials{AccessToken: "tok"}); err == nil {
		t.Fatal("want an error for HTTP 500")
	}
}

// An unknown kind must still render as something a person can read,
// rather than leaking the API's snake_case at them.
func TestLabelForUnknownKind(t *testing.T) {
	if got := labelFor("monthly_all", ""); got != "monthly all" {
		t.Errorf("labelFor = %q, want %q", got, "monthly all")
	}
	if got := labelFor("weekly_scoped", ""); got != "This week, one model" {
		t.Errorf("labelFor = %q", got)
	}
}
