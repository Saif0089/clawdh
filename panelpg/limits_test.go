package panelpg

import (
	"math"
	"testing"
)

// windowShareOf is the "% of the weekly window" math a %-quota is checked
// against: per account, the person's fraction of its usage times the account's
// real utilisation, summed — and nothing until utilisation data is present.
func TestWindowShareOf(t *testing.T) {
	for _, tc := range []struct {
		name              string
		mine, total, util map[string]float64
		want              float64
	}{
		{
			// One account at 80% weekly; the person did half of it -> 40% of the window.
			name:  "half of an 80%% window",
			mine:  map[string]float64{"a": 400},
			total: map[string]float64{"a": 800},
			util:  map[string]float64{"a": 0.8},
			want:  0.4,
		},
		{
			// Two accounts: 100% of a 50% window + 25% of a 40% window = 0.5 + 0.1.
			name:  "across two accounts",
			mine:  map[string]float64{"a": 1000, "b": 250},
			total: map[string]float64{"a": 1000, "b": 1000},
			util:  map[string]float64{"a": 0.5, "b": 0.4},
			want:  0.6,
		},
		{
			// No utilisation data yet -> fail open (share 0), a %-quota never blocks.
			name:  "no utilisation data",
			mine:  map[string]float64{"a": 500},
			total: map[string]float64{"a": 800},
			util:  map[string]float64{},
			want:  0,
		},
	} {
		if got := windowShareOf(tc.mine, tc.total, tc.util); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: windowShareOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}
