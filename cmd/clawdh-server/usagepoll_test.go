package main

import (
	"testing"
	"time"

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
