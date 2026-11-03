package panelpg

import (
	"context"
	"testing"
	"time"
)

// A break is shown only until the login reads fine again. The gateway reads every
// held login every few minutes, so a reading newer than the break means the login
// works now — however it was fixed — and the card must stop saying it stopped
// working. It used to say so for the whole 24 hours.
func TestABreakIsForgottenOnceTheLoginReadsAgain(t *testing.T) {
	b := freshBackend(t)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	const id = "acct-recovers"

	has := func(want bool, when string) {
		t.Helper()
		got, err := b.RecentCollisions(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got[id]; ok != want {
			t.Errorf("%s: break listed = %v, want %v", when, ok, want)
		}
	}

	if err := b.RecordCollision(ctx, id, "the shared login reached the end of its sign-in"); err != nil {
		t.Fatal(err)
	}
	has(true, "right after the break")

	// The login is signed in again and the gateway reads it successfully.
	time.Sleep(50 * time.Millisecond)
	b.RecordWindows(id, 0.4, time.Now().Add(24*time.Hour), nil)
	has(false, "after a reading newer than the break")

	// It breaks again later: that one counts.
	time.Sleep(50 * time.Millisecond)
	if err := b.RecordCollision(ctx, id, "the shared login failed to refresh"); err != nil {
		t.Fatal(err)
	}
	has(true, "after it broke a second time")

	// An older break than the cutoff is not listed at all.
	if got, _ := b.RecentCollisions(ctx, time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("a break before the cutoff was listed: %v", got)
	}
}
