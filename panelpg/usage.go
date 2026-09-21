package panelpg

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"clawdh/panel"
)

// The metering tables live alongside the panel_state blob. Unlike the small,
// bounded panel data (kept as one JSON row so the panel logic stays unchanged),
// usage is high-volume and must be queried by person, model and time window — so
// it is real relational tables. person_id/account_id are the blob's string IDs;
// there are no foreign keys because those rows live in the JSON blob, and names
// are resolved from it when a board is drawn.
const usageDDL = `
CREATE TABLE IF NOT EXISTS usage_events (
    id                    bigserial PRIMARY KEY,
    at                    timestamptz NOT NULL DEFAULT now(),
    person_id             text NOT NULL DEFAULT '',
    account_id            text NOT NULL DEFAULT '',
    model                 text NOT NULL,
    input_tokens          bigint NOT NULL DEFAULT 0,
    output_tokens         bigint NOT NULL DEFAULT 0,
    cache_creation_tokens bigint NOT NULL DEFAULT 0,
    cache_read_tokens     bigint NOT NULL DEFAULT 0,
    weighted_tokens       double precision NOT NULL DEFAULT 0,
    cost_usd              double precision NOT NULL DEFAULT 0,
    request_id            text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS usage_events_at         ON usage_events(at);
CREATE INDEX IF NOT EXISTS usage_events_person_at  ON usage_events(person_id, at);
CREATE INDEX IF NOT EXISTS usage_events_account_at ON usage_events(account_id, at);

CREATE TABLE IF NOT EXISTS usage_counters (
    subject_type text NOT NULL,          -- 'person' | 'account'
    subject_id   text NOT NULL,
    window_start timestamptz NOT NULL,   -- truncated to the hour
    model        text NOT NULL,
    weighted_tokens       double precision NOT NULL DEFAULT 0,
    input_tokens          bigint NOT NULL DEFAULT 0,
    output_tokens         bigint NOT NULL DEFAULT 0,
    cache_creation_tokens bigint NOT NULL DEFAULT 0,
    cache_read_tokens     bigint NOT NULL DEFAULT 0,
    cost_usd              double precision NOT NULL DEFAULT 0,
    PRIMARY KEY (subject_type, subject_id, window_start, model)
);

CREATE TABLE IF NOT EXISTS limits (
    id           text PRIMARY KEY,
    subject_type text NOT NULL,             -- 'person' | 'account'
    subject_id   text NOT NULL,
    max_percent  double precision NOT NULL, -- ceiling on the account's weekly window (0..1)
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- Earlier builds also capped metered tokens and a notional dollar figure per
-- calendar window. Those kinds are gone: their rows, then their columns.
ALTER TABLE limits ADD COLUMN IF NOT EXISTS max_percent double precision;
DELETE FROM limits WHERE max_percent IS NULL OR max_percent <= 0;
ALTER TABLE limits DROP COLUMN IF EXISTS max_weighted_tokens;
ALTER TABLE limits DROP COLUMN IF EXISTS max_cost_usd;
ALTER TABLE limits DROP COLUMN IF EXISTS window_kind;
ALTER TABLE limits ALTER COLUMN max_percent SET NOT NULL;`

func ensureUsageSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, usageDDL)
	return err
}

// UsageEvent is one forwarded response's metered usage, already priced (weighted
// tokens + USD) by the caller.
type UsageEvent struct {
	At            time.Time
	PersonID      string
	AccountID     string
	Model         string
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	Weighted      float64
	CostUSD       float64
	RequestID     string
}

