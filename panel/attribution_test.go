package panel

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A plan's size is learned from its own finished, fully metered weeks — the
// median of up to four, ignoring the window still running and any reading too
// coarse to size a plan from — and only without any does the running week stand
// in.
func TestAPlansSizeIsLearnedFromItsOwnFinishedWeeks(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	week := func(daysAgo int, util, weighted float64) WeekSample {
		return WeekSample{Reset: now.Add(-time.Duration(daysAgo) * 24 * time.Hour), Util: util, Weighted: weighted}
	}
	running := WeekSample{Reset: now.Add(40 * time.Hour), Util: 0.5, Weighted: 100}

	got, basis := weekScale([]WeekSample{
		running,               // still open: not a finished week
		week(1, 1.0, 1400e6),  // 1.40B
		week(8, 0.9, 1350e6),  // 1.50B
		week(15, 0.10, 500e6), // a 10% reading is too coarse to size a plan from
		week(22, 1.0, 1600e6), // 1.60B
	}, nil, now)
	if basis != ScaleHistory || !near(got, 1500e6) {
		t.Errorf("scale = %v (%s), want the median of the three usable weeks, 1.5B (history)", got, basis)
	}

	// Only four weeks are looked at, newest first, so a plan change is followed.
	var many []WeekSample
	for i, size := range []float64{2000e6, 2000e6, 2000e6, 2000e6, 100e6, 100e6, 100e6} {
		many = append(many, week(1+7*i, 1.0, size))
	}
	if got, _ := weekScale(many, nil, now); !near(got, 2000e6) {
		t.Errorf("scale = %v, want the four newest weeks (2B), not older ones", got)
	}

	// No finished week yet: the running one, when it is full enough.
	if got, basis := weekScale(nil, &WeekSample{Reset: running.Reset, Util: 0.8, Weighted: 800e6}, now); basis != ScaleThisWeek || !near(got, 1000e6) {
		t.Errorf("scale = %v (%s), want this week's own ratio, 1B (this-week)", got, basis)
	}
	// …and nothing at all rather than a guess when it is not.
	if got, basis := weekScale(nil, &WeekSample{Reset: running.Reset, Util: 0.1, Weighted: 80e6}, now); basis != ScaleNone || got != 0 {
		t.Errorf("scale = %v (%s), want none from a 10%% reading", got, basis)
	}
	if got, basis := weekScale(nil, nil, now); basis != ScaleNone || got != 0 {
		t.Errorf("scale = %v (%s), want none with nothing to learn from", got, basis)
	}
}

// What the gateway did not see is nobody's. Each person gets what their own
// tokens come to; the rest of Anthropic's figure is reported as unaccounted.
func TestWhatNobodyMeteredIsNotHandedToAnyone(t *testing.T) {
	shares, rest := splitWeek(0.80, 1000e6, []float64{100e6, 50e6})
	if !near(shares[0], 0.10) || !near(shares[1], 0.05) || !near(rest, 0.65) {
		t.Errorf("shares = %v, unaccounted = %v, want 10%% and 5%% with 65%% left over", shares, rest)
	}

	// The shares never exceed what Anthropic reports: a size learned a little
	// small is scaled down to fit, and nothing is then left over.
	shares, rest = splitWeek(0.80, 1000e6, []float64{900e6, 500e6})
	if got := shares[0] + shares[1]; !near(got, 0.80) || rest != 0 {
		t.Errorf("shares add to %v with %v left, want exactly the reported 80%% and nothing over", got, rest)
	}
	if !near(shares[0]/shares[1], 900.0/500.0) {
		t.Errorf("scaling changed the people's proportions: %v", shares)
	}

	// Without a size nobody gets a percentage, and all of it is unaccounted.
	shares, rest = splitWeek(0.80, 0, []float64{100e6})
	if shares[0] != 0 || !near(rest, 0.80) {
		t.Errorf("shares = %v, unaccounted = %v, want no invented share and all 80%% unaccounted", shares, rest)
	}
	if _, rest := splitWeek(0, 1000e6, []float64{1}); rest != 0 {
		t.Errorf("unaccounted = %v with nothing reported, want 0", rest)
	}
}

// boardAccount reads the usage board's one account after wiring a window and who
// ran it.
func boardAccount(t *testing.T, h *harness, id string) map[string]any {
	t.Helper()
	code, body := h.do("GET", "/api/usage/accounts?window=week", nil, "")
	if code != 200 {
		t.Fatalf("board = %d %v", code, body)
	}
	for _, a := range body["accounts"].([]any) {
		if m := a.(map[string]any); m["accountId"] == id {
			return m
		}
	}
	t.Fatalf("account %s is not on the board: %v", id, body)
	return nil
}

