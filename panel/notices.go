package panel

import (
	"context"
	"fmt"
	"time"
)

// Notices the panel computes for the person at a checking-in machine. These are
// the events the machine can't see for itself — its own quota standing, and an
// account whose shared login just broke — so the panel works them out from the
// metering it holds and hands them back on the check-in. They are read-only and
// best-effort: a metering read that errors just yields no notice, never a failed
// check-in. A file-backed panel with no metering (s.usage == nil) yields none.

// checkinNotices is every notice for the person this machine belongs to.
func (s *Server) checkinNotices(ctx context.Context, personID string, d Data) []Notice {
	if s.usage == nil || personID == "" {
		return nil
	}
	var out []Notice
	out = append(out, s.quotaNotice(ctx, personID, d)...)
	out = append(out, s.collisionNotices(ctx, personID, d)...)
	return out
}

// quotaNotice is a single "approaching / over your limit" notice, for the
// tightest of the person's own and the org's limits — the same limits the
// gateway enforces. Nothing under 75% is worth a notice.
//
// A person's window ceiling is measured the way the board measures it: against
// the fullest weekly window among the accounts they can use (ceilingWindow),
// since the metering layer's LimitUsage has no account in hand for a person.
func (s *Server) quotaNotice(ctx context.Context, personID string, d Data) []Notice {
	limits, err := s.usage.ListLimits(ctx)
	if err != nil {
		return nil
	}
	var windows []AccountWindow
	if hasPercent(limits) {
		windows, _ = s.usage.AccountWindows(ctx)
	}
	var best float64
	var window string
	var ceiling bool
	var reset time.Time
	for _, l := range limits {
		if !(l.SubjectType == "org" || (l.SubjectType == "person" && l.SubjectID == personID)) {
			continue
		}
		frac, r, err := s.usage.LimitUsage(ctx, l)
		if err != nil {
			continue
		}
		isCeiling := false
		if l.MaxPercent != nil && *l.MaxPercent > 0 {
			if w, ok := ceilingWindow(d, windows, l); ok {
				if c := w.SevenD / *l.MaxPercent; c > frac {
					frac, isCeiling = c, true
					if !w.SevenDReset.IsZero() {
						r = w.SevenDReset
					}
				}
			}
		}
		if frac > best {
			best, window, ceiling, reset = frac, l.WindowKind, isCeiling, r
		}
	}
	th := quotaThreshold(best)
	if th == "" {
		return nil
	}
	// The reset time keys the notice, so a fresh window re-notifies but a standing
	// one over the same threshold does not.
	return []Notice{{
		ID:   fmt.Sprintf("quota:%s:%d", th, reset.Unix()),
		Body: quotaBody(th, window, ceiling),
	}}
}

// collisionNotices tells a person any account they can use whose shared login
// just stopped working (used first-party outside the gateway, which rotates the
// token out from under it) — so whoever can re-add it learns it needs re-adding.
func (s *Server) collisionNotices(ctx context.Context, personID string, d Data) []Notice {
	cols, err := s.usage.RecentCollisions(ctx, s.now().Add(-24*time.Hour))
	if err != nil || len(cols) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []Notice
	for _, sh := range d.Shares {
		if sh.PersonID != personID || seen[sh.AccountID] {
			continue
		}
		at, broke := cols[sh.AccountID]
		if !broke {
			continue
		}
		seen[sh.AccountID] = true
		name := sh.AccountID
		if acct, ok := d.Account(sh.AccountID); ok {
			name = acct.Name
		}
		out = append(out, Notice{
			ID:   fmt.Sprintf("collision:%s:%d", sh.AccountID, at.Unix()),
			Body: name + "'s shared login broke — re-add it",
		})
	}
	return out
}

// quotaThreshold is the highest usage band a fraction has crossed, or "" under
// the first one — the 75% / 95% marks the gateway warns and blocks at.
func quotaThreshold(frac float64) string {
	switch {
	case frac >= 1.0:
		return "cap"
	case frac >= 0.95:
		return "95"
	case frac >= 0.75:
		return "75"
	}
	return ""
}

// quotaBody is the short line for a quota notice. A ceiling is about the
// account's window rather than the person's own spend, so it says so.
func quotaBody(threshold, windowKind string, ceiling bool) string {
	if ceiling {
		if threshold == "cap" {
			return "The account's weekly window is past your ceiling — you're turned away until it resets"
		}
		return "The account's weekly window is at " + threshold + "% of your ceiling"
	}
	w := windowWord(windowKind)
	if threshold == "cap" {
		return "You're over your " + w + "limit"
	}
	return "You're at " + threshold + "% of your " + w + "limit"
}

// windowWord turns a window kind into the adjective for a sentence, with a
// trailing space so an unknown kind reads "your limit", not "your  limit".
func windowWord(kind string) string {
	switch kind {
	case "day":
		return "daily "
	case "week":
		return "weekly "
	case "month":
		return "monthly "
	}
	return ""
}

// checkinWindows is the gateway's captured utilisation for each account the
// person can use — keyed by the share's slug and the account's email, so the
// machine can pair a reading with either a shared card or a local login the
// gateway now holds. Nothing without metering.
func (s *Server) checkinWindows(ctx context.Context, personID string, d Data) []ShareWindow {
	if s.usage == nil || personID == "" {
		return nil
	}
	rows, err := s.usage.AccountWindows(ctx)
	if err != nil {
		return nil
	}
	byAccount := map[string]AccountWindow{}
	for _, w := range rows {
		byAccount[w.AccountID] = w
	}
	var out []ShareWindow
	slugs := shareSlugs(d, personID)
	for _, sh := range d.Shares {
		if sh.PersonID != personID {
			continue
		}
		w, ok := byAccount[sh.AccountID]
		if !ok {
			continue
		}
		acct, _ := d.Account(sh.AccountID)
		out = append(out, ShareWindow{
			Slug: slugs[acct.ID], Email: acct.Email,
			FiveH: w.FiveH, SevenD: w.SevenD, FiveHReset: w.FiveHReset, SevenDReset: w.SevenDReset, UpdatedAt: w.UpdatedAt,
		})
	}
	return out
}
