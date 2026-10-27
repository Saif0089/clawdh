package panelpg

import (
	"context"
	"time"

	"clawdh/internal/meter"
	"clawdh/panel"
)

// PersonSeries is one person's usage cut into buckets from `from` on (everyone's,
// for an empty person), at the grain res names.
//
// The finest grains read the events, which are exact to the request but are an
// index range scan over minutes or hours, never over history. From an hour up
// it reads the hourly roll-up, which RecordUsage keeps in the same transaction
// as each event: a month is a few hundred rows however many requests were made,
// and the two agree to the token (series_test.go holds them to that). The unit
// comes from res, never from a request, so it is safe to put in the query.
//
// Every bucket is also split by token class. The raw counts are stored as they
// are; the weighted split is read per model, since a class weighs what its
// model's rate says, and is scaled to the weighted tokens the gateway recorded,
// so the classes always add up to the bucket rather than to a recomputation.
//
// A session narrows it to the requests one Claude Code session made, subagents
// included (they carry their session's id). The roll-up does not know sessions,
// so a session is always read from the events, at every grain; one session is a
// few thousand of them at most.
//
// Buckets are UTC, weeks begin on Monday, and a bucket with nothing in it is not
// returned.
func (b *Backend) PersonSeries(ctx context.Context, personID, sessionID string, res panel.Resolution, from time.Time) ([]panel.SeriesPoint, error) {
	var query string
	args := []any{personID, from.UTC()}
	if sessionID != "" {
		query = `
			SELECT date_trunc('` + res.Unit + `', at AT TIME ZONE 'UTC') AS t, model,
			       SUM(weighted_tokens), SUM(input_tokens), SUM(output_tokens),
			       SUM(cache_creation_tokens), SUM(cache_read_tokens), COUNT(*)
			  FROM usage_events
			 WHERE person_id = $1 AND session_id = $3 AND at >= $2
			 GROUP BY 1, 2 ORDER BY 1`
		args = append(args, sessionID)
	} else if res.Raw {
		query = `
			SELECT date_trunc('` + res.Unit + `', at AT TIME ZONE 'UTC') AS t, model,
			       SUM(weighted_tokens), SUM(input_tokens), SUM(output_tokens),
			       SUM(cache_creation_tokens), SUM(cache_read_tokens), COUNT(*)
			  FROM usage_events
			 WHERE ($1 = '' OR person_id = $1) AND at >= $2
			 GROUP BY 1, 2 ORDER BY 1`
	} else {
		query = `
			SELECT date_trunc('` + res.Unit + `', window_start AT TIME ZONE 'UTC') AS t, model,
			       SUM(weighted_tokens), SUM(input_tokens), SUM(output_tokens),
			       SUM(cache_creation_tokens), SUM(cache_read_tokens), 0
			  FROM usage_counters
			 WHERE subject_type = 'person' AND ($1 = '' OR subject_id = $1) AND window_start >= $2
			 GROUP BY 1, 2 ORDER BY 1`
	}
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.SeriesPoint
	for rows.Next() {
		var (
			t        time.Time
			model    string
			weighted float64
			u        meter.Usage
			requests int64
		)
		if err := rows.Scan(&t, &model, &weighted, &u.Input, &u.Output, &u.CacheCreation, &u.CacheRead, &requests); err != nil {
			return nil, err
		}
		t = t.UTC()
		if n := len(out); n == 0 || !out[n-1].T.Equal(t) {
			out = append(out, panel.SeriesPoint{T: t})
		}
		addModel(&out[len(out)-1], model, weighted, u, requests)
	}
	return out, rows.Err()
}

// PersonSessions is the Claude Code sessions a person ran requests in since a
// time, newest first: when each began and last ran, and what it came to, split
// by class like a series bucket. Requests from before sessions were recorded, or
// from a client that does not name its session, are not a session and are left
// out.
func (b *Backend) PersonSessions(ctx context.Context, personID string, since time.Time, limit int) ([]panel.SessionUsage, error) {
	rows, err := b.db.QueryContext(ctx, `
		WITH recent AS (
			SELECT session_id, MAX(at) AS last
			  FROM usage_events
			 WHERE person_id = $1 AND session_id <> '' AND at >= $2
			 GROUP BY 1 ORDER BY 2 DESC LIMIT $3)
		SELECT e.session_id, r.last, MIN(e.at), e.model,
		       SUM(e.weighted_tokens), SUM(e.input_tokens), SUM(e.output_tokens),
		       SUM(e.cache_creation_tokens), SUM(e.cache_read_tokens), COUNT(*)
		  FROM usage_events e JOIN recent r USING (session_id)
		 WHERE e.person_id = $1
		 GROUP BY 1, 2, 4 ORDER BY 2 DESC, 1`, personID, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.SessionUsage
	for rows.Next() {
		var (
			id          string
			last, first time.Time
			model       string
			weighted    float64
			u           meter.Usage
			requests    int64
		)
		if err := rows.Scan(&id, &last, &first, &model, &weighted, &u.Input, &u.Output, &u.CacheCreation, &u.CacheRead, &requests); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != id {
			out = append(out, panel.SessionUsage{ID: id, First: first.UTC(), Last: last.UTC(), Usage: panel.SeriesPoint{T: last.UTC()}})
		}
		s := &out[len(out)-1]
		if first.Before(s.First) {
			s.First = first.UTC()
		}
		addModel(&s.Usage, model, weighted, u, requests)
	}
	return out, rows.Err()
}

// addModel folds one model's part of a bucket into it.
func addModel(p *panel.SeriesPoint, model string, weighted float64, u meter.Usage, requests int64) {
	p.Weighted += weighted
	p.Tokens += u.Input + u.Output + u.CacheCreation + u.CacheRead
	p.Requests += requests
	p.Input.Tokens += u.Input
	p.Output.Tokens += u.Output
	p.CacheWrite.Tokens += u.CacheCreation
	p.CacheRead.Tokens += u.CacheRead
	parts := meter.WeightedParts(model, u)
	sum := parts.Sum()
	if sum <= 0 {
		return // a model with no known rate was recorded with no weight to split
	}
	f := weighted / sum
	p.Input.Weighted += parts.Input * f
	p.Output.Weighted += parts.Output * f
	p.CacheWrite.Weighted += parts.CacheCreation * f
	p.CacheRead.Weighted += parts.CacheRead * f
}

// UnlinkedAccountUsage is the weighted tokens metered since a time on account IDs
// that are neither one of currentIDs nor an earlier ID of the same login as one
// of them: usage the gateway recorded on a login the panel has no trace of. An
// earlier ID of a login still on the panel is not unlinked: it is that login's
// history, and the login's own card already counts it.
func (b *Backend) UnlinkedAccountUsage(ctx context.Context, currentIDs []string, since time.Time) (float64, error) {
	if currentIDs == nil {
		currentIDs = []string{}
	}
	var total float64
	err := b.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(c.weighted_tokens), 0)
		  FROM usage_counters c
		 WHERE c.subject_type = 'account' AND c.window_start >= $1
		   AND c.subject_id <> ALL($2::text[])
		   AND NOT EXISTS (
		       SELECT 1 FROM account_lineage l
		         JOIN account_lineage cur ON cur.login_key = l.login_key
		        WHERE l.account_id = c.subject_id AND cur.account_id = ANY($2::text[]))`,
		since.UTC(), currentIDs).Scan(&total)
	return total, err
}