// RecordUsage stores one event and rolls it into the hourly counters for both
// the person and the account, in one transaction. It is best-effort from the
// gateway's point of view — the caller never lets a metering failure affect a
// forwarded request — but here it is transactional so a counter never drifts
// from the events it summarizes.
func (b *Backend) RecordUsage(ctx context.Context, ev UsageEvent) error {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	hour := ev.At.UTC().Truncate(time.Hour)

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events
		  (at, person_id, account_id, model, input_tokens, output_tokens,
		   cache_creation_tokens, cache_read_tokens, weighted_tokens, cost_usd, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		ev.At, ev.PersonID, ev.AccountID, ev.Model, ev.Input, ev.Output,
		ev.CacheCreation, ev.CacheRead, ev.Weighted, ev.CostUSD, ev.RequestID); err != nil {
		return err
	}

	for _, s := range []struct{ kind, id string }{
		{"person", ev.PersonID},
		{"account", ev.AccountID},
	} {
		if s.id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO usage_counters
			  (subject_type, subject_id, window_start, model, weighted_tokens,
			   input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cost_usd)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (subject_type, subject_id, window_start, model) DO UPDATE SET
			  weighted_tokens       = usage_counters.weighted_tokens       + EXCLUDED.weighted_tokens,
			  input_tokens          = usage_counters.input_tokens          + EXCLUDED.input_tokens,
			  output_tokens         = usage_counters.output_tokens         + EXCLUDED.output_tokens,
			  cache_creation_tokens = usage_counters.cache_creation_tokens + EXCLUDED.cache_creation_tokens,
			  cache_read_tokens     = usage_counters.cache_read_tokens     + EXCLUDED.cache_read_tokens,
			  cost_usd              = usage_counters.cost_usd              + EXCLUDED.cost_usd`,
			s.kind, s.id, hour, ev.Model, ev.Weighted,
			ev.Input, ev.Output, ev.CacheCreation, ev.CacheRead, ev.CostUSD); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- board reads
//
// The board types (SubjectUsage/ModelUsage) live in the panel package so the
// panel can serve them without importing this Postgres layer; panelpg returns
// them. That keeps the client binary, which imports panel, free of pgx. The
// cost_usd column is recorded (what the tokens would have cost at API prices)
// but never read for a board: a subscription has no per-token price, and a
// dollar figure next to a window's real % was one number too many.

// UsageBySubject returns every subject of a kind ('person' | 'account') with its
// per-model usage since a time, sorted by weighted tokens descending. Names are
// resolved by the caller from the panel blob (these are IDs).
func (b *Backend) UsageBySubject(ctx context.Context, subjectType string, since time.Time) ([]panel.SubjectUsage, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT subject_id, model,
		       SUM(weighted_tokens), SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_creation_tokens), SUM(cache_read_tokens)
		  FROM usage_counters
		 WHERE subject_type = $1 AND window_start >= $2
		 GROUP BY subject_id, model
		 ORDER BY subject_id`, subjectType, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySubject := map[string]*panel.SubjectUsage{}
	for rows.Next() {
		var sid string
		var mu panel.ModelUsage
		if err := rows.Scan(&sid, &mu.Model, &mu.Weighted, &mu.Input, &mu.Output,
			&mu.CacheCreation, &mu.CacheRead); err != nil {
			return nil, err
		}
		s := bySubject[sid]
		if s == nil {
			s = &panel.SubjectUsage{SubjectID: sid}
			bySubject[sid] = s
		}
		s.ByModel = append(s.ByModel, mu)
		s.Weighted += mu.Weighted
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]panel.SubjectUsage, 0, len(bySubject))
	for _, s := range bySubject {
		sort.Slice(s.ByModel, func(i, j int) bool { return s.ByModel[i].Weighted > s.ByModel[j].Weighted })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weighted > out[j].Weighted })
	return out, nil
}

// AccountUsageByPerson breaks one account's usage down by person and model —
// who ran it. It reads the raw events (the hourly counters are per-subject, so
// they don't hold the person×account cross).
func (b *Backend) AccountUsageByPerson(ctx context.Context, accountID string, since time.Time) ([]panel.SubjectUsage, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT person_id, model,
		       SUM(weighted_tokens), SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_creation_tokens), SUM(cache_read_tokens)
		  FROM usage_events
		 WHERE account_id = $1 AND at >= $2
		 GROUP BY person_id, model`, accountID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySubject := map[string]*panel.SubjectUsage{}
	for rows.Next() {
		var sid string
		var mu panel.ModelUsage
		if err := rows.Scan(&sid, &mu.Model, &mu.Weighted, &mu.Input, &mu.Output,
			&mu.CacheCreation, &mu.CacheRead); err != nil {
			return nil, err
		}
		s := bySubject[sid]
		if s == nil {
			s = &panel.SubjectUsage{SubjectID: sid}
			bySubject[sid] = s
		}
		s.ByModel = append(s.ByModel, mu)
		s.Weighted += mu.Weighted
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]panel.SubjectUsage, 0, len(bySubject))
	for _, s := range bySubject {
		sort.Slice(s.ByModel, func(i, j int) bool { return s.ByModel[i].Weighted > s.ByModel[j].Weighted })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weighted > out[j].Weighted })
	return out, nil
}

// LatestEventAt is when the most recent usage event landed — the "as of" a board
// carries so a viewer always knows how fresh the numbers are, and nothing goes
// silently stale. A never-metered panel returns the zero time.
func (b *Backend) LatestEventAt(ctx context.Context) (time.Time, error) {
	var at sql.NullTime
	err := b.db.QueryRowContext(ctx, `SELECT max(at) FROM usage_events`).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	return at.Time, nil
}
