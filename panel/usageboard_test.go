package panel

import (
	"testing"
	"time"
)

// The board's Accounts section lists every account with a login — one the
// gateway has not read yet included, flagged, so "not read" never looks like
// "unused" — and names who filled each weekly window, over the seven days that
// window actually covers rather than the last seven on the clock.
func TestTheBoardListsEveryAccountAndWhoFilledItsWindow(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, bobTok := h.join("Bob", "bob-pc")
	teamID := h.contribute(aliceTok, "Team Max", "team@example.com")
	spareID := h.contribute(bobTok, "Spare", "spare@example.com")

	reset := h.clock.Add(50 * time.Hour)
	usage.windows = []AccountWindow{{AccountID: teamID, FiveH: 0.12, SevenD: 0.61, SevenDReset: reset, UpdatedAt: h.clock}}
	usage.ranBy = map[string][]SubjectUsage{teamID: {
		{SubjectID: bobID, Weighted: 300},
		{SubjectID: aliceID, Weighted: 100},
		{SubjectID: "someone-removed", Weighted: 999},
	}}

	code, body := h.do("GET", "/api/usage/windows", nil, "")
	if code != 200 {
		t.Fatalf("windows = %d %v", code, body)
	}
	windows, _ := body["windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("got %d accounts, want both (one without a reading): %v", len(windows), body)
	}
	team := windows[0].(map[string]any)
	if team["name"] != "Team Max" || team["hasReading"] != true || team["sevenD"] != 0.61 {
		t.Errorf("Team Max row = %v", team)
	}
	ranBy, _ := team["ranBy"].([]any)
	if len(ranBy) != 2 || ranBy[0].(map[string]any)["name"] != "Bob" || ranBy[1].(map[string]any)["name"] != "Alice" {
		t.Errorf("ranBy = %v, want Bob then Alice and nobody removed", ranBy)
	}
	if got, want := usage.since[teamID], reset.Add(-7*24*time.Hour); !got.Equal(want) {
		t.Errorf("Team Max's window was read since %v, want the seven days before its reset (%v)", got, want)
	}

	spare := windows[1].(map[string]any)
	if spare["name"] != "Spare" || spare["hasReading"] != false {
		t.Errorf("Spare row = %v, want it listed with no reading", spare)
	}
	if got, ok := spare["ranBy"].([]any); !ok || len(got) != 0 {
		t.Errorf("Spare ranBy = %v, want an empty list, not null", spare["ranBy"])
	}
	if got, want := usage.since[spareID], h.clock.Add(-7*24*time.Hour); !got.Equal(want) {
		t.Errorf("with no reading, Spare was read since %v, want the last seven days (%v)", got, want)
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
