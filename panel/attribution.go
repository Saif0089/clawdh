package panel

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Who spent an account's week.
//
// Two different instruments describe one weekly window. Anthropic says how full
// the plan is — a real figure, in whole percents. The gateway says which tokens
// it saw go through, and whose they were. They describe the same thing only if
// the gateway saw the whole week. When it did not — a login re-added mid-week
// starts a new account ID with no history, a login used outside the gateway, a
// stretch the database missed — the part of the plan's week that no metered
// request explains is simply not known.
//
// The board used to divide Anthropic's figure across whoever the gateway did
// see, which is only right when nothing is missing. Everything missing was
// charged, in proportion, to the people who happened to be metered: after a
// re-add, someone who ran 15M tokens read as 11% of a week that a colleague had
// spent 880M of an hour earlier. So the two are kept apart. A person's share is
// their weighted tokens over the number of weighted tokens that make a plan's
// whole week — a size measured from the plan's own finished weeks, never
// assumed — and whatever is left of Anthropic's figure is reported as what it
// is: not metered.

// How a plan's size was learned, for the board to say so.
const (
	// ScaleHistory: the median of the plan's own finished, fully metered weeks.
	ScaleHistory = "history"
	// ScaleThisWeek: no finished week to learn from yet, so this week itself —
	// which is sound only because the gateway has watched the login since
	// before the week began, so nothing it could have seen is missing. Nothing
	// is then left over; use of the login outside the gateway, which no
	// instrument here can see, would be absorbed into the size. That is why it
	// gives way to the plan's finished weeks as soon as there are any.
	ScaleThisWeek = "this-week"
	// ScaleNone: neither. Tokens are shown exactly; no percentage is invented.
	ScaleNone = "none"
)

const (
	// minScaleUtil is the smallest reading a plan's size is learned from.
	// Utilisation comes in whole percents, so at 10% one point is a tenth of
	// the figure; from 25% up the rounding is at most a fifth of a point in
	// the size per point of reading.
	minScaleUtil = 0.25
	// scaleWeeks is how many finished weeks the size is the median of: enough
	// that one odd week (a model-heavy one, an outage) does not move it, few
	// enough that a plan change is followed within a month.
	scaleWeeks = 4
)

// WeekSample is what one weekly window taught about a plan: how full Anthropic
// said it was, and the weighted tokens the gateway metered inside it, whoever
// ran them. Divided, they give the plan's size in the gateway's own unit.
type WeekSample struct {
	// Reset is the window's reset, rounded to the hour: the same window is read
	// from response headers and from the usage endpoint with a few milliseconds
	// between them, and those must land on one row.
	Reset    time.Time `json:"reset"`
	Util     float64   `json:"util"`     // 0..1
	Weighted float64   `json:"weighted"` // every metered token inside the window
}

// weekKey is the instant a window's samples are filed under.
func weekKey(reset time.Time) time.Time { return reset.UTC().Round(time.Hour) }

// weekScale is how many weighted tokens make a plan's whole week. It prefers the
// plan's own finished weeks (samples, newest first — the window still running is
// not one of them), because a finished week's reading is the fullest and its
// tokens are all in. Only without any does it fall back to cur, the running week.
func weekScale(samples []WeekSample, cur *WeekSample, now time.Time) (tokens float64, basis string) {
	var sizes []float64
	for _, s := range samples {
		if len(sizes) == scaleWeeks {
			break
		}
		if s.Reset.After(now) || s.Util < minScaleUtil || s.Weighted <= 0 {
			continue
		}
		sizes = append(sizes, s.Weighted/s.Util)
	}
	if n := len(sizes); n > 0 {
		sort.Float64s(sizes)
		if n%2 == 1 {
			return sizes[n/2], ScaleHistory
		}
		return (sizes[n/2-1] + sizes[n/2]) / 2, ScaleHistory
	}
	if cur != nil && cur.Util >= minScaleUtil && cur.Weighted > 0 {
		return cur.Weighted / cur.Util, ScaleThisWeek
	}
	return 0, ScaleNone
}

// splitWeek turns each weighted-token figure into a share of the plan's week —
// tokens over the plan's size — and returns what is left of Anthropic's own
// figure (sevenD, 0..1) that none of them explains. That remainder is never
// handed to anyone.
//
// The shares can never add up to more than Anthropic reports: if the size
// learned from earlier weeks runs a little small (the people used the login
// outside the gateway in those weeks too) they are scaled down to fit, and the
// remainder is then zero.
func splitWeek(sevenD, tokens float64, weighted []float64) (shares []float64, unaccounted float64) {
	shares = make([]float64, len(weighted))
	if sevenD <= 0 {
		return shares, 0
	}
	if tokens <= 0 {
		return shares, sevenD
	}
	var sum float64
	for i, w := range weighted {
		shares[i] = w / tokens
		sum += shares[i]
	}
	if sum > sevenD {
		f := sevenD / sum
		for i := range shares {
			shares[i] *= f
		}
		sum = sevenD
	}
	if rest := sevenD - sum; rest > 1e-9 {
		unaccounted = rest
	}
	return shares, unaccounted
}

