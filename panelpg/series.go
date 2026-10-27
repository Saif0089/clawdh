package panelpg

import (
	"context"
	"time"

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
// Buckets are UTC, weeks begin on Monday, and a bucket with nothing in it is not
// returned.
func (b *Backend) PersonSeries(ctx context.Context, personID string, res panel.Resolution, from time.Time) ([]panel.SeriesPoint, error) {
	var query string
	if res.Raw {
		query = `
			SELECT date_trunc('` + res.Unit + `', at AT TIME ZONE 'UTC') AS t,
			       SUM(weighted_tokens),
			       SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens),
			       COUNT(*)
			  FROM usage_events
			 WHERE ($1 = '' OR person_id = $1) AND at >= $2
			 GROUP BY 1 ORDER BY 1`
	} else {
		query = `
			SELECT date_trunc('` + res.Unit + `', window_start AT TIME ZONE 'UTC') AS t,
			       SUM(weighted_tokens),
			       SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens),
			       0
			  FROM usage_counters
			 WHERE subject_type = 'person' AND ($1 = '' OR subject_id = $1) AND window_start >= $2
			 GROUP BY 1 ORDER BY 1`
	}
	rows, err := b.db.QueryContext(ctx, query, personID, from.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []panel.SeriesPoint
	for rows.Next() {
		var p panel.SeriesPoint
		if err := rows.Scan(&p.T, &p.Weighted, &p.Tokens, &p.Requests); err != nil {
			return nil, err
		}
		p.T = p.T.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
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
