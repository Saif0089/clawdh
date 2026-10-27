package panel

import (
	"testing"
	"time"
)

// A bucket is cut the way the database cuts it, in UTC, with weeks beginning on
// Monday — so a view's first bucket and the rows the database groups agree.
func TestAGrainCutsBucketsTheWayTheDatabaseDoes(t *testing.T) {
	at := time.Date(2026, 10, 28, 15, 47, 33, 500, time.UTC) // a Wednesday
	for name, want := range map[string]time.Time{
		"second": time.Date(2026, 10, 28, 15, 47, 33, 0, time.UTC),
		"minute": time.Date(2026, 10, 28, 15, 47, 0, 0, time.UTC),
		"hour":   time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC),
		"day":    time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		"week":   time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), // the Monday
		"month":  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	} {
		res, ok := ResolutionByName(name)
		if !ok {
			t.Fatalf("no %s grain", name)
		}
		if got := res.truncate(at); !got.Equal(want) {
			t.Errorf("%s bucket = %v, want %v", name, got, want)
		}
	}
	// A Sunday belongs to the week that began the Monday before it.
	week, _ := ResolutionByName("week")
	if got, want := week.truncate(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Sunday's week begins %v, want %v", got, want)
	}

	// A view reaches its span back; an incremental read starts at the bucket the
	// client already holds the end of, and never earlier than the view itself.
	now := time.Date(2026, 10, 27, 12, 30, 10, 0, time.UTC)
	minute, _ := ResolutionByName("minute")
	if got, want := minute.from(now, time.Time{}), time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("minute view starts %v, want two hours back (%v)", got, want)
	}
	if got, want := minute.from(now, now.Add(-90*time.Second)), time.Date(2026, 10, 27, 12, 28, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("incremental read starts %v, want the bucket it holds (%v)", got, want)
	}
	if got, want := minute.from(now, now.Add(-30*time.Hour)), time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("a stale cursor starts %v, want no earlier than the view (%v)", got, want)
	}
	all, _ := ResolutionByName("all")
	if got := all.from(now, time.Time{}); !got.IsZero() {
		t.Errorf("all starts %v, want the beginning of the history", got)
	}
}

// The admin reads one person's usage over time at the grain they ask, or
// everyone's, and a grain or person that does not exist is refused.
func TestTheBoardServesAPersonsUsageAtTheAskedGrain(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	usage.series = []SeriesPoint{
		{T: h.clock.Add(-2 * time.Minute).Truncate(time.Minute), Weighted: 1200, Tokens: 9000, Requests: 14},
		{T: h.clock.Add(-time.Minute).Truncate(time.Minute), Weighted: 300, Tokens: 2200, Requests: 3},
	}

	code, body := h.do("GET", "/api/usage/telemetry?res=minute&person="+aliceID, nil, "")
	if code != 200 || body["res"] != "minute" || body["person"] != aliceID || body["bucketSeconds"] != 60.0 {
		t.Fatalf("telemetry = %d %v", code, body)
	}
	pts, _ := body["points"].([]any)
	if len(pts) != 2 || pts[0].(map[string]any)["weighted"] != 1200.0 || pts[0].(map[string]any)["requests"] != 14.0 {
		t.Errorf("points = %v, want the two buckets as metered", body["points"])
	}
	if usage.seriesPerson != aliceID || usage.seriesRes != "minute" || !usage.seriesFrom.Equal(h.clock.Add(-2*time.Hour).Truncate(time.Minute)) {
		t.Errorf("asked for %q at %q from %v, want Alice's minutes from two hours back", usage.seriesPerson, usage.seriesRes, usage.seriesFrom)
	}

	// Nothing named is everyone; an empty answer is an empty list, never null.
	usage.series = nil
	_, body = h.do("GET", "/api/usage/telemetry?res=week", nil, "")
	if usage.seriesPerson != "" {
		t.Errorf("asked for person %q, want everyone", usage.seriesPerson)
	}
	if pts, ok := body["points"].([]any); !ok || len(pts) != 0 {
		t.Errorf("points = %v, want an empty list", body["points"])
	}

	// Syncing: a client sends the end of what it holds and is sent from there.
	cursor := h.clock.Add(-95 * time.Second).UTC().Format(time.RFC3339)
	h.do("GET", "/api/usage/telemetry?res=second&since="+cursor, nil, "")
	if want := h.clock.Add(-95 * time.Second).Truncate(time.Second); !usage.seriesFrom.Equal(want) {
		t.Errorf("incremental read started at %v, want %v", usage.seriesFrom, want)
	}

	for path, want := range map[string]int{
		"/api/usage/telemetry?res=fortnight":           400,
		"/api/usage/telemetry":                         400,
		"/api/usage/telemetry?res=hour&since=whenever": 400,
		"/api/usage/telemetry?res=hour&person=nobody":  404,
	} {
		if code, _ := h.do("GET", path, nil, ""); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
}

// A machine reads its own person's usage with its enrolment, and cannot ask for
// anyone else's: the person is the machine's, never a parameter.
func TestAMachineReadsOnlyItsOwnUsageOverTime(t *testing.T) {
	usage := &fakeUsage{series: []SeriesPoint{{T: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), Weighted: 5e6, Tokens: 40e6}}}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, _ := h.join("Bob", "bob-pc")

	code, body := h.do("GET", "/api/v1/telemetry?res=day&person="+bobID, nil, aliceTok)
	if code != 200 || usage.seriesPerson != aliceID {
		t.Fatalf("telemetry = %d %v, asked for person %q, want Alice's own (%q) whatever was passed", code, body, usage.seriesPerson, aliceID)
	}
	if pts, _ := body["points"].([]any); len(pts) != 1 {
		t.Errorf("points = %v", body["points"])
	}
	if code, _ := h.do("GET", "/api/v1/telemetry?res=day", nil, ""); code != 401 {
		t.Errorf("without an enrolment = %d, want 401", code)
	}
	if code, _ := h.do("GET", "/api/v1/telemetry?res=day", nil, "not-a-token"); code != 401 {
		t.Errorf("with an unknown token = %d, want 401", code)
	}
}

