package panel

import (
	"net/http"
	"strings"
	"time"
)

// Limit is one configured quota (a mirror of the row the Postgres layer stores).
// A limit caps a person, one account, or the whole org to a number of weighted
// tokens and/or a USD amount within a calendar window that resets daily, weekly,
// or monthly.
type Limit struct {
	ID          string   `json:"id"`
	SubjectType string   `json:"subjectType"` // 'person' | 'account' | 'org'
	SubjectID   string   `json:"subjectId"`   // '' for org-wide
	WindowKind  string   `json:"windowKind"`  // 'day' | 'week' | 'month'
	MaxWeighted *float64 `json:"maxWeighted,omitempty"`
	MaxCostUSD  *float64 `json:"maxCostUsd,omitempty"`
	// MaxPercent is a ceiling on an account's weekly window — Anthropic's own
	// number for how full the window is, read off the login — past which the
	// subject is turned away. 0..1. For an account it applies to everyone using
	// it; for a person, to whichever account they are using. Always a weekly
	// window; enforced from the utilisation the gateway captures, so it does
	// nothing until a reading exists (fail-open).
	MaxPercent *float64 `json:"maxPercent,omitempty"`
}

func (s *Server) handleListLimits(w http.ResponseWriter, r *http.Request) {
	limits, err := s.usage.ListLimits(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "The quotas could not be read: "+err.Error())
		return
	}
	d, _ := s.store.Load()
	// The accounts' weekly windows, for the % ceilings: read once for the list.
	var windows []AccountWindow
	if hasPercent(limits) {
		windows, _ = s.usage.AccountWindows(r.Context())
	}
	out := make([]map[string]any, 0, len(limits))
	for _, l := range limits {
		name := "the whole team"
		switch l.SubjectType {
		case "person":
			name = s.subjectName(d, "person", l.SubjectID)
		case "account":
			name = s.subjectName(d, "account", l.SubjectID)
		}
		// How much of this limit is used right now, so the board can flag the ones
		// approaching their cap. Best-effort: a usage read that errors just leaves
		// the row without a live figure rather than failing the whole list.
		frac, reset, _ := s.usage.LimitUsage(r.Context(), l)
		row := map[string]any{"limit": l, "name": name, "fraction": frac, "resetAt": reset}
		// A % ceiling is measured against an account's weekly window. For an
		// account that is its own; for a person it is the fullest window among
		// the accounts they can use — the one that would turn them away first.
		// The row says which, so the board can name it, and its reset is the
		// window's own rolling reset — what actually frees it — not the
		// calendar week the metered caps run on.
		if l.MaxPercent != nil && *l.MaxPercent > 0 {
			if w, ok := ceilingWindow(d, windows, l); ok {
				row["window"] = w.SevenD
				row["windowAccount"] = d.accountName(w.AccountID)
				if l.SubjectType == "person" {
					row["fraction"] = w.SevenD / *l.MaxPercent
				}
				if !w.SevenDReset.IsZero() {
					row["resetAt"] = w.SevenDReset
				}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"limits": out})
}

func (s *Server) handleSetLimit(w http.ResponseWriter, r *http.Request) {
	var l Limit
	if err := readJSON(r, &l); err != nil {
		fail(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	l.SubjectType = strings.ToLower(strings.TrimSpace(l.SubjectType))
	l.WindowKind = strings.ToLower(strings.TrimSpace(l.WindowKind))
	l.SubjectID = strings.TrimSpace(l.SubjectID)
	if l.SubjectType != "person" && l.SubjectType != "account" && l.SubjectType != "org" {
		fail(w, http.StatusBadRequest, "A quota applies to a person, an account, or the whole team.")
		return
	}
	if l.SubjectType == "org" {
		l.SubjectID = ""
	} else if l.SubjectID == "" {
		fail(w, http.StatusBadRequest, "Pick which person or account this quota applies to.")
		return
	}
	// A window ceiling is a percentage of an account's weekly window, so it
	// forces a weekly window and can't apply to the whole team. Accept 0..1 or
	// a 0..100 percentage.
	if l.MaxPercent != nil {
		p := *l.MaxPercent
		if p > 1 {
			p /= 100
		}
		if p <= 0 || p > 1 {
			fail(w, http.StatusBadRequest, "A window ceiling is a percent between 0 and 100.")
			return
		}
		if l.SubjectType == "org" {
			fail(w, http.StatusBadRequest, "A % of the weekly window applies to a person or an account, not the whole team.")
			return
		}
		l.WindowKind = "week"
		l.MaxPercent = &p
	}
	switch l.WindowKind {
	case "day", "week", "month":
	default:
		fail(w, http.StatusBadRequest, "A quota resets daily, weekly, or monthly.")
		return
	}
	if l.MaxWeighted == nil && l.MaxCostUSD == nil && l.MaxPercent == nil {
		fail(w, http.StatusBadRequest, "Set a % of the weekly window, a weighted-token cap, or a USD cap.")
		return
	}
	if err := s.usage.SetLimit(r.Context(), l); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteLimit(w http.ResponseWriter, r *http.Request) {
	if err := s.usage.DeleteLimit(r.Context(), r.PathValue("id")); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasPercent(limits []Limit) bool {
	for _, l := range limits {
		if l.MaxPercent != nil && *l.MaxPercent > 0 {
			return true
		}
	}
	return false
}

// ceilingWindow is the weekly window a % ceiling is checked against: the
// account's own for an account limit; for a person, the fullest among the
// accounts currently shared with them (an expired loan is not one they can
// use). False when no reading exists at all — a ceiling with nothing to
// measure against, which fails open.
func ceilingWindow(d Data, windows []AccountWindow, l Limit) (AccountWindow, bool) {
	byAccount := make(map[string]AccountWindow, len(windows))
	for _, w := range windows {
		byAccount[w.AccountID] = w
	}
	if l.SubjectType == "account" {
		w, ok := byAccount[l.SubjectID]
		return w, ok
	}
	var best AccountWindow
	found := false
	now := time.Now()
	for _, sh := range d.Shares {
		if sh.PersonID != l.SubjectID || !sh.Live(now) {
			continue
		}
		w, ok := byAccount[sh.AccountID]
		if !ok {
			continue
		}
		if !found || w.SevenD > best.SevenD {
			best, found = w, true
		}
	}
	return best, found
}
