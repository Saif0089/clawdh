// Package panelpg keeps a clawdh panel's state in Postgres, for a panel that runs
// as more than one process at once — a serverless deployment, where each
// request may be a fresh instance with no shared disk.
//
// It is a separate package on purpose. The Postgres driver is a dependency the
// panel server needs and the clawdh client that ships to every machine does not,
// so only the server's entrypoint imports this; the client binary stays on its
// small, cgo-free dependency set.
//
// The whole panel is one row: a JSON blob and a version. That keeps every line
// of the panel's own logic — the model, the one-holder rule, the activity log —
// exactly as it is on a single machine, and turns "several instances must not
// both win a write" into one compare-and-swap the database settles.
package panelpg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Backend implements the panel's storage over a single Postgres row.
type Backend struct {
	db *sql.DB
}

// Open connects to Postgres and ensures the one table and one row exist. dsn is
// an ordinary Postgres URL — what a hosted Postgres hands you as DATABASE_URL.
func Open(ctx context.Context, dsn string) (*Backend, error) {
	// Parse the DSN and force the simple query protocol. A serverless panel is
	// handed a pooled (pgbouncer, transaction-mode) connection, under which
	// pgx's default implicit prepared statements collide across pooled
	// backends; the simple protocol sends no prepared statements at all. The
	// panel's queries are a handful of trivial ones, so nothing is lost.
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("the panel database URL could not be parsed: %w", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db := stdlib.OpenDB(*cfg)
	// A serverless instance handles one request and may vanish; a pile of idle
	// connections left behind would exhaust a small database's limit. Keep the
	// pool tiny and short-lived.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(30 * time.Second)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("reaching the panel database: %w", err)
	}
	if err := ensureSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureUsageSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureAlertsSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureJobsSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureWindowsSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureLineageSchema(ctx, db); err != nil {
		return nil, err
	}
	return &Backend{db: db}, nil
}

// Close releases the connection pool.
func (b *Backend) Close() error { return b.db.Close() }

func ensureSchema(ctx context.Context, db *sql.DB) error {
	// One row, id fixed at 1. The version is what a save writes against, so two
	// instances cannot both believe they wrote the same generation.
	const ddl = `
CREATE TABLE IF NOT EXISTS panel_state (
    id      integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    version bigint  NOT NULL DEFAULT 0,
    data    jsonb   NOT NULL DEFAULT '{}'::jsonb
);
INSERT INTO panel_state (id, version, data)
     VALUES (1, 0, '{}'::jsonb)
ON CONFLICT (id) DO NOTHING;`
	_, err := db.ExecContext(ctx, ddl)
	return err
}

// Load returns the current blob and its version. A never-written panel reads as
// an empty object, which the store treats as "not set up yet".
func (b *Backend) Load() ([]byte, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var version int64
	var data []byte
	err := b.db.QueryRowContext(ctx,
		`SELECT version, data FROM panel_state WHERE id = 1`).Scan(&version, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	// An empty object is "nothing here yet"; hand back nil so the store reads it
	// as a fresh panel rather than trying to unmarshal {} into fields.
	if string(data) == "{}" {
		return nil, version, nil
	}
	return data, version, nil
}

// Save writes new bytes only if the version has not moved since load, and
// reports whether it won. A lost compare-and-swap is not an error — the store
// re-reads and re-decides against the winner.
func (b *Backend) Save(raw []byte, expected int64) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := b.db.ExecContext(ctx,
		`UPDATE panel_state SET version = version + 1, data = $1::jsonb
		  WHERE id = 1 AND version = $2`, string(raw), expected)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
