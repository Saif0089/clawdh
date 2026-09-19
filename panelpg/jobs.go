package panelpg

import (
	"context"
	"database/sql"
	"time"

	"clawdh/panel"
)

// Remote jobs. A job is a small, consented request the admin makes of one
// enrolled machine — "diagnose yourself", "list your sessions", "send session
// X's transcript" — that the machine picks up at its next check-in, runs only
// if its owner turned remote help on, and posts the answer back to. Results can
// be large (a transcript), so jobs are a relational table, not the JSON blob.
const jobsDDL = `
CREATE TABLE IF NOT EXISTS jobs (
    id           text PRIMARY KEY,
    device_id    text NOT NULL,
    person_id    text NOT NULL DEFAULT '',
    kind         text NOT NULL,                    -- 'diagnose' | 'sessions' | 'transcript'
    params       text NOT NULL DEFAULT '',         -- kind-specific argument (e.g. a session id)
    status       text NOT NULL DEFAULT 'pending',  -- 'pending' | 'done' | 'error'
    result       text NOT NULL DEFAULT '',
    requested_by text NOT NULL DEFAULT '',          -- the admin who asked
    created_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz
);
CREATE INDEX IF NOT EXISTS jobs_device_status ON jobs(device_id, status);
CREATE INDEX IF NOT EXISTS jobs_created       ON jobs(created_at);`

func ensureJobsSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, jobsDDL)
	return err
}

// EnqueueJob records a new pending job for a device to pick up at its next
// check-in.
func (b *Backend) EnqueueJob(ctx context.Context, j panel.Job) (panel.Job, error) {
	if j.ID == "" {
		j.ID = newHexID()
	}
	j.Status = "pending"
	j.CreatedAt = time.Now()
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO jobs (id, device_id, person_id, kind, params, status, requested_by, created_at)
		VALUES ($1,$2,$3,$4,$5,'pending',$6,$7)`,
		j.ID, j.DeviceID, j.PersonID, j.Kind, j.Params, j.RequestedBy, j.CreatedAt)
	return j, err
}

// PendingJobs returns a device's not-yet-run jobs. It does not clear them —
// posting a result is what flips a job out of pending — so a job the machine
// never gets to (offline, remote help off) simply stays queued.
func (b *Backend) PendingJobs(ctx context.Context, deviceID string) ([]panel.Job, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, kind, params, requested_by FROM jobs
		 WHERE device_id = $1 AND status = 'pending' ORDER BY created_at`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.Job
	for rows.Next() {
		var j panel.Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Params, &j.RequestedBy); err != nil {
			return nil, err
		}
		j.DeviceID, j.Status = deviceID, "pending"
		out = append(out, j)
	}
	return out, rows.Err()
}

// CompleteJob stores a device's answer to one of its own pending jobs. Scoping
// the update to the device id means a machine can only ever resolve jobs that
// were addressed to it.
func (b *Backend) CompleteJob(ctx context.Context, deviceID, jobID, status, result string) error {
	// "awaiting" is the one intermediate state: the machine has the request and is
	// holding it for its owner's approval. Everything else resolves the job.
	if status == "awaiting" {
		_, err := b.db.ExecContext(ctx, `
			UPDATE jobs SET status = 'awaiting'
			 WHERE id = $1 AND device_id = $2 AND status = 'pending'`, jobID, deviceID)
		return err
	}
	if status != "done" && status != "error" && status != "denied" {
		status = "done"
	}
	_, err := b.db.ExecContext(ctx, `
		UPDATE jobs SET status = $1, result = $2, resolved_at = now()
		 WHERE id = $3 AND device_id = $4 AND status IN ('pending', 'awaiting')`,
		status, result, jobID, deviceID)
	return err
}

// RecentJobs returns a device's jobs, newest first, for the admin to read the
// answers. limit bounds how many come back.
func (b *Backend) RecentJobs(ctx context.Context, deviceID string, limit int) ([]panel.Job, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, device_id, person_id, kind, params, status, result, requested_by, created_at, resolved_at
		  FROM jobs WHERE device_id = $1 ORDER BY created_at DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.Job
	for rows.Next() {
		var j panel.Job
		var resolved sql.NullTime
		if err := rows.Scan(&j.ID, &j.DeviceID, &j.PersonID, &j.Kind, &j.Params,
			&j.Status, &j.Result, &j.RequestedBy, &j.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		if resolved.Valid {
			t := resolved.Time
			j.ResolvedAt = &t
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
