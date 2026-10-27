package panelpg

import (
	"context"
	"testing"
	"time"

	"clawdh/panel"
)

func resetLineage(t *testing.T, b *Backend) {
	t.Helper()
	if _, err := b.db.Exec(`DELETE FROM usage_events; DELETE FROM usage_counters; DELETE FROM account_lineage; DELETE FROM week_samples`); err != nil {
		t.Fatal(err)
	}
}

func meter(t *testing.T, b *Backend, at time.Time, person, account string, weighted float64) {
	t.Helper()
	if err := b.RecordUsage(context.Background(), UsageEvent{
		At: at, PersonID: person, AccountID: account, Model: "claude-opus-5", Weighted: weighted,
		RequestID: person + account + at.String(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A login removed and added again is the same login under a new account ID. What
// was metered under the old ID is part of its week, and the gateway has been
// watching it since the first of them — not since the re-add.
func TestAReaddedLoginKeepsTheUsageMeteredUnderItsOldID(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)

	week := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	meter(t, b, week.Add(12*time.Hour), "hassan", "old-id", 880e6)
	meter(t, b, week.Add(36*time.Hour), "hassan", "new-id", 15e6)
	meter(t, b, week.Add(36*time.Hour), "bd-loq", "new-id", 28e6)
	meter(t, b, week.Add(30*time.Hour), "hassan", "someone-elses-login", 999e6)

	for id, key := range map[string]string{"old-id": "hassan@example.com", "new-id": "hassan@example.com", "someone-elses-login": "other@example.com"} {
		if err := b.LinkLogin(ctx, id, key); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := b.AccountUsageByPerson(ctx, "new-id", week)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.SubjectID] = r.Weighted
	}
	if got["hassan"] != 895e6 || got["bd-loq"] != 28e6 || len(got) != 2 {
		t.Errorf("new-id's week = %v, want hassan 895M (880M under the old ID + 15M) and bd-loq 28M, and nothing of another login's", got)
	}

	first, err := b.FirstMeteredAt(ctx, "new-id")
	if err != nil {
		t.Fatal(err)
	}
	if want := week.Add(12 * time.Hour); !first.Equal(want) {
		t.Errorf("first metered = %v, want %v — when the login was first seen, not when it was re-added", first, want)
	}

	// An account that was never linked is only itself; it must not match
	// every other unlinked account (a NULL login key matching a NULL).
	meter(t, b, week, "dawood", "unlinked-a", 1)
	meter(t, b, week, "rizwan", "unlinked-b", 2)
	rows, err = b.AccountUsageByPerson(ctx, "unlinked-a", week)
	if err != nil || len(rows) != 1 || rows[0].SubjectID != "dawood" {
		t.Errorf("an unlinked account read as %v (%v), want only itself", rows, err)
	}
	if first, _ := b.FirstMeteredAt(ctx, "never-metered"); !first.IsZero() {
		t.Errorf("first metered of an unknown account = %v, want zero", first)
	}

	// Linking again — every process does, once — changes nothing.
	if err := b.LinkLogin(ctx, "new-id", "hassan@example.com"); err != nil {
		t.Fatal(err)
	}
}

// A week's reading is filed as it is seen and the fullest one is kept; the
// samples belong to the login, whichever of its IDs filed them.
func TestAWeeksFullestReadingIsKeptAndSharedAcrossAnIDChange(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	for _, id := range []string{"old-id", "new-id"} {
		if err := b.LinkLogin(ctx, id, "hassan@example.com"); err != nil {
			t.Fatal(err)
		}
	}

	reset := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	for _, s := range []panel.WeekSample{
		{Reset: reset, Util: 0.50, Weighted: 700e6},
		{Reset: reset, Util: 0.97, Weighted: 1400e6},
		{Reset: reset, Util: 0.40, Weighted: 100e6}, // a stale reading after the fact must not replace it
	} {
		if err := b.RecordWeekSample(ctx, "old-id", s); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.RecordWeekSample(ctx, "new-id", panel.WeekSample{Reset: reset.Add(7 * 24 * time.Hour), Util: 0.8, Weighted: 1100e6}); err != nil {
		t.Fatal(err)
	}

	got, err := b.WeekSamples(ctx, "new-id", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Reset.Equal(reset.Add(7*24*time.Hour)) || got[1].Util != 0.97 || got[1].Weighted != 1400e6 {
		t.Errorf("samples = %+v, want the newer week first and the older one at its fullest (97%%, 1.4B)", got)
	}
	if one, _ := b.WeekSamples(ctx, "old-id", 1); len(one) != 1 {
		t.Errorf("limit 1 gave %d samples", len(one))
	}
	// An unlinked account's samples are its own, not everyone's.
	if err := b.RecordWeekSample(ctx, "lone", panel.WeekSample{Reset: reset, Util: 0.9, Weighted: 1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.WeekSamples(ctx, "another-lone", 5); len(got) != 0 {
		t.Errorf("another account read %+v from a stranger's samples", got)
	}
}