// Usage that has no card — a removed person's, a login the panel has no trace of
// — is reported, not dropped, so the board's lists add up to what was metered.
func TestTheTeamBoardsReportUsageThatHasNoCard(t *testing.T) {
	usage := &fakeUsage{unlinked: 42e6}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	usage.people = []SubjectUsage{
		{SubjectID: aliceID, Weighted: 100e6},
		{SubjectID: "someone-removed", Weighted: 7e6},
		{SubjectID: "another-removed", Weighted: 3e6},
	}

	_, body := h.do("GET", "/api/usage/people?window=week", nil, "")
	if body["removedPeople"] != 10e6 {
		t.Errorf("removedPeople = %v, want the 10M two removed people ran", body["removedPeople"])
	}
	if subjects, _ := body["subjects"].([]any); len(subjects) != 1 {
		t.Errorf("subjects = %v, want only Alice, with no ghost cards", body["subjects"])
	}
	_, body = h.do("GET", "/api/usage/accounts?window=week", nil, "")
	if body["unlinkedAccounts"] != 42e6 {
		t.Errorf("unlinkedAccounts = %v, want the 42M on logins the panel has no trace of", body["unlinkedAccounts"])
	}
}

// The admin reads one named person's sessions; a person is required, and one
// that does not exist is refused.
func TestTheBoardListsAPersonsSessions(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	at := h.clock.Add(-time.Hour)
	usage.sessions = []SessionUsage{{ID: "607f886f-fb57", First: at, Last: h.clock, Usage: SeriesPoint{T: h.clock, Weighted: 900, Requests: 4}}}

	code, body := h.do("GET", "/api/usage/telemetry/sessions?person="+aliceID, nil, "")
	list, _ := body["sessions"].([]any)
	if code != 200 || usage.seriesPerson != aliceID || len(list) != 1 || list[0].(map[string]any)["id"] != "607f886f-fb57" {
		t.Fatalf("sessions = %d %v (asked for %q), want Alice's one session", code, body, usage.seriesPerson)
	}
	if code, _ := h.do("GET", "/api/usage/telemetry/sessions", nil, ""); code != 400 {
		t.Errorf("without a person = %d, want 400", code)
	}
	if code, _ := h.do("GET", "/api/usage/telemetry/sessions?person=nobody", nil, ""); code != 404 {
		t.Errorf("an unknown person = %d, want 404", code)
	}
	usage.sessions = nil
	if _, body := h.do("GET", "/api/usage/telemetry/sessions?person="+aliceID, nil, ""); body["sessions"] == nil {
		t.Errorf("no sessions = %v, want an empty list, never null", body)
	}
}

// A session is read over its own span: a minute view of a session that ran days
// ago starts where the client says it began, not two hours back from now.
func TestASessionIsReadOverItsOwnSpan(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	began := h.clock.Add(-72 * time.Hour).Add(17 * time.Second)
	h.do("GET", "/api/usage/telemetry?res=minute&person="+aliceID+"&session=s1&since="+began.UTC().Format(time.RFC3339), nil, "")
	if usage.seriesSess != "s1" || !usage.seriesFrom.Equal(began.Truncate(time.Minute)) {
		t.Errorf("asked for session %q from %v, want s1 from %v", usage.seriesSess, usage.seriesFrom, began.Truncate(time.Minute))
	}
	h.do("GET", "/api/usage/telemetry?res=hour&person="+aliceID+"&session=s1", nil, "")
	if !usage.seriesFrom.IsZero() {
		t.Errorf("a session with no start given read from %v, want all of it", usage.seriesFrom)
	}
	if code, _ := h.do("GET", "/api/usage/telemetry?res=hour&session=s1", nil, ""); code != 400 {
		t.Errorf("a session with no person = %d, want 400", code)
	}
}