// loginKey is what identifies one Claude login across the account IDs the panel
// has given it. Removing a login and adding it again makes a new account with a
// new ID; its email is the one thing that stays, and with it the weeks the
// gateway already metered.
func loginKey(a Account) string {
	if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
		return e
	}
	return "acct:" + a.ID
}

// weekShares is who spent an account's current week.
type weekShares struct {
	// People are those who ran it in the window and still have a card, largest
	// first, each with OfWeekly set.
	People []PersonShare
	// Unaccounted is the part of the plan's week, as a 0..1 fraction of the
	// plan, that no metered request explains.
	Unaccounted float64
	// Tokens is how many weighted tokens make the plan's whole week; 0 when it
	// is not yet known.
	Tokens float64
	Basis  string // ScaleHistory | ScaleThisWeek | ScaleNone
	// Since is when the gateway first metered this login — the earliest week it
	// can account for. Zero if it never has.
	Since time.Time
}

// weeklyShares works out who spent an account's current week, and how much of the
// whole plan each of them accounts for. Empty when there is no weekly reading to
// divide or no weekly reset to scope the tokens to.
func (s *Server) weeklyShares(ctx context.Context, d Data, accountID string, sevenD float64, sevenDReset time.Time) weekShares {
	out := weekShares{Basis: ScaleNone}
	if s.usage == nil || sevenD <= 0 || sevenDReset.IsZero() {
		return out
	}
	s.linkLogin(ctx, d, accountID)

	start := sevenDReset.Add(-weeklyWindow)
	rows, err := s.usage.AccountUsageByPerson(ctx, accountID, start)
	if err != nil {
		return out
	}
	// The total is every metered token in the window, including a person since
	// removed from the panel: their tokens are known, they just have no card to
	// hang them on, and leaving them out would read as "not metered".
	weighted := make([]float64, len(rows))
	var total float64
	for i, r := range rows {
		weighted[i] = r.Weighted
		total += r.Weighted
	}

	first, _ := s.usage.FirstMeteredAt(ctx, accountID)
	out.Since = first
	// Only a window the gateway watched from its first minute says anything
	// about the plan's size: otherwise some of its tokens are missing, and the
	// ratio would be wrong in exactly the way this exists to avoid.
	var cur *WeekSample
	if !first.IsZero() && first.Before(start) && total > 0 {
		cur = &WeekSample{Reset: weekKey(sevenDReset), Util: sevenD, Weighted: total}
		s.recordSample(ctx, accountID, *cur)
	}
	samples, _ := s.usage.WeekSamples(ctx, accountID, scaleWeeks+2)
	out.Tokens, out.Basis = weekScale(samples, cur, s.now())

	shares, unaccounted := splitWeek(sevenD, out.Tokens, weighted)
	out.Unaccounted = unaccounted
	for i, r := range rows {
		name, ok := s.lookupSubject(d, "person", r.SubjectID)
		if !ok || r.Weighted <= 0 {
			continue
		}
		out.People = append(out.People, PersonShare{
			ID: r.SubjectID, Name: name, Weighted: r.Weighted, ByModel: r.ByModel, OfWeekly: shares[i],
		})
	}
	sort.Slice(out.People, func(i, j int) bool { return out.People[i].Weighted > out.People[j].Weighted })
	return out
}

// linkLogin tells the metering which login an account ID is, once per process
// per account: the gateway keys usage by account ID, and this is what lets a
// re-added login's new ID inherit the weeks metered under the old one.
func (s *Server) linkLogin(ctx context.Context, d Data, accountID string) {
	acct, ok := d.Account(accountID)
	if !ok {
		return
	}
	key := loginKey(*acct)
	s.memo.Lock()
	done := s.memo.linked[accountID] == key
	s.memo.Unlock()
	if done {
		return
	}
	if err := s.usage.LinkLogin(ctx, accountID, key); err != nil {
		return // tried again on the next read
	}
	s.memo.Lock()
	s.memo.linked[accountID] = key
	s.memo.Unlock()
}

// recordSample files the running week's reading, but only when it moved: a
// board and every machine's check-in read this every few seconds, and a week
// whose figure has not risen has nothing new to teach.
func (s *Server) recordSample(ctx context.Context, accountID string, cur WeekSample) {
	s.memo.Lock()
	last, seen := s.memo.sampled[accountID]
	s.memo.Unlock()
	if seen && last.Reset.Equal(cur.Reset) && cur.Util <= last.Util {
		return
	}
	if err := s.usage.RecordWeekSample(ctx, accountID, cur); err != nil {
		return
	}
	s.memo.Lock()
	s.memo.sampled[accountID] = cur
	s.memo.Unlock()
}
