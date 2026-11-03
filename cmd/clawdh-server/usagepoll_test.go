package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"clawdh/internal/gateway"
	"clawdh/internal/usage"
)

// The weekly window the boards show is lifted out of a usage report as a
// fraction with its reset; a report without it is reported as such. A leftover
// "session" entry (the five-hour window Anthropic removed) is not a window.
func TestWindowsFromReport(t *testing.T) {
	reset := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	w, ok := windowsFromReport(&usage.Report{Limits: []usage.Limit{
		{Kind: "session", Percent: 42, ResetsAt: &reset},
		{Kind: "weekly_all", Percent: 43.5},
		{Kind: "weekly_scoped", Percent: 38}, // per-model: not a window the boards draw
	}})
	if !ok || w.SevenD != 0.435 || !w.SevenDReset.IsZero() {
		t.Fatalf("windows = %+v ok=%v", w, ok)
	}
	if _, ok := windowsFromReport(&usage.Report{Limits: []usage.Limit{{Kind: "session", Percent: 42, ResetsAt: &reset}}}); ok {
		t.Error("a session entry alone must not count as a reading")
	}
	if _, ok := windowsFromReport(&usage.Report{}); ok {
		t.Error("an empty report must not count as a reading")
	}
}

// A login whose sign-in has ended is said so once, with its reason saved for the
// panel, and the usage poller then leaves it alone: it used to ask the token
// service about it every four minutes for as long as the login stayed dead. A
// re-added login (a different stored credential) is read again.
func TestThePollerStopsAskingAboutAnEndedLogin(t *testing.T) {
	u, _, db, s := newTestUpstream(t)
	d := u.snapshot()
	acct, ok := d.Account("acct1")
	if !ok {
		t.Fatal("the test account is missing")
	}

	if u.knownEnded(*acct) {
		t.Fatal("a login nobody has found ended is reported ended")
	}
	expired := fmt.Errorf("refreshing the shared login: %w",
		&gateway.RefreshError{Status: "400 Bad Request", Code: "invalid_grant", Description: "Refresh token expired"})
	u.markEnded(*acct, expired)

	if err := u.flush(); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	note := db.notes["acct1"]
	db.mu.Unlock()
	if note != expiredNote {
		t.Errorf("noted %q, want the expiry note", note)
	}

	if !u.knownEnded(*acct) {
		t.Fatal("an ended login is not remembered as ended")
	}
	// A round now skips it: no token manager is built, so no token service is asked.
	pollUsageOnce(context.Background(), u, usage.NewClient())
	u.mu.Lock()
	built := len(u.managers)
	u.mu.Unlock()
	if built != 0 {
		t.Errorf("a round opened %d login(s); an ended one should be skipped", built)
	}

	// The account's owner signs in again: the stored login is different.
	again := *acct
	again.Credential = sealedLogin(t, s, "at-2", "rt-2", time.Now().Add(8*time.Hour))
	if u.knownEnded(again) {
		t.Error("a re-added login is still treated as ended")
	}
}
