package panel

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQuotaThreshold(t *testing.T) {
	for _, tc := range []struct {
		frac float64
		want string
	}{
		{0, ""}, {0.5, ""}, {0.749, ""},
		{0.75, "75"}, {0.9, "75"},
		{0.95, "95"}, {0.999, "95"},
		{1.0, "cap"}, {1.4, "cap"},
	} {
		if got := quotaThreshold(tc.frac); got != tc.want {
			t.Errorf("quotaThreshold(%v) = %q, want %q", tc.frac, got, tc.want)
		}
	}
}

func TestQuotaBody(t *testing.T) {
	for _, tc := range []struct {
		th, window string
		ceiling    bool
		want       string
	}{
		{"75", "week", false, "You're at 75% of your weekly limit"},
		{"95", "day", false, "You're at 95% of your daily limit"},
		{"cap", "month", false, "You're over your monthly limit"},
		{"cap", "", false, "You're over your limit"}, // unknown window: no double space
		{"75", "week", true, "The account's weekly window is at 75% of your ceiling"},
		{"cap", "week", true, "The account's weekly window is past your ceiling — you're turned away until it resets"},
	} {
		if got := quotaBody(tc.th, tc.window, tc.ceiling); got != tc.want {
			t.Errorf("quotaBody(%q,%q,%v) = %q, want %q", tc.th, tc.window, tc.ceiling, got, tc.want)
		}
	}
}

// fakeUsage is a UsageReader that answers only the three reads notices use; the
// rest of the board surface is unused here and returns nothing.
type fakeUsage struct {
	limits     []Limit
	frac       map[string]float64 // limit ID -> current utilisation
	reset      time.Time
	collisions map[string]time.Time // account ID -> when its login broke
	windows    []AccountWindow      // the accounts' captured weekly windows, for ceilings
}

func (f *fakeUsage) ListLimits(context.Context) ([]Limit, error) { return f.limits, nil }
func (f *fakeUsage) LimitUsage(_ context.Context, l Limit) (float64, time.Time, error) {
	return f.frac[l.ID], f.reset, nil
}
func (f *fakeUsage) RecentCollisions(context.Context, time.Time) (map[string]time.Time, error) {
	return f.collisions, nil
}
func (f *fakeUsage) UsageBySubject(context.Context, string, time.Time) ([]SubjectUsage, error) {
	return nil, nil
}
func (f *fakeUsage) AccountUsageByPerson(context.Context, string, time.Time) ([]SubjectUsage, error) {
	return nil, nil
}
func (f *fakeUsage) HourlyTotals(context.Context, string, string, time.Time) ([]HourBucket, error) {
	return nil, nil
}
func (f *fakeUsage) LatestEventAt(context.Context) (time.Time, error)        { return time.Time{}, nil }
func (f *fakeUsage) SetLimit(context.Context, Limit) error                   { return nil }
func (f *fakeUsage) DeleteLimit(context.Context, string) error               { return nil }
func (f *fakeUsage) AccountWindows(context.Context) ([]AccountWindow, error) { return f.windows, nil }

// The panel works out a checking-in person's notices from the metering it holds:
// their own tightest quota (not someone else's), and any account they can use
// whose shared login just broke.
func TestCheckinNotices(t *testing.T) {
	reset := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	colAt := time.Date(2026, 9, 19, 3, 0, 0, 0, time.UTC)
	usage := &fakeUsage{
		limits: []Limit{
			{ID: "l1", SubjectType: "person", SubjectID: "p1", WindowKind: "week"},
			{ID: "l2", SubjectType: "person", SubjectID: "p2", WindowKind: "week"}, // someone else's
		},
		frac:       map[string]float64{"l1": 0.96, "l2": 0.99},
		reset:      reset,
		collisions: map[string]time.Time{"a1": colAt},
	}
	s := &Server{usage: usage, now: func() time.Time { return colAt.Add(time.Hour) }}
	d := Data{
		Accounts: []Account{{ID: "a1", Name: "ehtisham@devhouse.co"}},
		Shares:   []Share{{AccountID: "a1", PersonID: "p1"}},
	}

	got := s.checkinNotices(context.Background(), "p1", d)
	if len(got) != 2 {
		t.Fatalf("p1 got %d notices, want 2 (quota + collision): %+v", len(got), got)
	}
	// p1's own 96% limit, not p2's 99% one.
	if !strings.Contains(got[0].Body, "95% of your weekly limit") {
		t.Errorf("quota notice = %q, want the 95%% weekly line off p1's own limit", got[0].Body)
	}
	if got[0].ID != "quota:95:"+strconv.FormatInt(reset.Unix(), 10) {
		t.Errorf("quota notice ID = %q, want it keyed by threshold+reset", got[0].ID)
	}
	if !strings.Contains(got[1].Body, "ehtisham@devhouse.co") || !strings.Contains(got[1].Body, "login broke") {
		t.Errorf("collision notice = %q, want it name the account", got[1].Body)
	}

	// A person who shares nothing broken and has no limit gets nothing.
	if n := s.checkinNotices(context.Background(), "p3", d); len(n) != 0 {
		t.Errorf("p3 (no limit, no shared account) got %d notices, want 0: %+v", len(n), n)
	}

	// No metering wired (a file-backed panel): no server notices at all.
	if n := (&Server{now: time.Now}).checkinNotices(context.Background(), "p1", d); n != nil {
		t.Errorf("a panel with no usage reader must yield no notices, got %+v", n)
	}
}

// A person's window ceiling is measured against the fullest weekly window among
// the accounts they can use — the metering layer has no account in hand for a
// person, so the notice does what the board does. The notice is worded for a
// window, and keyed by the window's own reset.
func TestCheckinNoticesMeasureAPersonsCeilingAgainstTheirAccounts(t *testing.T) {
	windowReset := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	pct := 0.5
	usage := &fakeUsage{
		limits: []Limit{{ID: "c1", SubjectType: "person", SubjectID: "p1", WindowKind: "week", MaxPercent: &pct}},
		reset:  time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), // the calendar week, which a ceiling ignores
		windows: []AccountWindow{
			{AccountID: "a1", SevenD: 0.2},
			{AccountID: "a2", SevenD: 0.4, SevenDReset: windowReset}, // 0.4 of a 0.5 ceiling: 80%
			{AccountID: "a3", SevenD: 0.9},                           // not shared with p1
		},
	}
	s := &Server{usage: usage, now: time.Now}
	d := Data{
		Accounts: []Account{{ID: "a1", Name: "one"}, {ID: "a2", Name: "two"}, {ID: "a3", Name: "three"}},
		Shares:   []Share{{AccountID: "a1", PersonID: "p1"}, {AccountID: "a2", PersonID: "p1"}, {AccountID: "a3", PersonID: "p2"}},
	}

	got := s.checkinNotices(context.Background(), "p1", d)
	if len(got) != 1 {
		t.Fatalf("got %d notices, want the one ceiling notice: %+v", len(got), got)
	}
	if got[0].Body != "The account's weekly window is at 75% of your ceiling" {
		t.Errorf("notice = %q", got[0].Body)
	}
	if got[0].ID != "quota:75:"+strconv.FormatInt(windowReset.Unix(), 10) {
		t.Errorf("notice ID = %q, want it keyed by the window's own reset", got[0].ID)
	}
}
