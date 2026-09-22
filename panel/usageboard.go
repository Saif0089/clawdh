package panel

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// The usage board. Its unit is the account: for each shared login, the rolling
// 5-hour and weekly windows — Anthropic's own figures for how full they are,
// read by the gateway off the login — and, for a rolling day, week, or month,
// who ran it and on which models, from the gateway's own metering. A short
// across-accounts list of people follows, for the one question a per-account
// view cannot answer.
//
// Everything is attributed per account and per person by the gateway, so a
// request from an editor and one from a terminal land on the same rows. Every
// board carries an `asOf` (the time of the most recent metered event) so a
// number is never mistaken for live while it is stale.

// ModelUsage is one model's totals within a window.
type ModelUsage struct {
	Model         string  `json:"model"`
	Weighted      float64 `json:"weighted"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheCreation int64   `json:"cacheCreation"`
	CacheRead     int64   `json:"cacheRead"`
}

// SubjectUsage is one person's or account's usage in a window, broken down by
// model — "40% was this person; how much of that was Opus?".
type SubjectUsage struct {
	SubjectID string       `json:"subjectId"`
	Weighted  float64      `json:"weighted"`
	ByModel   []ModelUsage `json:"byModel"`
}

// PersonShare is one person's part of an account's usage in a period: how much
// of it they ran, and on which models.
type PersonShare struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Weighted float64      `json:"weighted"`
	ByModel  []ModelUsage `json:"byModel"`
	// OfWeekly is how much of the account's real weekly allowance this person
	// accounts for, as a fraction of the whole plan — not of what was metered.
	//
	// The difference is the whole point. A person's token count divided by the
	// account's token count says "you were 100% of who used it", which on a
	// card beside a window meter reads as "you have used up the plan". One
	// person working alone is always 100% of that, however little they ran.
	// This is Anthropic's own weekly utilisation multiplied by that person's
	// slice of the tokens metered inside the same window, so it answers the
	// question people actually ask — how much of our week did you spend —
	// and every percentage on the card means one thing.
	//
	// Zero when the gateway has no window reading yet, when the weekly reset
	// time is unknown (there is then no window to scope the tokens to), or
	// when nothing was metered in the window.
	OfWeekly float64 `json:"ofWeekly"`
}

// AccountWindow is a subscription's real utilisation of its rolling usage
// windows, read from Anthropic's own rate-limit headers and usage endpoint by
// the gateway — the exact "% of the 5h / weekly window" that /usage shows, so
// the board mirrors it rather than estimating it. Fractions are 0..1.
type AccountWindow struct {
	AccountID   string    `json:"accountId"`
	Name        string    `json:"name"`
	FiveH       float64   `json:"fiveH"`
	SevenD      float64   `json:"sevenD"`
	FiveHReset  time.Time `json:"fiveHReset,omitempty"`
	SevenDReset time.Time `json:"sevenDReset,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// Models are the per-model weekly allowances Claude meters separately from
	// the all-models week — the one that usually runs out first (see
	// ModelWindow).
	Models []ModelWindow `json:"models,omitempty"`
	// HasReading is false for an account the gateway has not read yet: the
	// board lists it with "no reading yet" instead of a 0% that looks like idle.
	HasReading bool `json:"hasReading"`
	// People is who ran this account in the board's period, largest first, each
	// with their model split; Weighted and ByModel are the account's totals over
	// the same period. All empty when nobody ran it through the gateway then.
	People   []PersonShare `json:"people"`
	Weighted float64       `json:"weighted"`
	ByModel  []ModelUsage  `json:"byModel"`
}

