package panelpg

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"clawdh/panel"
)

// A panel that already has the old limits table — token and dollar caps per
// calendar window beside a nullable ceiling — comes up with only its ceilings:
// the other rows go, then their columns, and the schema is the ceiling-only one
// a fresh panel gets. Opening it again is a no-op.
func TestOpeningMigratesTheOldLimitsTableToCeilingsOnly(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
		DROP TABLE IF EXISTS limits;
		CREATE TABLE limits (
		    id                  text PRIMARY KEY,
		    subject_type        text NOT NULL,
		    subject_id          text NOT NULL DEFAULT '',
		    window_kind         text NOT NULL,
		    max_weighted_tokens double precision,
		    max_cost_usd        double precision,
		    max_percent         double precision,
		    created_at          timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO limits (id, subject_type, subject_id, window_kind, max_weighted_tokens, max_cost_usd, max_percent) VALUES
		    ('tok',  'person',  'p1', 'day',   500000, NULL, NULL),
		    ('usd',  'org',     '',   'month', NULL,   40,   NULL),
		    ('ceil', 'account', 'a1', 'week',  NULL,   NULL, 0.6)`); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 2; pass++ {
		b, err := Open(ctx, dsn(t))
		if err != nil {
			t.Fatalf("open (pass %d): %v", pass, err)
		}
		got, err := b.ListLimits(ctx)
		b.Close()
		if err != nil {
			t.Fatalf("list (pass %d): %v", pass, err)
		}
		if len(got) != 1 || got[0].ID != "ceil" || got[0].SubjectType != "account" || got[0].SubjectID != "a1" || got[0].MaxPercent != 0.6 {
			t.Fatalf("pass %d: limits after opening = %+v, want only the 60%% ceiling on a1", pass, got)
		}
	}

	var cols []string
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'limits' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if got := strings.Join(cols, ","); got != "created_at,id,max_percent,subject_id,subject_type" {
		t.Errorf("limits columns = %s, want the ceiling-only set", got)
	}
}

// A ceiling replaces any earlier one on the same subject, and the gateway's
// standing for a request is the tightest of the person's own and the
// account's — read against the account's real weekly window, and open until
// there is a reading.
func TestACeilingIsCheckedAgainstTheAccountsWeeklyWindow(t *testing.T) {
	ctx := context.Background()
	b := freshBackend(t)
	if _, err := b.db.ExecContext(ctx, `DELETE FROM limits; DELETE FROM account_windows`); err != nil {
		t.Fatal(err)
	}
	if err := b.SetLimit(ctx, panel.Limit{SubjectType: "person", SubjectID: "p1", MaxPercent: 0.8}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetLimit(ctx, panel.Limit{SubjectType: "person", SubjectID: "p1", MaxPercent: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetLimit(ctx, panel.Limit{SubjectType: "account", SubjectID: "a1", MaxPercent: 0.9}); err != nil {
		t.Fatal(err)
	}
	limits, err := b.ListLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 {
		t.Fatalf("limits = %+v, want p1's latest ceiling and a1's", limits)
	}

	// No reading yet: nothing applies.
	st, err := b.MemberLimitStatus(ctx, "p1", "a1")
	if err != nil || st.Over || st.Fraction != 0 {
		t.Fatalf("with no reading, status = %+v, %v; want open", st, err)
	}

	reset := time.Now().Add(40 * time.Hour).UTC().Truncate(time.Second)
	b.RecordWindows("a1", 0.1, 0.45, time.Time{}, reset)
	st, err = b.MemberLimitStatus(ctx, "p1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	// 0.45 of p1's 0.5 ceiling (90%) is tighter than 0.45 of a1's 0.9 (50%).
	if st.Over || st.Fraction < 0.899 || st.Fraction > 0.901 || !st.ResetAt.Equal(reset) {
		t.Errorf("status = %+v, want 90%% of p1's ceiling with the window's reset", st)
	}
	if !strings.Contains(st.Message, "45% full, past the 50% ceiling set for you") {
		t.Errorf("message = %q", st.Message)
	}

	b.RecordWindows("a1", 0.1, 0.55, time.Time{}, reset)
	st, _ = b.MemberLimitStatus(ctx, "p1", "a1")
	if !st.Over {
		t.Errorf("at 55%% of a 50%% ceiling, status = %+v, want over", st)
	}

	// Someone with no ceiling of their own is held only by the account's.
	st, _ = b.MemberLimitStatus(ctx, "p2", "a1")
	if st.Over || st.Fraction < 0.61 || st.Fraction > 0.612 {
		t.Errorf("p2's status = %+v, want 0.55 of a1's 0.9 ceiling", st)
	}
	if !strings.Contains(st.Message, "set for everyone using it") {
		t.Errorf("p2's message = %q, want it to say the ceiling is the account's", st.Message)
	}
}
