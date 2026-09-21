package main

import (
	"testing"
	"time"

	"clawdh/internal/usage"
)

// The two rolling windows the boards show are lifted out of a usage report as
// fractions with their resets; a report without them is reported as such.
func TestWindowsFromReport(t *testing.T) {
	reset := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	w, ok := windowsFromReport(&usage.Report{Limits: []usage.Limit{
		{Kind: "session", Percent: 42, ResetsAt: &reset},
		{Kind: "weekly_all", Percent: 43.5},
		{Kind: "weekly_scoped", Percent: 38}, // per-model: not a window the boards draw
	}})
	if !ok || w.FiveH != 0.42 || w.SevenD != 0.435 || !w.FiveHReset.Equal(reset) || !w.SevenDReset.IsZero() {
		t.Fatalf("windows = %+v ok=%v", w, ok)
	}
	if _, ok := windowsFromReport(&usage.Report{}); ok {
		t.Error("an empty report must not count as a reading")
	}
}
