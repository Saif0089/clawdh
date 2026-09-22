package panel

import (
	"testing"
	"time"
)

// The board's unit is the account. Every account with a login is listed — one
// the gateway has not read yet included, flagged, so "not read" never looks
// like "unused" — with who ran it over the asked rolling period, each person's
// model split, and the account's own total and split summed from them.
func TestTheBoardShowsEachAccountWithWhoRanItAndOnWhat(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, bobTok := h.join("Bob", "bob-pc")
	teamID := h.contribute(aliceTok, "Team Max", "team@example.com")
	spareID := h.contribute(bobTok, "Spare", "spare@example.com")

	usage.windows = []AccountWindow{{AccountID: teamID, FiveH: 0.12, SevenD: 0.61, SevenDReset: h.clock.Add(50 * time.Hour), UpdatedAt: h.clock}}
	usage.ranBy = map[string][]SubjectUsage{teamID: {
		{SubjectID: bobID, Weighted: 300, ByModel: []ModelUsage{{Model: "claude-opus-5", Weighted: 200}, {Model: "claude-sonnet-5", Weighted: 100}}},
		{SubjectID: aliceID, Weighted: 100, ByModel: []ModelUsage{{Model: "claude-sonnet-5", Weighted: 100}}},
		{SubjectID: "someone-removed", Weighted: 999},
	}}

	code, body := h.do("GET", "/api/usage/accounts?window=month", nil, "")
	if code != 200 || body["window"] != "month" {
		t.Fatalf("accounts = %d %v", code, body)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want both (one without a reading): %v", len(accounts), body)
	}
	team := accounts[0].(map[string]any)
	if team["name"] != "Team Max" || team["hasReading"] != true || team["sevenD"] != 0.61 {
		t.Errorf("Team Max row = %v", team)
	}
	people, _ := team["people"].([]any)
	if len(people) != 2 || people[0].(map[string]any)["name"] != "Bob" || people[1].(map[string]any)["name"] != "Alice" {
		t.Errorf("people = %v, want Bob then Alice and nobody removed", people)
	}
	if split, _ := people[0].(map[string]any)["byModel"].([]any); len(split) != 2 {
		t.Errorf("Bob's split = %v, want his two models", people[0])
	}
	if team["weighted"] != 400.0 {
		t.Errorf("Team Max total = %v, want 400 (the people listed, no one removed)", team["weighted"])
	}
	models, _ := team["byModel"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["model"] != "claude-opus-5" && models[0].(map[string]any)["weighted"] != 200.0 {
		t.Errorf("Team Max models = %v", models)
	}
	// Sonnet is 100 + 100 across the two of them; Opus 200 — a tie broken either way, but both present and summed.
	for _, m := range models {
		if mm := m.(map[string]any); mm["weighted"] != 200.0 {
			t.Errorf("model %v = %v, want 200", mm["model"], mm["weighted"])
		}
	}
	// Each account is read for the period on screen first. An account with a
	// window reading is then read a second time, scoped to its weekly window,
	// because that is the span the per-person percentages are a share of.
	for _, id := range []string{teamID, spareID} {
		asks := usage.asks[id]
		if len(asks) == 0 {
			t.Errorf("account %s was never read", id)
			continue
		}
		if got, want := asks[0], h.clock.Add(-30*24*time.Hour); !got.Equal(want) {
			t.Errorf("account %s read since %v, want the last 30 days (%v)", id, got, want)
		}
	}
	// Team Max has a weekly reading, so its people carry a share of the real
	// weekly allowance — not a share of each other, which is 100% for whoever
	// ran it alone and says nothing about the plan.
	if len(usage.asks[teamID]) != 2 {
		t.Errorf("Team Max read %d times, want the period and its weekly window", len(usage.asks[teamID]))
	}
	for _, p := range people {
		pm := p.(map[string]any)
		of, _ := pm["ofWeekly"].(float64)
		if of <= 0 || of > 0.61 {
			t.Errorf("%v ofWeekly = %v, want a slice of the account's 61%% week", pm["name"], of)
		}
	}
	if len(usage.asks[spareID]) != 1 {
		t.Errorf("Spare read %d times, want only the period — it has no window to attribute", len(usage.asks[spareID]))
	}

	spare := accounts[1].(map[string]any)
	if spare["name"] != "Spare" || spare["hasReading"] != false || spare["weighted"] != 0.0 {
		t.Errorf("Spare row = %v, want it listed with no reading and no usage", spare)
	}
	if got, ok := spare["people"].([]any); !ok || len(got) != 0 {
		t.Errorf("Spare people = %v, want an empty list, not null", spare["people"])
	}
}

// The People section covers a rolling day, week, or month — never a calendar
// day — ranks whoever ran anything, and drops someone since removed.
func TestTheBoardRanksPeopleOverARollingWindow(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	usage.people = []SubjectUsage{
		{SubjectID: aliceID, Weighted: 100, ByModel: []ModelUsage{{Model: "claude-opus-5", Weighted: 100}}},
		{SubjectID: "someone-removed", Weighted: 5},
	}
	usage.latest = h.clock.Add(-time.Minute)

	code, body := h.do("GET", "/api/usage/people?window=month", nil, "")
	if code != 200 || body["window"] != "month" {
		t.Fatalf("people = %d %v", code, body)
	}
	subjects, _ := body["subjects"].([]any)
	if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != "Alice" {
		t.Errorf("subjects = %v, want Alice alone", subjects)
	}
	if got, want := usage.since["person"], h.clock.Add(-30*24*time.Hour); !got.Equal(want) {
		t.Errorf("month read since %v, want the last 30 days (%v)", got, want)
	}
	if body["asOf"] == nil || body["asOf"] == "0001-01-01T00:00:00Z" {
		t.Errorf("asOf = %v, want the newest event's time", body["asOf"])
	}

	// A window the board no longer has (the old 5-hour view) falls back to a day.
	if _, body := h.do("GET", "/api/usage/people?window=5h", nil, ""); body["window"] != "day" {
		t.Errorf("window=5h gave %v, want it read as a day", body["window"])
	}
	if got, want := usage.since["person"], h.clock.Add(-24*time.Hour); !got.Equal(want) {
		t.Errorf("day read since %v, want the last 24 hours (%v)", got, want)
	}
}

// A ceiling is the only kind of quota: a percent of an account's weekly window,
// on a person or an account — never the whole team — and the list shows each
// one's standing against the window it is actually checked against.
func TestACeilingIsAPercentOfAnAccountsWeeklyWindow(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	teamID := h.contribute(aliceTok, "Team Max", "team@example.com")

	if code, body := h.do("POST", "/api/limits", map[string]any{"subjectType": "org", "maxPercent": 60}, ""); code != 400 {
		t.Errorf("a team-wide ceiling = %d %v, want it refused", code, body)
	}
	if code, body := h.do("POST", "/api/limits", map[string]any{"subjectType": "person", "subjectId": aliceID, "maxPercent": 0}, ""); code != 400 {
		t.Errorf("a 0%% ceiling = %d %v, want it refused", code, body)
	}
	if code, body := h.do("POST", "/api/limits", map[string]any{"subjectType": "person", "subjectId": aliceID, "maxPercent": 60}, ""); code != 200 {
		t.Fatalf("setting a ceiling = %d %v", code, body)
	}
	if len(usage.limits) != 1 || usage.limits[0].MaxPercent != 0.6 {
		t.Fatalf("stored %+v, want one ceiling at 0.6", usage.limits)
	}

	reset := h.clock.Add(30 * time.Hour)
	usage.windows = []AccountWindow{{AccountID: teamID, SevenD: 0.45, SevenDReset: reset}}
	code, body := h.do("GET", "/api/limits", nil, "")
	if code != 200 {
		t.Fatalf("limits = %d %v", code, body)
	}
	rows, _ := body["limits"].([]any)
	if len(rows) != 1 {
		t.Fatalf("limits = %v, want the one", rows)
	}
	row := rows[0].(map[string]any)
	if row["name"] != "Alice" || row["windowAccount"] != "Team Max" || row["window"] != 0.45 {
		t.Errorf("row = %v, want Alice measured against Team Max's 45%% window", row)
	}
	if frac, _ := row["fraction"].(float64); frac < 0.749 || frac > 0.751 {
		t.Errorf("fraction = %v, want 0.45 of a 0.6 ceiling = 0.75", row["fraction"])
	}
	if row["resetAt"] == nil {
		t.Errorf("row = %v, want the window's own reset on it", row)
	}
}
