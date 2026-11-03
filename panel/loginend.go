package panel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// A Claude sign-in has a hard end. Its refresh token stops working about four
// weeks after the browser sign-in (Anthropic answers "Refresh token expired"),
// and the gateway refreshing the access token every eight hours never moves
// that date: it rolls the short token forward inside a sign-in that is still
// running out. The date is stamped in the credential at sign-in time, so the
// panel can read it long before the login dies and say so, instead of finding
// out from the first member who gets turned away.
//
// Earlier code (and its commit message) treated a shared login as one that does
// not expire. It does; the only thing that renews it is signing in again and
// adding it to the panel.

const (
	// loginWarnWithin is how far ahead of its end a login starts to warn.
	loginWarnWithin = 7 * 24 * time.Hour
	// loginEndedFor is how long after its end a login is still announced; past
	// that it is an old account nobody is waiting on.
	loginEndedFor = 7 * 24 * time.Hour
)

// loginEnd is when the account's stored sign-in runs out, read from the sealed
// credential. ok is false when there is nothing to read (no login, no key, or a
// credential that carries no date), in which case nothing is said.
func (s *Server) loginEnd(a Account) (end time.Time, ok bool) {
	if s.secret == nil || !a.HasLogin() {
		return time.Time{}, false
	}
	raw, err := s.secret.Open(a.Credential)
	if err != nil {
		return time.Time{}, false
	}
	return loginEndOf(raw)
}

// loginEndOf reads the end date out of a stored credential's JSON.
func loginEndOf(raw []byte) (time.Time, bool) {
	var b struct {
		O struct {
			RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &b) != nil || b.O.RefreshTokenExpiresAt <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(b.O.RefreshTokenExpiresAt).UTC(), true
}

// sameSignInProblem is what a machine is told when it hands up a sign-in that is
// not newer than the one the panel already holds. Re-adding it would change
// nothing, and could replace a login the gateway has been keeping alive with
// this machine's older copy of it.
func sameSignInProblem(name string, held time.Time, now time.Time) string {
	when := "ended " + inWords(held.Sub(now)) + " ago"
	if held.After(now) {
		when = "ends in " + inWords(held.Sub(now))
	}
	return "That is the same sign-in " + name + " already has on the panel (it " + when + "), so adding it again would change nothing. " +
		"Sign in to the account again on this machine first, then add it."
}

// inWords is a span the way a person says it: "6 days", "5 hours", "20 minutes".
func inWords(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d >= 48*time.Hour:
		return unit(int(d/(24*time.Hour)), "day")
	case d >= time.Hour:
		return unit(int(d/time.Hour), "hour")
	case d >= time.Minute:
		return unit(int(d/time.Minute), "minute")
	}
	return "under a minute"
}

// loginEndWarning is the line for the board when the login is close to its end
// or past it, "" when it is far off. It is worded in spans, not clock times,
// because the panel cannot know the reader's time zone.
func loginEndWarning(left time.Duration) string {
	const fix = "Claude logins last about four weeks and signing in again is the only way to renew one: sign in to the account and add it to the panel from that machine's clawdh page."
	switch {
	case left <= 0:
		return "Its login ended " + inWords(left) + " ago. " + fix
	case left <= loginWarnWithin:
		return "Its login ends in " + inWords(left) + ". " + fix
	}
	return ""
}

// applyLoginEnds puts each account's login end on the board: always as a date,
// and as a warning when it is close. A login already
// past its end replaces the first-party guess a failed refresh leaves behind,
// since the cause is now known; an approaching one only fills an empty warning.
func (s *Server) applyLoginEnds(d Data, accounts []accountView) {
	byID := make(map[string]int, len(accounts))
	for i, v := range accounts {
		byID[v.ID] = i
	}
	now := s.now()
	for _, a := range d.Accounts {
		i, shown := byID[a.ID]
		end, ok := s.loginEnd(a)
		if !shown || !ok {
			continue
		}
		accounts[i].LoginEndsAt = end
		left := end.Sub(now)
		if left > 0 && accounts[i].Warning != "" {
			continue
		}
		if w := loginEndWarning(left); w != "" {
			accounts[i].Warning = w
		}
	}
}

// loginEndNotices tells the people an account matters to — whoever added it and
// everyone who can use it — that its login is about to end or has. The band is
// part of the notice id, so each of "7 days", "3 days", "1 day" and "ended" is
// shown once rather than the same line every check-in.
func (s *Server) loginEndNotices(personID string, d Data) []Notice {
	now := s.now()
	var out []Notice
	for _, a := range d.Accounts {
		if !concerns(d, a, personID) {
			continue
		}
		end, ok := s.loginEnd(a)
		if !ok {
			continue
		}
		left := end.Sub(now)
		band := ""
		switch {
		case left <= 0 && -left <= loginEndedFor:
			band = "ended"
		case left > 0 && left <= 24*time.Hour:
			band = "1d"
		case left > 0 && left <= 3*24*time.Hour:
			band = "3d"
		case left > 0 && left <= loginWarnWithin:
			band = "7d"
		}
		if band == "" {
			continue
		}
		body := a.Name + "'s login ends in " + inWords(left)
		if left <= 0 {
			body = a.Name + "'s login ended " + inWords(left) + " ago"
		}
		out = append(out, Notice{
			ID:   fmt.Sprintf("loginend:%s:%d:%s", a.ID, end.Unix(), band),
			Body: body + " — its owner needs to sign in to it again and add it to the panel",
		})
	}
	return out
}

// concerns reports whether an account matters to a person: they added it, or
// they can use it.
func concerns(d Data, a Account, personID string) bool {
	if a.AddedBy == personID {
		return true
	}
	for _, sh := range d.Shares {
		if sh.AccountID == a.ID && sh.PersonID == personID {
			return true
		}
	}
	return false
}

// loginEnded reports whether the account's login is already past its end, so the
// "broke — re-add it" notice does not repeat what the ended notice says.
func (s *Server) loginEnded(a Account) bool {
	end, ok := s.loginEnd(a)
	return ok && !end.After(s.now())
}

// loginEndOrZero is loginEnd for a struct literal: the zero time when unknown.
func loginEndOrZero(s *Server, a Account) time.Time {
	end, _ := s.loginEnd(a)
	return end
}