func shareOf(t *testing.T, acct map[string]any, name string) (ofWeekly, weighted float64) {
	t.Helper()
	for _, p := range acct["people"].([]any) {
		if m := p.(map[string]any); m["name"] == name {
			return m["ofWeekly"].(float64), m["weighted"].(float64)
		}
	}
	t.Fatalf("%s is not on the card: %v", name, acct["people"])
	return 0, 0
}

// The bug this exists for. A login is removed and added again, so it comes back
// under a new account ID that holds only what was metered since. The plan's week
// is 80% full; most of that was spent under the old ID and is not on this
// card. The people who ran it since must read as what their own tokens come to,
// not as shares of the whole 80%.
func TestAReaddedLoginsEarlierWeekIsNotChargedToWhoeverRanItAfter(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	bobID, bobTok := h.join("Bob", "bob-pc")
	acct := h.contribute(bobTok, "Shared Max", "Shared@Example.com")

	reset := h.clock.Add(50 * time.Hour)
	usage.windows = []AccountWindow{{AccountID: acct, SevenD: 0.80, SevenDReset: reset, UpdatedAt: h.clock}}
	// Metered only since the re-add, two hours ago: inside the week, so the
	// week is not one the gateway watched from its start.
	usage.first = h.clock.Add(-2 * time.Hour)
	usage.ranBy = map[string][]SubjectUsage{acct: {
		{SubjectID: bobID, Weighted: 92e6},
		{SubjectID: aliceID, Weighted: 15e6},
	}}
	// What the plan's finished weeks say a whole week is: 1.4B and 1.5B.
	usage.samples = []WeekSample{
		{Reset: h.clock.Add(-4 * 24 * time.Hour), Util: 1.0, Weighted: 1400e6},
		{Reset: h.clock.Add(-11 * 24 * time.Hour), Util: 1.0, Weighted: 1500e6},
	}

	card := boardAccount(t, h, acct)
	size := 1450e6
	aliceOf, aliceTokens := shareOf(t, card, "Alice")
	bobOf, _ := shareOf(t, card, "Bob")
	if !near(aliceOf, 15e6/size) || !near(bobOf, 92e6/size) {
		t.Errorf("Alice = %.4f, Bob = %.4f, want what their own tokens come to (%.4f, %.4f)", aliceOf, bobOf, 15e6/size, 92e6/size)
	}
	if oldWay := 0.80 * (15.0 / 107.0); aliceOf > oldWay/4 {
		t.Errorf("Alice reads %.1f%%, close to the %.1f%% the old split charged her for a week she mostly did not spend", 100*aliceOf, 100*oldWay)
	}
	if aliceTokens != 15e6 {
		t.Errorf("Alice's tokens = %v, want the exact 15M", aliceTokens)
	}
	if got, want := card["unaccounted"].(float64), 0.80-(15e6+92e6)/size; !near(got, want) {
		t.Errorf("unaccounted = %v, want the rest of the 80%% week, %v", got, want)
	}
	if card["weekBasis"] != ScaleHistory || card["tokensPerWeek"].(float64) != size {
		t.Errorf("basis = %v, size = %v, want history and 1.45B", card["weekBasis"], card["tokensPerWeek"])
	}
	if len(usage.recorded) != 0 {
		t.Errorf("filed %v as a week the gateway watched, but it began metering mid-week", usage.recorded)
	}
	// The panel told the metering which login this ID is, lower-cased, so a
	// later re-add under another ID inherits the history.
	if usage.links[acct] != "shared@example.com" {
		t.Errorf("linked as %q, want the lower-cased email", usage.links[acct])
	}
}

// With no size to go by — a login new to the gateway, metered only part of its
// first week — no percentage is invented. The people's tokens stay exact and the
// whole of Anthropic's figure is reported as unaccounted.
func TestWithoutAKnownPlanSizeNoPercentageIsInvented(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	acct := h.contribute(aliceTok, "New Login", "new@example.com")

	usage.windows = []AccountWindow{{AccountID: acct, SevenD: 0.60, SevenDReset: h.clock.Add(50 * time.Hour), UpdatedAt: h.clock}}
	usage.first = h.clock.Add(-3 * time.Hour)
	usage.ranBy = map[string][]SubjectUsage{acct: {{SubjectID: aliceID, Weighted: 40e6}}}

	card := boardAccount(t, h, acct)
	of, tokens := shareOf(t, card, "Alice")
	if of != 0 || tokens != 40e6 {
		t.Errorf("Alice = %v of the week, %v tokens, want no percentage and her exact 40M", of, tokens)
	}
	if card["weekBasis"] != ScaleNone || !near(card["unaccounted"].(float64), 0.60) {
		t.Errorf("basis = %v, unaccounted = %v, want none and the whole 60%%", card["weekBasis"], card["unaccounted"])
	}
}

