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

// Quotas: window ceilings. A limit is a percentage of an account's weekly
// window — Anthropic's own number, read straight from the login, never an
// estimate of who caused it — past which a person, or everyone on an account,
// is turned away. The gateway checks the tightest applicable ceiling before
// serving; over it, the member gets a definitive 429 that says when the window
// itself rolls over, which is what actually frees it.

// The Limit type lives in the panel package (so the panel serves it without
// importing this Postgres layer). LimitStatus is the enforcement result the
// gateway consumes and stays here.

// LimitStatus is a subject's standing against the tightest ceiling that
// applies: how much of it is used (0..1+), when the window resets, and a
// message for the 429 or a 75%/95% warning. Over is true once any applicable
// ceiling is reached.
type LimitStatus struct {
	Over     bool      `json:"over"`
	Fraction float64   `json:"fraction"`
	ResetAt  time.Time `json:"resetAt"`
	Message  string    `json:"message"`
}

// MemberLimitStatus checks one request — a person using a specific account —
// against every ceiling that could stop it: the person's own, and the one on
// the account they are using. It returns the tightest (fullest). A request
// with no applicable ceiling, or one on an account with no reading yet, comes
// back Over=false, Fraction=0 — it fails open, never blocking on missing data.
func (b *Backend) MemberLimitStatus(ctx context.Context, personID, accountID string) (LimitStatus, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT subject_type, max_percent
		  FROM limits
		 WHERE (subject_type = 'person'  AND subject_id = $1)
		    OR (subject_type = 'account' AND subject_id = $2)`, personID, accountID)
	if err != nil {
		return LimitStatus{}, err
	}
	defer rows.Close()
	type lim struct {
		subjectType string
		maxP        float64
	}
	var limits []lim
	for rows.Next() {
		var l lim
		if err := rows.Scan(&l.subjectType, &l.maxP); err != nil {
			return LimitStatus{}, err
		}
		if l.maxP > 0 {
			limits = append(limits, l)
		}
	}
	if err := rows.Err(); err != nil {
		return LimitStatus{}, err
	}
	if len(limits) == 0 {
		return LimitStatus{}, nil
	}

	util, reset, err := b.accountWeeklyWindow(ctx, accountID)
	if err != nil {
		return LimitStatus{}, err
	}
	var tightest LimitStatus
	for _, l := range limits {
		frac := util / l.maxP
		if frac <= tightest.Fraction {
			continue
		}
		tightest = LimitStatus{
			Over: frac >= 1.0, Fraction: frac, ResetAt: reset,
			Message: fmt.Sprintf("this account's weekly window is %.0f%% full, past the %.0f%% ceiling set for %s on clawdh; the window resets %s UTC.",
				util*100, l.maxP*100, subjectWord(l.subjectType), reset.UTC().Format("2006-01-02 15:04")),
		}
	}
	return tightest, nil
}

// accountWeeklyWindow is how full an account's weekly usage window is (0..1)
// and when it rolls over — Anthropic's own numbers, captured off the
// rate-limit headers or read by the usage poller. Zero (fail-open) until a
// reading exists for the account.
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

// SetLimit sets the ceiling for a subject, replacing any prior one for that
// exact (subject_type, subject_id) so a subject has one ceiling, not a pile.
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
		`DELETE FROM limits WHERE subject_type=$1 AND subject_id=$2`, l.SubjectType, l.SubjectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO limits (id, subject_type, subject_id, max_percent) VALUES ($1,$2,$3,$4)`,
		l.ID, l.SubjectType, l.SubjectID, l.MaxPercent); err != nil {
		return err
	}
	return tx.Commit()
}

// ListLimits returns every configured ceiling.
func (b *Backend) ListLimits(ctx context.Context) ([]panel.Limit, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, subject_type, subject_id, max_percent
		  FROM limits ORDER BY subject_type, subject_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.Limit
	for rows.Next() {
		var l panel.Limit
		if err := rows.Scan(&l.ID, &l.SubjectType, &l.SubjectID, &l.MaxPercent); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLimit removes a ceiling by id.
func (b *Backend) DeleteLimit(ctx context.Context, id string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM limits WHERE id = $1`, id)
	return err
}

func subjectWord(subjectType string) string {
	if subjectType == "account" {
		return "everyone using it"
	}
	return "you"
}