// UsageReader is the metering read surface a board is drawn from. The Postgres
// backend implements it; a file-backed local panel has none, and the board
// routes are simply not mounted (nil).
type UsageReader interface {
	// UsageBySubject is every subject of a kind ('person' | 'account') with its
	// per-model usage since a time, largest first.
	UsageBySubject(ctx context.Context, subjectType string, since time.Time) ([]SubjectUsage, error)
	// AccountUsageByPerson breaks one account's usage down by person — who ran
	// it since a time.
	AccountUsageByPerson(ctx context.Context, accountID string, since time.Time) ([]SubjectUsage, error)
	LatestEventAt(ctx context.Context) (time.Time, error)
	// Quotas: window ceilings.
	ListLimits(ctx context.Context) ([]Limit, error)
	SetLimit(ctx context.Context, l Limit) error
	DeleteLimit(ctx context.Context, id string) error
	// Health: accounts whose shared login recently broke (used outside the gateway).
	RecentCollisions(ctx context.Context, since time.Time) (map[string]time.Time, error)
	// AccountWindows is each account's latest real 5h / weekly utilisation, from
	// Anthropic's own numbers — what the board's bars and every ceiling read.
	AccountWindows(ctx context.Context) ([]AccountWindow, error)
}

// windowSince maps a window name to a rolling start: the last 24 hours, 7
// days, or 30 days — never a calendar day, which would make "today" mean a
// different span every hour of the day. The board labels them as such.
func windowSince(now time.Time, name string) time.Time {
	switch name {
	case "week":
		return now.Add(-7 * 24 * time.Hour)
	case "month":
		return now.Add(-30 * 24 * time.Hour)
	default: // "day"
		return now.Add(-24 * time.Hour)
	}
}

// handlePeopleUsage serves the per-person board for a window: everyone who
// ran anything through the gateway in it, ranked, with their model split.
func (s *Server) handlePeopleUsage(w http.ResponseWriter, r *http.Request) {
	windowName := normalizeWindow(r.URL.Query().Get("window"))
	since := windowSince(s.now(), windowName)
	ctx := r.Context()
	rows, err := s.usage.UsageBySubject(ctx, "person", since)
	if err != nil {
		fail(w, http.StatusInternalServerError, "The usage board could not be read: "+err.Error())
		return
	}
	d, _ := s.store.Load() // names for the IDs; a read failure just leaves them blank

	subjects := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name, ok := s.lookupSubject(d, "person", row.SubjectID)
		if !ok {
			continue // a removed person — no ghost card
		}
		subjects = append(subjects, map[string]any{
			"id":       row.SubjectID,
			"name":     name,
			"weighted": row.Weighted,
			"byModel":  row.ByModel,
		})
	}
	asOf, _ := s.usage.LatestEventAt(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"window":   windowName,
		"since":    since.UTC(),
		"asOf":     asOf.UTC(),
		"subjects": subjects,
	})
}

// handleAccountsUsage serves the board's one view: every account that has a
// login, with its real 5h / weekly utilisation — the same windows /usage
// shows — and, for the asked period, who ran it and on which models. An
// account the gateway has not read yet is listed all the same, flagged, so the
// board never mistakes "not read" for "unused".
func (s *Server) handleAccountsUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	windowName := normalizeWindow(r.URL.Query().Get("window"))
	since := windowSince(s.now(), windowName)
	rows, err := s.usage.AccountWindows(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, "The window utilisation could not be read: "+err.Error())
		return
	}
	byAccount := make(map[string]AccountWindow, len(rows))
	for _, row := range rows {
		byAccount[row.AccountID] = row
	}
	d, _ := s.store.Load()
	out := make([]AccountWindow, 0, len(d.Accounts))
	for _, a := range d.Accounts {
		if !a.HasLogin() {
			continue // nothing runs through the gateway on it, so nothing to show
		}
		row, ok := byAccount[a.ID]
		row.AccountID, row.Name, row.HasReading = a.ID, a.Name, ok
		row.People = s.ranBy(ctx, d, a.ID, since)
		row.Weighted, row.ByModel = accountTotals(row.People)
		s.attributeWeekly(ctx, d, &row)
		out = append(out, row)
	}
	asOf, _ := s.usage.LatestEventAt(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"window": windowName, "since": since.UTC(), "asOf": asOf.UTC(), "accounts": out,
	})
}

