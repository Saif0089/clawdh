package cli

import (
	"testing"
	"time"
)

// A machine whose check-ins keep failing asks less and less often, capped at
// five minutes; one that is succeeding stays on the thirty-second beat.
func TestCheckInBackoff(t *testing.T) {
	for _, c := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 5 * time.Minute},
		{50, 5 * time.Minute},
	} {
		if got := checkInBackoff(c.failures); got != c.want {
			t.Errorf("checkInBackoff(%d) = %s, want %s", c.failures, got, c.want)
		}
	}
}
