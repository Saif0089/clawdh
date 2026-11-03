package panelpg

import (
	"context"
	"database/sql"
	"time"
)

// account_alerts records the most recent time an account's shared login stopped
// working. That almost always means the same account is being used first-party
// (a raw login, outside the gateway) on some machine, which rotates its refresh
// token out from under the gateway — the collision that silently breaks sharing.
// The panel shows a warning on such accounts so it is seen, not discovered.
const alertsDDL = `
CREATE TABLE IF NOT EXISTS account_alerts (
    account_id     text PRIMARY KEY,
    last_collision timestamptz NOT NULL DEFAULT now(),
    note           text NOT NULL DEFAULT ''
);`

func ensureAlertsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, alertsDDL)
	return err
}

// RecordCollision notes that an account's shared login just failed to refresh.
// Best-effort; the gateway calls it off the request's error path.
func (b *Backend) RecordCollision(ctx context.Context, accountID, note string) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO account_alerts (account_id, last_collision, note)
		VALUES ($1, now(), $2)
		ON CONFLICT (account_id) DO UPDATE SET last_collision = now(), note = EXCLUDED.note`,
		accountID, note)
	return err
}

// RecentCollisions returns, per account id, the last time its shared login broke,
// for collisions since a cutoff. The panel uses it to flag accounts that are
// being used outside the gateway.
//
// A break counts only until the login reads fine again. The gateway reads every
// held login every few minutes, and a reading it got AFTER the break proves the
// login works now, whether it was signed in again, refreshed on the panel or
// simply recovered. Without this the card kept saying "stopped working in the
// last day" for up to a day after the person had fixed it, which reads as the fix
// having failed.
func (b *Backend) RecentCollisions(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT a.account_id, a.last_collision
		  FROM account_alerts a
		  LEFT JOIN account_windows w ON w.account_id = a.account_id
		 WHERE a.last_collision >= $1
		   AND (w.updated_at IS NULL OR w.updated_at < a.last_collision)`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}
