package panelpg

import (
	"context"
	"database/sql"
	"time"

	"clawdh/panel"
)

// The gateway meters by account ID, and an account ID lasts only as long as the
// login stays in the panel: remove it and add it again — to re-add a login that
// stopped refreshing — and the same Claude login comes back under a new ID with
// no history. Anything computed per account then starts from zero, mid-week,
// and the week's earlier usage belongs to an ID that no longer has a card.
//
// account_lineage ties every account ID the panel has ever shown to the login it
// was, so a read "for an account" can mean the login. It is additive: usage_events
// still holds exactly what the gateway wrote, under the ID the gateway saw.
//
// week_samples is what each weekly window taught about its plan: Anthropic's
// utilisation at its fullest, and the weighted tokens metered in that window.
// Their ratio is the plan's size in the gateway's unit, learned from the plan
// itself rather than from a table of plans. It cannot be reconstructed later —
// the reading at the end of a past week is gone once the next one overwrites
// account_windows — so it is filed as it is seen.
const lineageDDL = `
CREATE TABLE IF NOT EXISTS account_lineage (
    account_id text PRIMARY KEY,
    login_key  text NOT NULL,
    linked_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS account_lineage_login ON account_lineage(login_key);

CREATE TABLE IF NOT EXISTS week_samples (
    login_key  text NOT NULL,
    reset      timestamptz NOT NULL,
    util       double precision NOT NULL,
    weighted   double precision NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (login_key, reset)
);`

func ensureLineageSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, lineageDDL)
	return err
}

// sameLogin is a WHERE fragment, with the account ID as $1, matching an
// account's own ID and every other ID the same login has had. An account never
// linked matches only itself.
const sameLogin = `(account_id = $1 OR account_id IN (
	SELECT l.account_id FROM account_lineage l
	 WHERE l.login_key = (SELECT login_key FROM account_lineage WHERE account_id = $1)))`

// loginOf is the login key an account's samples are filed under: its link, or —
// for an account never linked — itself.
const loginOf = `COALESCE((SELECT login_key FROM account_lineage WHERE account_id = $1), 'acct:' || $1)`

// LinkLogin records that an account ID is the login under a key. Idempotent; a
// login whose email changes moves to its new key.
func (b *Backend) LinkLogin(ctx context.Context, accountID, loginKey string) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO account_lineage (account_id, login_key) VALUES ($1, $2)
		ON CONFLICT (account_id) DO UPDATE SET login_key = EXCLUDED.login_key`,
		accountID, loginKey)
	return err
}

// FirstMeteredAt is when the gateway first metered this login, under any of the
// IDs it has had. Zero if it never has.
func (b *Backend) FirstMeteredAt(ctx context.Context, accountID string) (time.Time, error) {
	var at sql.NullTime
	err := b.db.QueryRowContext(ctx,
		`SELECT min(at) FROM usage_events WHERE `+sameLogin, accountID).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	return at.Time, nil
}

// WeekSamples are the readings filed for this login's weekly windows, newest
// window first.
func (b *Backend) WeekSamples(ctx context.Context, accountID string, limit int) ([]panel.WeekSample, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT reset, util, weighted FROM week_samples
		 WHERE login_key = `+loginOf+`
		 ORDER BY reset DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.WeekSample
	for rows.Next() {
		var s panel.WeekSample
		if err := rows.Scan(&s.Reset, &s.Util, &s.Weighted); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecordWeekSample files a window's reading, keeping the fullest one the window
// ever had: the size of a plan is read most exactly where the reading is highest,
// and a later, lower figure (a window that reset early, a stale header) must not
// overwrite it.
func (b *Backend) RecordWeekSample(ctx context.Context, accountID string, s panel.WeekSample) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO week_samples (login_key, reset, util, weighted)
		VALUES (`+loginOf+`, $2, $3, $4)
		ON CONFLICT (login_key, reset) DO UPDATE
		   SET util = EXCLUDED.util, weighted = EXCLUDED.weighted, updated_at = now()
		 WHERE EXCLUDED.util >= week_samples.util`,
		accountID, s.Reset.UTC(), s.Util, s.Weighted)
	return err
}
