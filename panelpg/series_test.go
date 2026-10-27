package panelpg

import (
	"context"
	"math"
	"testing"
	"time"

	"clawdh/internal/meter"
	"clawdh/panel"
)

func series(t *testing.T, b *Backend, person, grain string, from time.Time) []panel.SeriesPoint {
	t.Helper()
	res, ok := panel.ResolutionByName(grain)
	if !ok {
		t.Fatalf("no %s grain", grain)
	}
	pts, err := b.PersonSeries(context.Background(), person, "", res, from)
	if err != nil {
		t.Fatalf("%s series: %v", grain, err)
	}
	return pts
}

func sum(pts []panel.SeriesPoint) (weighted float64, tokens, requests int64) {
	for _, p := range pts {
		weighted += p.Weighted
		tokens += p.Tokens
		requests += p.Requests
	}
	return
}

// The series is held to the events. Every request is recorded, then read back at
// every grain: the buckets add up to exactly what the events do, they sit where
// the events happened, and the hourly roll-up the longer grains read agrees with
// the raw events the finer ones read, to the token.
func TestEveryGrainAddsUpToTheEventsItWasCutFrom(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)

	// Wednesday 28 Oct 2026, and the days and weeks before it, for two people.
	type ev struct {
		at     time.Time
		person string
		w      float64
		tok    int64
	}
	day := func(m time.Month, d, h, min, s int) time.Time { return time.Date(2026, m, d, h, min, s, 0, time.UTC) }
	events := []ev{
		{day(10, 28, 15, 47, 33), "alice", 1000, 10},
		{day(10, 28, 15, 47, 33), "alice", 500, 5}, // same second as the one above
		{day(10, 28, 15, 47, 59), "alice", 250, 2},
		{day(10, 28, 15, 48, 1), "alice", 125, 1},
		{day(10, 28, 14, 5, 0), "alice", 2000, 20},
		{day(10, 27, 23, 59, 59), "alice", 4000, 40}, // the day before, last second
		{day(10, 26, 0, 0, 0), "alice", 8000, 80},    // the Monday of that week
		{day(10, 25, 12, 0, 0), "alice", 16000, 160}, // the Sunday before: last week
		{day(9, 30, 9, 0, 0), "alice", 32000, 320},   // last month
		{day(10, 28, 15, 47, 40), "bob", 64000, 640},
	}
	for i, e := range events {
		if err := b.RecordUsage(ctx, UsageEvent{
			At: e.at, PersonID: e.person, AccountID: "acct", Model: "claude-opus-5", Input: e.tok, Weighted: e.w,
			RequestID: e.person + e.at.String() + string(rune('a'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var wantAlice float64
	var wantAliceTok int64
	for _, e := range events {
		if e.person == "alice" {
			wantAlice += e.w
			wantAliceTok += e.tok
		}
	}
	long := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Every grain adds up to every event, exactly.
	for _, grain := range []string{"second", "minute", "hour", "day", "week", "month", "all"} {
		w, tok, _ := sum(series(t, b, "alice", grain, long))
		if w != wantAlice || tok != wantAliceTok {
			t.Errorf("%s adds up to %v weighted, %d tokens; the events are %v and %d", grain, w, tok, wantAlice, wantAliceTok)
		}
	}

	// Seconds: the two requests in one second are one bucket, and counted.
	secs := series(t, b, "alice", "second", day(10, 28, 15, 47, 0))
	if len(secs) != 3 || !secs[0].T.Equal(day(10, 28, 15, 47, 33)) || secs[0].Weighted != 1500 || secs[0].Requests != 2 {
		t.Errorf("seconds = %+v, want 15:47:33 holding both requests (1500 weighted, 2 requests) then :59 and :48:01", secs)
	}
	// Minutes: 15:47 holds the three before :48.
	mins := series(t, b, "alice", "minute", day(10, 28, 15, 47, 0))
	if len(mins) != 2 || mins[0].Weighted != 1750 || mins[0].Requests != 3 || mins[1].Weighted != 125 {
		t.Errorf("minutes = %+v, want 15:47 = 1750 over 3 requests and 15:48 = 125", mins)
	}
	// Hours are read from the roll-up, which counts no requests, and say so.
	hours := series(t, b, "alice", "hour", day(10, 28, 0, 0, 0))
	if len(hours) != 2 || hours[0].Weighted != 2000 || hours[1].Weighted != 1875 || hours[1].Requests != 0 {
		t.Errorf("hours = %+v, want 14:00 = 2000 and 15:00 = 1875 with no request count", hours)
	}
	// Days and weeks fall where the calendar says: the Monday begins a week and
	// the Sunday before it belongs to the last one.
	days := series(t, b, "alice", "day", day(10, 25, 0, 0, 0))
	if len(days) != 4 || days[0].Weighted != 16000 || days[1].Weighted != 8000 || days[2].Weighted != 4000 || days[3].Weighted != 3875 {
		t.Errorf("days = %+v, want 25th 16000, 26th 8000, 27th 4000, 28th 3875", days)
	}
	weeks := series(t, b, "alice", "week", long)
	if len(weeks) != 3 || !weeks[2].T.Equal(day(10, 26, 0, 0, 0)) || weeks[2].Weighted != 8000+4000+3875 || weeks[1].Weighted != 16000 {
		t.Errorf("weeks = %+v, want the week of the 26th = 15875, the week before = 16000, and September's before that", weeks)
	}
	months := series(t, b, "alice", "month", long)
	if len(months) != 2 || !months[0].T.Equal(day(9, 1, 0, 0, 0)) || months[0].Weighted != 32000 || months[1].Weighted != wantAlice-32000 {
		t.Errorf("months = %+v, want September 32000 and October the rest", months)
	}

	// From a point: nothing earlier comes back (an incremental read).
	if got := series(t, b, "alice", "minute", day(10, 28, 15, 48, 0)); len(got) != 1 || got[0].Weighted != 125 {
		t.Errorf("minutes from 15:48 = %+v, want only that bucket", got)
	}
	// One person is not another, and nobody named is everyone.
	if w, _, _ := sum(series(t, b, "bob", "all", long)); w != 64000 {
		t.Errorf("bob = %v, want his 64000 and none of Alice's", w)
	}
	if w, _, _ := sum(series(t, b, "", "all", long)); w != wantAlice+64000 {
		t.Errorf("everyone = %v, want %v", w, wantAlice+64000)
	}
	if got := series(t, b, "carol", "all", long); len(got) != 0 {
		t.Errorf("carol = %+v, want nothing", got)
	}
}

// The roll-up and the events are the same numbers, for a hundred requests
// scattered over a few weeks. A series that disagreed with the events would be
// the board being false in a new place.
func TestTheRollUpAndTheEventsAgreeForAScatteredRun(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	start := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	var want float64
	for i := 0; i < 100; i++ {
		at := start.Add(time.Duration(i*7919) * time.Minute) // a prime step spreads them over hours, days and weeks
		w := float64(100 + i*37)
		want += w
		if err := b.RecordUsage(ctx, UsageEvent{At: at, PersonID: "alice", AccountID: "a", Model: "m", Weighted: w, Input: int64(w), RequestID: at.String()}); err != nil {
			t.Fatal(err)
		}
	}
	var fromEvents float64
	if err := b.db.QueryRow(`SELECT SUM(weighted_tokens) FROM usage_events WHERE person_id = 'alice'`).Scan(&fromEvents); err != nil {
		t.Fatal(err)
	}
	if math.Abs(fromEvents-want) > 1e-6 {
		t.Fatalf("events hold %v, recorded %v", fromEvents, want)
	}
	long := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, grain := range []string{"minute", "hour", "day", "week", "month", "all"} {
		if w, _, _ := sum(series(t, b, "alice", grain, long)); math.Abs(w-want) > 1e-6 {
			t.Errorf("%s adds up to %v, the events to %v", grain, w, want)
		}
	}
}

// Usage on a login the panel has no trace of is reported, and usage on an earlier
// ID of a login that is still there is not: that is the login's own history.
func TestOnlyUsageOnALoginWithNoTraceIsUnlinked(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for _, ev := range []struct {
		account string
		w       float64
	}{{"current", 1000}, {"earlier-id", 200}, {"ghost", 30}} {
		if err := b.RecordUsage(ctx, UsageEvent{At: at, PersonID: "p", AccountID: ev.account, Model: "m", Weighted: ev.w, RequestID: ev.account}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"current", "earlier-id"} {
		if err := b.LinkLogin(ctx, id, "login@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.UnlinkedAccountUsage(ctx, []string{"current"}, at.Add(-time.Hour))
	if err != nil || got != 30 {
		t.Errorf("unlinked = %v (%v), want only the ghost's 30", got, err)
	}
	if got, _ := b.UnlinkedAccountUsage(ctx, []string{"current"}, at.Add(time.Hour)); got != 0 {
		t.Errorf("unlinked since after the events = %v, want 0", got)
	}
	// Without the link, the earlier ID is no one's.
	if _, err := b.db.Exec(`DELETE FROM account_lineage WHERE account_id = 'earlier-id'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.UnlinkedAccountUsage(ctx, nil, at.Add(-time.Hour)); got != 1230 {
		t.Errorf("unlinked with no current logins = %v, want everything (1230)", got)
	}
}

// Every bucket is split by token class, and the split reconciles: the classes'
// raw tokens add up to the bucket's, their weighted shares to the weighted tokens
// the gateway recorded (even where that was priced at an older rate), each class
// weighs what its model's rate says, and a model with no known rate adds its
// tokens but no weight. The events and the roll-up split the same way.
func TestEveryBucketSplitsByTokenClassAndAddsUp(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	at := time.Date(2026, 10, 28, 9, 15, 7, 0, time.UTC)
	opus := meter.Usage{Input: 2, Output: 300, CacheCreation: 1000, CacheRead: 200000}
	sonnet := meter.Usage{Input: 10, Output: 50, CacheCreation: 0, CacheRead: 40000}
	odd := meter.Usage{Input: 7, CacheRead: 70}
	for i, ev := range []struct {
		model    string
		u        meter.Usage
		weighted float64
	}{
		{"claude-opus-5-5", opus, meter.Measure("claude-opus-5-5", opus).Weighted},
		{"claude-sonnet-4-6", sonnet, 2 * meter.Measure("claude-sonnet-4-6", sonnet).Weighted}, // priced at an older, doubled rate
		{"no-such-model", odd, 0},
	} {
		if err := b.RecordUsage(ctx, UsageEvent{
			At: at, PersonID: "alice", AccountID: "a", Model: ev.model, Weighted: ev.weighted,
			Input: ev.u.Input, Output: ev.u.Output, CacheCreation: ev.u.CacheCreation, CacheRead: ev.u.CacheRead,
			RequestID: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	op := meter.WeightedParts("claude-opus-5-5", opus)
	sp := meter.WeightedParts("claude-sonnet-4-6", sonnet)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	for _, grain := range []string{"second", "hour"} {
		pts := series(t, b, "alice", grain, at.Add(-time.Hour))
		if len(pts) != 1 {
			t.Fatalf("%s: %d buckets, want 1", grain, len(pts))
		}
		p := pts[0]
		if p.Input.Tokens != 19 || p.Output.Tokens != 350 || p.CacheWrite.Tokens != 1000 || p.CacheRead.Tokens != 240070 {
			t.Errorf("%s raw classes = %+v %+v %+v %+v, want 19 / 350 / 1000 / 240070", grain, p.Input, p.Output, p.CacheWrite, p.CacheRead)
		}
		if p.Input.Tokens+p.Output.Tokens+p.CacheWrite.Tokens+p.CacheRead.Tokens != p.Tokens {
			t.Errorf("%s: the classes' tokens do not add up to the bucket's %d", grain, p.Tokens)
		}
		if got := p.Input.Weighted + p.Output.Weighted + p.CacheWrite.Weighted + p.CacheRead.Weighted; !near(got, p.Weighted) {
			t.Errorf("%s: the classes weigh %v, the bucket %v", grain, got, p.Weighted)
		}
		if !near(p.CacheRead.Weighted, op.CacheRead+2*sp.CacheRead) || !near(p.Output.Weighted, op.Output+2*sp.Output) {
			t.Errorf("%s: cache read %v / output %v, want %v / %v", grain, p.CacheRead.Weighted, p.Output.Weighted, op.CacheRead+2*sp.CacheRead, op.Output+2*sp.Output)
		}
		if grain == "second" && p.Requests != 3 {
			t.Errorf("seconds count %d requests, want 3 across the models", p.Requests)
		}
	}
}

// A session is the requests one Claude Code session made: its series holds only
// those, at every grain (the roll-up knows no sessions, so it is read from the
// events), and the sessions list names each one newest first with what it came
// to. Requests that named no session are no session's.
func TestASessionIsOnlyItsOwnRequests(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	resetLineage(t, b)
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for i, ev := range []struct {
		person, session string
		min             int
		w               float64
		read            int64
	}{
		{"alice", "s-old", 0, 100, 1000},
		{"alice", "s-old", 5, 200, 2000},
		{"alice", "s-new", 10, 400, 4000},
		{"alice", "", 11, 800, 8000},      // named no session
		{"bob", "s-new", 12, 1600, 16000}, // someone else's, same id
	} {
		if err := b.RecordUsage(ctx, UsageEvent{
			At: at.Add(time.Duration(ev.min) * time.Minute), PersonID: ev.person, AccountID: "a", Model: "claude-opus-5-5",
			Weighted: ev.w, CacheRead: ev.read, SessionID: ev.session, RequestID: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, grain := range []string{"minute", "hour", "all"} {
		res, _ := panel.ResolutionByName(grain)
		pts, err := b.PersonSeries(ctx, "alice", "s-old", res, at.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		w, tok, req := sum(pts)
		if w != 300 || tok != 3000 || req != 2 {
			t.Errorf("%s: s-old came to %v weighted, %d tokens, %d requests; want 300, 3000, 2", grain, w, tok, req)
		}
	}
	list, err := b.PersonSessions(ctx, "alice", at.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "s-new" || list[1].ID != "s-old" {
		t.Fatalf("sessions = %+v, want s-new then s-old", list)
	}
	if list[0].Usage.Weighted != 400 || list[0].Usage.Requests != 1 {
		t.Errorf("s-new = %+v, want only Alice's 400 over 1 request", list[0].Usage)
	}
	old := list[1]
	if !old.First.Equal(at) || !old.Last.Equal(at.Add(5*time.Minute)) || old.Usage.CacheRead.Tokens != 3000 || old.Usage.Requests != 2 {
		t.Errorf("s-old = %+v, want 09:00–09:05, 3000 cache-read tokens over 2 requests", old)
	}
	if got, _ := b.PersonSessions(ctx, "alice", at.Add(-time.Hour), 1); len(got) != 1 || got[0].ID != "s-new" {
		t.Errorf("limit 1 = %+v, want only the newest", got)
	}
}