// ranBy is who ran an account since a time, largest share first, by name, with
// each person's model split. People since removed from the panel are left
// out: their share is history with no card to hang it on. Best-effort: a read
// that fails names nobody.
func (s *Server) ranBy(ctx context.Context, d Data, accountID string, since time.Time) []PersonShare {
	rows, err := s.usage.AccountUsageByPerson(ctx, accountID, since)
	if err != nil {
		return []PersonShare{}
	}
	out := make([]PersonShare, 0, len(rows))
	for _, row := range rows {
		name, ok := s.lookupSubject(d, "person", row.SubjectID)
		if !ok || row.Weighted <= 0 {
			continue
		}
		out = append(out, PersonShare{ID: row.SubjectID, Name: name, Weighted: row.Weighted, ByModel: row.ByModel})
	}
	return out
}

// weeklyWindow is 7 days back from the reset Anthropic reports, which is the
// span its weekly utilisation figure covers.
const weeklyWindow = 7 * 24 * time.Hour

// attributeWeekly works out how much of the account's real weekly allowance
// each person accounts for (see PersonShare.OfWeekly).
//
// The tokens have to be counted over the same span the percentage covers, or
// the two do not divide: the board's own period is the reader's choice — a
// day, a week, a month — while the weekly utilisation is a fixed window ending
// at Anthropic's reset. So this asks a second time, scoped to that window, and
// splits the utilisation across the people in it.
func (s *Server) attributeWeekly(ctx context.Context, d Data, row *AccountWindow) {
	if !row.HasReading || len(row.People) == 0 {
		return
	}
	share := map[string]float64{}
	for _, p := range s.weeklyShares(ctx, d, row.AccountID, row.SevenD, row.SevenDReset) {
		share[p.ID] = p.OfWeekly
	}
	for i := range row.People {
		row.People[i].OfWeekly = share[row.People[i].ID]
	}
}

// weeklyShares is who spent an account's current week, and how much of the
// whole plan each of them accounts for. Empty when there is no weekly reading
// to divide, or nothing was metered inside the window.
func (s *Server) weeklyShares(ctx context.Context, d Data, accountID string, sevenD float64, sevenDReset time.Time) []PersonShare {
	if s.usage == nil || sevenD <= 0 || sevenDReset.IsZero() {
		return nil
	}
	inWindow := s.ranBy(ctx, d, accountID, sevenDReset.Add(-weeklyWindow))
	var total float64
	for _, p := range inWindow {
		total += p.Weighted
	}
	if total <= 0 {
		return nil
	}
	for i := range inWindow {
		inWindow[i].OfWeekly = sevenD * (inWindow[i].Weighted / total)
	}
	return inWindow
}

// accountTotals sums the people who ran an account into the account's own
// total and model split, largest model first.
func accountTotals(people []PersonShare) (float64, []ModelUsage) {
	var total float64
	byModel := map[string]*ModelUsage{}
	for _, p := range people {
		total += p.Weighted
		for _, m := range p.ByModel {
			t := byModel[m.Model]
			if t == nil {
				t = &ModelUsage{Model: m.Model}
				byModel[m.Model] = t
			}
			t.Weighted += m.Weighted
			t.Input += m.Input
			t.Output += m.Output
			t.CacheCreation += m.CacheCreation
			t.CacheRead += m.CacheRead
		}
	}
	out := make([]ModelUsage, 0, len(byModel))
	for _, m := range byModel {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weighted > out[j].Weighted })
	return total, out
}

func normalizeWindow(name string) string {
	switch name {
	case "week", "month":
		return name
	default:
		return "day"
	}
}

func (s *Server) subjectName(d Data, subjectType, id string) string {
	if name, ok := s.lookupSubject(d, subjectType, id); ok {
		return name
	}
	if subjectType == "account" {
		return "removed account"
	}
	return "removed member"
}

// lookupSubject resolves an id to a current person or account name and reports
// whether it still exists — so a board can drop the rows of a subject that was
// removed instead of showing a ghost "removed account/member" card for data the
// gateway captured before it went away.
func (s *Server) lookupSubject(d Data, subjectType, id string) (string, bool) {
	if subjectType == "account" {
		for _, a := range d.Accounts {
			if a.ID == id {
				return a.Name, true
			}
		}
		return "", false
	}
	for _, p := range d.People {
		if p.ID == id {
			return p.Name, true
		}
	}
	return "", false
}
