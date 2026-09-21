package panelpg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"clawdh/panel"
)

// newHexID mints a short random id, matching the shape of the panel's own ids.
func newHexID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Quotas. A limit caps a person, one account, or the whole org to a number of
// weighted tokens and/or a USD amount (or a % of the weekly window) within a
// calendar window (day/week/month, UTC boundaries — the reset shape Anthropic's
// own spend limits use). The gateway checks the tightest applicable limit before
// serving; over it, the member gets a definitive 429 with the window's reset
// time, exactly like a real spend cap.

// The Limit type lives in the panel package (so the panel serves it without
// importing this Postgres layer). LimitStatus is the enforcement result the
// gateway consumes and stays here.

// LimitStatus is a subject's standing against the tightest quota that applies:
// how much of it is used (0..1+), when it resets, and a message for the 429 or a
// 75%/95% warning. Over is true once any applicable limit is reached.
type LimitStatus struct {
	Over     bool      `json:"over"`
	Fraction float64   `json:"fraction"`
	ResetAt  time.Time `json:"resetAt"`
	Message  string    `json:"message"`
}

// windowBounds returns the calendar start and reset of a quota window in UTC:
// day = midnight, week = Monday, month = the 1st.
func windowBounds(now time.Time, kind string) (start, reset time.Time) {
	n := now.UTC()
	switch kind {
	case "week":
		off := (int(n.Weekday()) + 6) % 7 // days since Monday
		start = time.Date(n.Year(), n.Month(), n.Day()-off, 0, 0, 0, 0, time.UTC)
		reset = start.AddDate(0, 0, 7)
	case "month":
		start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
		reset = start.AddDate(0, 1, 0)
	default: // day
		start = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
		reset = start.AddDate(0, 0, 1)
	}
	return
}

