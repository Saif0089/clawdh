package panelpg

import (
	"context"
	"fmt"
)

// The metering tables, in the order a wipe reports and clears them. Ceilings
// (limits) are configuration, not readings, and stay; so does the panel's
// own state. account_windows holds only each login's latest reading and is
// re-read by the poller within minutes, so clearing it costs nothing and
// leaves no stale % on the board.
var meteringTables = []string{"usage_events", "usage_counters", "account_windows"}

// MeteringCounts is how many rows each metering table holds, keyed by table —
// what a wipe shows before it is confirmed, and after.
func (b *Backend) MeteringCounts(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(meteringTables))
	for _, t := range meteringTables {
		var n int64
		if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return nil, fmt.Errorf("counting %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// MeteringTables is the tables a wipe clears, in order.
func MeteringTables() []string { return append([]string(nil), meteringTables...) }

// WipeMetering clears every metering table in one transaction: everything the
// gateway has recorded of who used what, and every window reading. The
// boards start over from the next request; ceilings and shares are untouched.
func (b *Backend) WipeMetering(ctx context.Context) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	for _, t := range meteringTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("clearing %s: %w", t, err)
		}
	}
	return tx.Commit()
}
