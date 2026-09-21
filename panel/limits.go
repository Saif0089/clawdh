package panel

import (
	"net/http"
	"strings"
	"time"
)

// A quota on this panel is a window ceiling and nothing else: a percentage of
// an account's weekly window — Anthropic's own number for how full it is, read
// off the login — past which the subject is turned away. For an account it
// holds everyone using it (a reserve); for a person, whichever account they are
// using ("they can use it while its week is under 60% full"). The gateway
// enforces it from the utilisation it captures, so it does nothing until a
// reading exists (fail-open). Earlier builds also capped metered tokens and a
// notional dollar figure per calendar window; those were a second, competing
// number for the same window, and a subscription has no per-token price.

// Limit is one configured ceiling (a mirror of the row the Postgres layer stores).
type Limit struct {
	ID          string  `json:"id"`
	SubjectType string  `json:"subjectType"` // 'person' | 'account'
	SubjectID   string  `json:"subjectId"`
	MaxPercent  float64 `json:"maxPercent"` // 0..1 of the weekly window
}

func (s *Server) handleListLimits(w http.ResponseWriter, r *http.Request) {
	limits, err := s.usage.ListLimits(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "The quotas could not be read: "+err.Error())
		return
	}
	d, _ := s.store.Load()
	// The accounts' weekly windows, which every ceiling is measured against:
	// read once for the list. A read that fails leaves every row without a
	// standing rather than failing the list.
	var windows []AccountWindow
	if len(limits) > 0 {
		windows, _ = s.usage.AccountWindows(r.Context())
	}
	out := make([]map[string]any, 0, len(limits))
	for _, l := range limits {
		row := map[string]any{"limit": l, "name": s.subjectName(d, l.SubjectType, l.SubjectID), "fraction": 0.0}
		// The standing: how full the window it is checked against is, over the
		// ceiling. For an account that is its own; for a person it is the fullest
		// window among the accounts they can use — the one that would turn them
		// away first. The row says which, so the board can name it, and its
		// reset is the window's own rolling reset — what actually frees it.
		if win, ok := ceilingWindow(d, windows, l); ok {
			row["fraction"] = win.SevenD / l.MaxPercent
			row["window"] = win.SevenD
			row["windowAccount"] = d.accountName(win.AccountID)
			if !win.SevenDReset.IsZero() {
				row["resetAt"] = win.SevenDReset
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
	l.SubjectID = strings.TrimSpace(l.SubjectID)
	if l.SubjectType != "person" && l.SubjectType != "account" {
		fail(w, http.StatusBadRequest, "A ceiling applies to a person or an account.")
		return
	}
	if l.SubjectID == "" {
		fail(w, http.StatusBadRequest, "Pick which person or account this ceiling applies to.")
		return
	}
	// Accept 0..1 or a 0..100 percentage.
	if l.MaxPercent > 1 {
		l.MaxPercent /= 100
	}
	if l.MaxPercent <= 0 || l.MaxPercent > 1 {
		fail(w, http.StatusBadRequest, "A window ceiling is a percent between 0 and 100.")
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

// ceilingWindow is the weekly window a ceiling is checked against: the
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
