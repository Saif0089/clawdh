package meter

import (
	"math"
	"testing"
)

func TestSonnet4IsTheBaseline(t *testing.T) {
	// One Sonnet-4 input token is exactly 1.0 weighted, by definition.
	m := Measure("claude-sonnet-4-6", Usage{Input: 1})
	if !m.Known {
		t.Fatal("sonnet-4 must be a known model")
	}
	if math.Abs(m.Weighted-1.0) > 1e-9 {
		t.Errorf("sonnet-4 input weight = %v, want 1.0", m.Weighted)
	}
}

func TestModelTiersRankAsExpected(t *testing.T) {
	// fable/mythos > opus > sonnet-4 > sonnet-5 > haiku, on the same input.
	in := Usage{Input: 1000}
	fable := Measure("claude-fable-5-1", in).Weighted
	opus := Measure("claude-opus-5", in).Weighted
	s4 := Measure("claude-sonnet-4-6", in).Weighted
	s5 := Measure("claude-sonnet-5", in).Weighted
	haiku := Measure("claude-haiku-4-5-20251001", in).Weighted
	if !(fable > opus && opus > s4 && s4 > s5 && s5 > haiku) {
		t.Errorf("tier ordering wrong: fable=%v opus=%v s4=%v s5=%v haiku=%v", fable, opus, s4, s5, haiku)
	}
}

func TestUnknownModelIsReportedNeverInherited(t *testing.T) {
	// The bug that overcharged the fleet 50%: a model we don't know must NOT be
	// scored as if it were some family member. It is reported unknown with zero
	// weight/cost so the raw tokens are recorded but nothing is fabricated.
	m := Measure("claude-something-brand-new-9", Usage{Input: 1000, Output: 500})
	if m.Known {
		t.Error("an unrecognised model must be Known=false")
	}
	if m.Weighted != 0 || m.CostUSD != 0 {
		t.Errorf("unknown model got weight=%v cost=%v, want 0/0 (no inheritance)", m.Weighted, m.CostUSD)
	}
}

func TestCacheTokensAreCounted(t *testing.T) {
	// The other monitor bug: cache tokens were ignored. Creation and read must
	// both add weight (creation more than read).
	base := Measure("claude-sonnet-4-6", Usage{Input: 100}).Weighted
	withCreate := Measure("claude-sonnet-4-6", Usage{Input: 100, CacheCreation: 100}).Weighted
	withRead := Measure("claude-sonnet-4-6", Usage{Input: 100, CacheRead: 100}).Weighted
	if !(withCreate > withRead && withRead > base) {
		t.Errorf("cache tokens not weighted right: base=%v read=%v create=%v", base, withRead, withCreate)
	}
}

func TestCostTracksTokens(t *testing.T) {
	// Opus output is priced at $25/MTok; 1M output tokens ~ $25.
	m := Measure("claude-opus-5", Usage{Output: 1_000_000})
	if math.Abs(m.CostUSD-25.0) > 1e-6 {
		t.Errorf("opus 1M output cost = %v, want ~25", m.CostUSD)
	}
}

// A one-hour cache write is priced at 2x input, a five-minute one at 1.25x.
func TestHourCacheWritesCostMore(t *testing.T) {
	five := Measure("claude-sonnet-4-5", Usage{CacheCreation: 1000})
	mixed := Measure("claude-sonnet-4-5", Usage{CacheCreation: 1000, CacheCreation1h: 400})
	if five.Weighted != 1250 {
		t.Errorf("five-minute write = %v, want 1250", five.Weighted)
	}
	if mixed.Weighted != 600*1.25+400*2 {
		t.Errorf("mixed write = %v, want %v", mixed.Weighted, 600*1.25+400*2)
	}
	if got := WeightedParts("claude-sonnet-4-5", Usage{CacheCreation: 1000, CacheCreation1h: 400}).Sum(); got != mixed.Weighted {
		t.Errorf("parts sum %v != weighted %v", got, mixed.Weighted)
	}
}