// MemberLimitStatus checks one request — a person using a specific account —
// against every quota that could stop it: the person's own limits, the limits on
// the account they are using, and any org-wide limit. It returns the tightest
// (most-used). A request with no applicable limit comes back Over=false,
// Fraction=0. now is a parameter for tests.
func (b *Backend) MemberLimitStatus(ctx context.Context, personID, accountID string, now time.Time) (LimitStatus, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT subject_type, window_kind, max_weighted_tokens, max_cost_usd, max_percent
		  FROM limits
		 WHERE (subject_type = 'person'  AND subject_id = $1)
		    OR (subject_type = 'account' AND subject_id = $2)
		    OR  subject_type = 'org'`, personID, accountID)
	if err != nil {
		return LimitStatus{}, err
	}
	defer rows.Close()

	type lim struct {
		subjectType, windowKind string
		maxW, maxC, maxP        sql.NullFloat64
	}
	var limits []lim
	for rows.Next() {
		var l lim
		if err := rows.Scan(&l.subjectType, &l.windowKind, &l.maxW, &l.maxC, &l.maxP); err != nil {
			return LimitStatus{}, err
		}
		limits = append(limits, l)
	}
	if err := rows.Err(); err != nil {
		return LimitStatus{}, err
	}

	// The account's weekly utilisation is only read if a % limit needs it.
	accountUtil, utilDone := 0.0, false
	var windowReset time.Time

	var tightest LimitStatus
	for _, l := range limits {
		start, reset := windowBounds(now, l.windowKind)
		// Which subject's usage this limit measures: the account for an account
		// limit, the person otherwise (org sums everyone, ignoring the id).
		subjectID := personID
		if l.subjectType == "account" {
			subjectID = accountID
		}
		usedW, usedC, err := b.usedSince(ctx, l.subjectType, subjectID, start)
		if err != nil {
			return LimitStatus{}, err
		}
		frac := 0.0
		if l.maxW.Valid && l.maxW.Float64 > 0 {
			frac = maxf(frac, usedW/l.maxW.Float64)
		}
		if l.maxC.Valid && l.maxC.Float64 > 0 {
			frac = maxf(frac, usedC/l.maxC.Float64)
		}
		// A "% of weekly" cap is a ceiling on the account's own weekly window —
		// Anthropic's number, read straight from the login — never an estimate
		// of who caused it. For an account it applies to everyone using it; for
		// a person it applies to whichever account they are using right now
		// ("Ibrahim can use it while its week is under 25% full"). Fails open
		// (0) until a reading exists, so it never blocks on missing data.
		if l.maxP.Valid && l.maxP.Float64 > 0 && (l.subjectType == "person" || l.subjectType == "account") {
			if !utilDone {
				accountUtil, windowReset, err = b.accountWeeklyWindow(ctx, accountID)
				if err != nil {
					return LimitStatus{}, err
				}
				utilDone = true
			}
			frac = maxf(frac, accountUtil/l.maxP.Float64)
			if !windowReset.IsZero() && accountUtil/l.maxP.Float64 >= frac {
				reset = windowReset
			}
		}
		if frac > tightest.Fraction {
			who := "your"
			switch l.subjectType {
			case "org":
				who = "the team's"
			case "account":
				who = "this account's"
			}
			msg := fmt.Sprintf("%s clawdh %s quota is reached; it resets %s UTC.",
				who, l.windowKind, reset.Format("2006-01-02 15:04"))
			// A ceiling is about the account's window, so say that — and when
			// the window itself rolls over, which is what actually frees it.
			if l.maxP.Valid && l.maxP.Float64 > 0 && accountUtil/l.maxP.Float64 >= frac {
				msg = fmt.Sprintf("this account's weekly window is %.0f%% full, past the %.0f%% ceiling set for %s on clawdh; the window resets %s UTC.",
					accountUtil*100, l.maxP.Float64*100, subjectWord(l.subjectType), reset.Format("2006-01-02 15:04"))
			}
			tightest = LimitStatus{Over: frac >= 1.0, Fraction: frac, ResetAt: reset, Message: msg}
		}
	}
	return tightest, nil
}

// LimitUsage reports how much of one configured limit is used right now (0..1+,
// the max of its weighted-token and USD utilisation) and when its window resets.
// It reuses the same window math and counters the gateway enforces against, so
// the board and the enforcement never disagree.
func (b *Backend) LimitUsage(ctx context.Context, l panel.Limit) (float64, time.Time, error) {
	start, reset := windowBounds(time.Now(), l.WindowKind)
	usedW, usedC, err := b.usedSince(ctx, l.SubjectType, l.SubjectID, start)
	if err != nil {
		return 0, reset, err
	}
	frac := 0.0
	if l.MaxWeighted != nil && *l.MaxWeighted > 0 {
		frac = maxf(frac, usedW/(*l.MaxWeighted))
	}
	if l.MaxCostUSD != nil && *l.MaxCostUSD > 0 {
		frac = maxf(frac, usedC/(*l.MaxCostUSD))
	}
	// An account's % cap is measured against its own weekly window. A person's
	// is a ceiling on whichever account they use, so without an account in hand
	// it is settled by the caller, which knows which accounts the person can use
	// (see panel.handleListLimits).
	if l.MaxPercent != nil && *l.MaxPercent > 0 && l.SubjectType == "account" {
		if util, err := b.accountWeeklyUtil(ctx, l.SubjectID); err == nil {
			frac = maxf(frac, util/(*l.MaxPercent))
		}
	}
	return frac, reset, nil
}

// usedSince sums a subject's weighted tokens and USD in a window: one person's
// or one account's own counters, or — for an org limit — every person's (which
// is all usage). Counter rows exist for both persons and accounts because
// RecordUsage rolls every event into both.
func (b *Backend) usedSince(ctx context.Context, subjectType, subjectID string, start time.Time) (weighted, cost float64, err error) {
	counterType := "person"
	if subjectType == "account" {
		counterType = "account"
	}
	q := `SELECT COALESCE(SUM(weighted_tokens),0), COALESCE(SUM(cost_usd),0)
	        FROM usage_counters WHERE subject_type = $1 AND window_start >= $2`
	args := []any{counterType, start}
	if subjectType != "org" {
		q += ` AND subject_id = $3`
		args = append(args, subjectID)
	}
	err = b.db.QueryRowContext(ctx, q, args...).Scan(&weighted, &cost)
	return
}

// accountWeeklyUtil is how full an account's weekly usage window is (0..1) —
// Anthropic's own number, captured off the rate-limit headers or read by the
// usage poller — the number a window ceiling is checked against. Zero
// (fail-open) until a reading exists for the account.
func (b *Backend) accountWeeklyUtil(ctx context.Context, accountID string) (float64, error) {
	u, _, err := b.accountWeeklyWindow(ctx, accountID)
	return u, err
}

// accountWeeklyWindow is accountWeeklyUtil with the window's own reset time —
// when Anthropic's rolling week actually frees up, which is what a ceiling
// message should promise, not the calendar week.
func (b *Backend) accountWeeklyWindow(ctx context.Context, accountID string) (float64, time.Time, error) {
	var u sql.NullFloat64
	var reset sql.NullTime
	err := b.db.QueryRowContext(ctx,
		`SELECT sevend_util, sevend_reset FROM account_windows WHERE account_id = $1`, accountID).Scan(&u, &reset)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	return u.Float64, reset.Time, nil
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// SetLimit sets the cap for a subject+window, replacing any prior one for that
// exact (subject_type, subject_id, window_kind) so a subject has one limit per
// window rather than a pile of them.
func (b *Backend) SetLimit(ctx context.Context, l panel.Limit) error {
	if l.ID == "" {
		l.ID = newHexID()
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM limits WHERE subject_type=$1 AND subject_id=$2 AND window_kind=$3`,
		l.SubjectType, l.SubjectID, l.WindowKind); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO limits (id, subject_type, subject_id, window_kind, max_weighted_tokens, max_cost_usd, max_percent)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		l.ID, l.SubjectType, l.SubjectID, l.WindowKind, nullF(l.MaxWeighted), nullF(l.MaxCostUSD), nullF(l.MaxPercent)); err != nil {
		return err
	}
	return tx.Commit()
}

// ListLimits returns every configured limit.
func (b *Backend) ListLimits(ctx context.Context) ([]panel.Limit, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, subject_type, subject_id, window_kind, max_weighted_tokens, max_cost_usd, max_percent
		  FROM limits ORDER BY subject_type, subject_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.Limit
	for rows.Next() {
		var l panel.Limit
		var w, c, p sql.NullFloat64
		if err := rows.Scan(&l.ID, &l.SubjectType, &l.SubjectID, &l.WindowKind, &w, &c, &p); err != nil {
			return nil, err
		}
		if w.Valid {
			l.MaxWeighted = &w.Float64
		}
		if c.Valid {
			l.MaxCostUSD = &c.Float64
		}
		if p.Valid {
			l.MaxPercent = &p.Float64
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLimit removes a limit by id.
func (b *Backend) DeleteLimit(ctx context.Context, id string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM limits WHERE id = $1`, id)
	return err
}

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func subjectWord(subjectType string) string {
	if subjectType == "account" {
		return "everyone using it"
	}
	return "you"
}
