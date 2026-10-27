package panelpg

import (
	"context"
	"testing"
	"time"
)

// Each person's newest metered request since a time, and only theirs: what the
// panel reads "working" from.
func TestLastRequestsIsEachPersonsNewestSinceATime(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for i, ev := range []struct {
		person string
		ago    time.Duration
	}{{"alice", 10 * time.Minute}, {"alice", 40 * time.Second}, {"alice", 2 * time.Minute}, {"bob", 9 * time.Minute}, {"", time.Second}} {
		if err := b.RecordUsage(ctx, UsageEvent{At: at.Add(-ev.ago), PersonID: ev.person, AccountID: "a", Model: "m", RequestID: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.LastRequests(ctx, at.Add(-3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["alice"].Equal(at.Add(-40*time.Second)) {
		t.Errorf("last requests = %v, want only Alice, 40s ago (Bob's is too old, and nobody's is nobody)", got)
	}
}
