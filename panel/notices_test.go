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
		th, account string
		want        string
	}{
		{"75", "ehtisham@devhouse.co", "ehtisham@devhouse.co's weekly window is at 75% of your ceiling"},
		{"95", "Team Max", "Team Max's weekly window is at 95% of your ceiling"},
		{"cap", "Team Max", "Team Max's weekly window is past your ceiling — you're turned away until it resets"},
	} {
		if got := quotaBody(tc.th, tc.account); got != tc.want {
			t.Errorf("quotaBody(%q,%q) = %q, want %q", tc.th, tc.account, got, tc.want)
		}
	}
}

// fakeUsage is a UsageReader with canned answers: the ceilings and window
// readings the notices and quota list read, and the per-person and per-account
// usage the board reads. It remembers the `since` each board read asked for,
// so a test can check which period a number covers.
type fakeUsage struct {
	limits     []Limit
	collisions map[string]time.Time // account ID -> when its login broke
	windows    []AccountWindow      // the accounts' captured weekly windows, which ceilings read
	people     []SubjectUsage       // everyone's usage, whatever the window
	ranBy      map[string][]SubjectUsage
	latest     time.Time
	since      map[string]time.Time // subject asked about ("people" or an account id) -> the last since
	// asks is every `since` the board requested per subject. An account is read
	// twice now — once for the period on screen, once scoped to the weekly
	// window that the per-person percentages are a share of — so the single
	// `since` above keeps the first (the period) and this keeps both.
	asks map[string][]time.Time
}

func (f *fakeUsage) asked(key string, since time.Time) {
	if f.since == nil {
		f.since = map[string]time.Time{}
	}
	if f.asks == nil {
		f.asks = map[string][]time.Time{}
	}
	f.since[key] = since
	f.asks[key] = append(f.asks[key], since)
}

func (f *fakeUsage) ListLimits(context.Context) ([]Limit, error) { return f.limits, nil }
func (f *fakeUsage) SetLimit(_ context.Context, l Limit) error {
	f.limits = append(f.limits, l)
	return nil
}
func (f *fakeUsage) DeleteLimit(_ context.Context, id string) error {
	kept := f.limits[:0]
	for _, l := range f.limits {
		if l.ID != id {
			kept = append(kept, l)
		}
	}
	f.limits = kept
	return nil
}
func (f *fakeUsage) RecentCollisions(context.Context, time.Time) (map[string]time.Time, error) {
	return f.collisions, nil
}
func (f *fakeUsage) UsageBySubject(_ context.Context, kind string, since time.Time) ([]SubjectUsage, error) {
	f.asked(kind, since)
	return f.people, nil
}
func (f *fakeUsage) AccountUsageByPerson(_ context.Context, accountID string, since time.Time) ([]SubjectUsage, error) {
	f.asked(accountID, since)
	return f.ranBy[accountID], nil
}
func (f *fakeUsage) LatestEventAt(context.Context) (time.Time, error)        { return f.latest, nil }
func (f *fakeUsage) AccountWindows(context.Context) ([]AccountWindow, error) { return f.windows, nil }

// The panel works out a checking-in person's notices from the metering it holds:
// their own tightest quota (not someone else's), and any account they can use
// whose shared login just broke.
func TestCheckinNotices(t *testing.T) {
	reset := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	colAt := time.Date(2026, 9, 19, 3, 0, 0, 0, time.UTC)
	usage := &fakeUsage{
		limits: []Limit{
			{ID: "l1", SubjectType: "person", SubjectID: "p1", MaxPercent: 0.5},
			{ID: "l2", SubjectType: "person", SubjectID: "p2", MaxPercent: 0.4}, // someone else's, and tighter
		},
		windows:    []AccountWindow{{AccountID: "a1", SevenD: 0.48, SevenDReset: reset}}, // 0.48 of 0.5: 96%
		collisions: map[string]time.Time{"a1": colAt},
	}
	s := &Server{usage: usage, now: func() time.Time { return colAt.Add(time.Hour) }}
	d := Data{
		Accounts: []Account{{ID: "a1", Name: "ehtisham@devhouse.co"}},
		Shares:   []Share{{AccountID: "a1", PersonID: "p1"}, {AccountID: "a1", PersonID: "p2"}},
	}

	got := s.checkinNotices(context.Background(), "p1", d)
	if len(got) != 2 {
		t.Fatalf("p1 got %d notices, want 2 (quota + collision): %+v", len(got), got)
	}
	// p1's own ceiling (96% of it), not p2's tighter one.
	if got[0].Body != "ehtisham@devhouse.co's weekly window is at 95% of your ceiling" {
		t.Errorf("quota notice = %q, want the 95%% line off p1's own ceiling", got[0].Body)
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
	usage := &fakeUsage{
		limits: []Limit{{ID: "c1", SubjectType: "person", SubjectID: "p1", MaxPercent: 0.5}},
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
	if got[0].Body != "two's weekly window is at 75% of your ceiling" {
		t.Errorf("notice = %q, want it to name the account whose window it is", got[0].Body)
	}
	if got[0].ID != "quota:75:"+strconv.FormatInt(windowReset.Unix(), 10) {
		t.Errorf("notice ID = %q, want it keyed by the window's own reset", got[0].ID)
	}
}