// A login the gateway has watched since before the week began has nothing
// missing, so the running week can stand in for a history it does not have yet —
// and is filed, once, so that it becomes part of one.
func TestAFullyMeteredFirstWeekNeedsNoHistoryAndIsFiledOnce(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, _ := h.join("Bob", "bob-pc")
	acct := h.contribute(aliceTok, "Team", "team@example.com")

	reset := h.clock.Add(50 * time.Hour)
	usage.windows = []AccountWindow{{AccountID: acct, SevenD: 0.60, SevenDReset: reset, UpdatedAt: h.clock}}
	usage.first = h.clock.Add(-20 * 24 * time.Hour)
	usage.ranBy = map[string][]SubjectUsage{acct: {
		{SubjectID: aliceID, Weighted: 300e6},
		{SubjectID: bobID, Weighted: 100e6},
		{SubjectID: "someone-removed", Weighted: 200e6}, // metered, so not "unaccounted", though no card
	}}

	card := boardAccount(t, h, acct)
	aliceOf, _ := shareOf(t, card, "Alice")
	bobOf, _ := shareOf(t, card, "Bob")
	// 600M of metered tokens in a 60% week: a plan of 1B tokens a week.
	if card["weekBasis"] != ScaleThisWeek || !near(card["tokensPerWeek"].(float64), 1000e6) {
		t.Errorf("basis = %v, size = %v, want this-week and 1B", card["weekBasis"], card["tokensPerWeek"])
	}
	if !near(aliceOf, 0.30) || !near(bobOf, 0.10) {
		t.Errorf("Alice = %v, Bob = %v, want 30%% and 10%%", aliceOf, bobOf)
	}
	if card["unaccounted"].(float64) != 0 {
		t.Errorf("unaccounted = %v, want nothing missing from a week metered whole", card["unaccounted"])
	}
	if len(usage.recorded) != 1 || usage.recorded[0].Util != 0.60 || usage.recorded[0].Weighted != 600e6 {
		t.Fatalf("filed %+v, want this week once: 60%% and all 600M, the removed person's included", usage.recorded)
	}
	if want := weekKey(reset); !usage.recorded[0].Reset.Equal(want) {
		t.Errorf("filed under %v, want the reset rounded to the hour (%v)", usage.recorded[0].Reset, want)
	}

	// Reading the board again, or a machine checking in, must not file it again
	// until the week has moved.
	boardAccount(t, h, acct)
	if len(usage.recorded) != 1 {
		t.Errorf("filed %d times for an unmoved week, want once", len(usage.recorded))
	}
	usage.windows[0].SevenD = 0.65
	boardAccount(t, h, acct)
	if len(usage.recorded) != 2 || usage.recorded[1].Util != 0.65 {
		t.Errorf("filed %+v, want the week filed again once its reading rose to 65%%", usage.recorded)
	}
}

// The machine's own page reads the same attribution off its check-in.
func TestCheckinCarriesTheUnaccountedShareToTheMachine(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, bobTok := h.join("Bob", "bob-pc")
	acct := h.contribute(bobTok, "Team", "team@example.com")
	if code, body := h.do("POST", "/api/accounts/"+acct+"/share", map[string]string{"personId": aliceID}, ""); code != 200 {
		t.Fatalf("sharing = %d %v", code, body)
	}

	usage.windows = []AccountWindow{{AccountID: acct, SevenD: 0.80, SevenDReset: h.clock.Add(50 * time.Hour), UpdatedAt: h.clock}}
	usage.first = h.clock.Add(-2 * time.Hour)
	usage.ranBy = map[string][]SubjectUsage{acct: {{SubjectID: bobID, Weighted: 100e6}}}
	usage.samples = []WeekSample{{Reset: h.clock.Add(-4 * 24 * time.Hour), Util: 1.0, Weighted: 1000e6}}

	_, body := h.do("POST", "/api/v1/checkin", nil, aliceTok)
	wins, _ := body["windows"].([]any)
	if len(wins) != 1 {
		t.Fatalf("windows = %v, want the one shared account's", body["windows"])
	}
	w := wins[0].(map[string]any)
	if !near(w["unaccounted"].(float64), 0.80-0.10) || w["weekBasis"] != ScaleHistory {
		t.Errorf("unaccounted = %v, basis = %v, want 70%% unaccounted from history", w["unaccounted"], w["weekBasis"])
	}
	people := w["people"].([]any)
	if len(people) != 1 || !near(people[0].(map[string]any)["ofWeekly"].(float64), 0.10) {
		t.Errorf("people = %v, want Bob at his own 10%%", people)
	}
}
