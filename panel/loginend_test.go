package panel

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var loginNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func loginServer(t *testing.T) (*Server, *Secret) {
	t.Helper()
	secret, err := LoadSecret(filepath.Join(t.TempDir(), "panel.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{secret: secret, now: func() time.Time { return loginNow }}, secret
}

// sealedLogin is a stored credential whose refresh token ends at the given time
// (zero: a credential that carries no date).
func sealedLogin(t *testing.T, secret *Secret, ends time.Time) []byte {
	t.Helper()
	date := ""
	if !ends.IsZero() {
		date = fmt.Sprintf(`,"refreshTokenExpiresAt":%d`, ends.UnixMilli())
	}
	sealed, err := secret.Seal([]byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1` + date + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// The end of a login is read from the credential the panel already holds; where
// there is nothing to read, nothing is said rather than something invented.
func TestLoginEndIsReadFromTheSealedCredential(t *testing.T) {
	s, secret := loginServer(t)
	ends := loginNow.Add(50 * time.Hour)

	got, ok := s.loginEnd(Account{ID: "a", Credential: sealedLogin(t, secret, ends)})
	if !ok || !got.Equal(ends) {
		t.Errorf("loginEnd = %v, %v; want %v", got, ok, ends)
	}
	if _, ok := s.loginEnd(Account{ID: "b", Credential: sealedLogin(t, secret, time.Time{})}); ok {
		t.Error("a credential with no date must say nothing")
	}
	if _, ok := s.loginEnd(Account{ID: "c"}); ok {
		t.Error("an account with no login must say nothing")
	}
	if _, ok := s.loginEnd(Account{ID: "d", Credential: []byte("not sealed")}); ok {
		t.Error("a credential that does not open must say nothing")
	}
	if _, ok := (&Server{now: time.Now}).loginEnd(Account{ID: "e", Credential: []byte("x")}); ok {
		t.Error("a panel with no key must say nothing")
	}
}

func TestLoginEndWarningIsWordedInSpans(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want string // "" = no warning
	}{
		{30 * 24 * time.Hour, ""},
		{6*24*time.Hour + 5*time.Hour, "ends in 6 days"},
		{47 * time.Hour, "ends in 47 hours"},
		{5*time.Hour + 40*time.Minute, "ends in 5 hours"},
		{time.Hour, "ends in 1 hour"},
		{20 * time.Minute, "ends in 20 minutes"},
		{-3 * 24 * time.Hour, "ended 3 days ago"},
		{-30 * time.Minute, "ended 30 minutes ago"},
	} {
		got := loginEndWarning(c.left)
		if c.want == "" {
			if got != "" {
				t.Errorf("%v left: warned %q, want nothing", c.left, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%v left: %q does not say %q", c.left, got, c.want)
		}
	}
}

// The board shows the end of each login. A login already past its end replaces
// the first-party guess a failed refresh leaves behind, because the cause is now
// known; one that is only approaching never hides a real collision.
func TestTheBoardWarnsAboutLoginsRunningOut(t *testing.T) {
	s, secret := loginServer(t)
	guess := "Its shared login stopped working in the last day — most likely used directly elsewhere"
	d := Data{Accounts: []Account{
		{ID: "soon", Credential: sealedLogin(t, secret, loginNow.Add(2*24*time.Hour))},
		{ID: "far", Credential: sealedLogin(t, secret, loginNow.Add(20*24*time.Hour))},
		{ID: "ended", Credential: sealedLogin(t, secret, loginNow.Add(-3*24*time.Hour))},
		{ID: "soonbroke", Credential: sealedLogin(t, secret, loginNow.Add(2*24*time.Hour))},
		{ID: "undated", Credential: sealedLogin(t, secret, time.Time{})},
	}}
	views := []accountView{{ID: "soon"}, {ID: "far"}, {ID: "ended", Warning: guess}, {ID: "soonbroke", Warning: guess}, {ID: "undated"}}

	s.applyLoginEnds(d, views)

	if !strings.Contains(views[0].Warning, "ends in 2 days") {
		t.Errorf("soon: %q", views[0].Warning)
	}
	if views[1].Warning != "" {
		t.Errorf("far: %q, want no warning", views[1].Warning)
	}
	if !strings.Contains(views[2].Warning, "ended 3 days ago") || strings.Contains(views[2].Warning, "most likely") {
		t.Errorf("ended: %q, want the known cause in place of the guess", views[2].Warning)
	}
	if views[3].Warning != guess {
		t.Errorf("soonbroke: %q, want the collision warning kept", views[3].Warning)
	}
	if views[4].Warning != "" {
		t.Errorf("undated: %q, want nothing", views[4].Warning)
	}
}

// Whoever added a login and everyone who can use it hear it is running out, once
// per stage; a stranger hears nothing, and neither does anyone about a login that
// is far off or long dead.
func TestLoginEndNoticesGoToTheRightPeopleOncePerStage(t *testing.T) {
	s, secret := loginServer(t)
	d := Data{
		Accounts: []Account{
			{ID: "a1", Name: "Saif", AddedBy: "owner", Credential: sealedLogin(t, secret, loginNow.Add(20*time.Hour))},
			{ID: "a2", Name: "Gone", AddedBy: "owner", Credential: sealedLogin(t, secret, loginNow.Add(-2*24*time.Hour))},
			{ID: "a3", Name: "Old", AddedBy: "owner", Credential: sealedLogin(t, secret, loginNow.Add(-10*24*time.Hour))},
			{ID: "a4", Name: "Fine", AddedBy: "owner", Credential: sealedLogin(t, secret, loginNow.Add(20*24*time.Hour))},
			{ID: "a5", Name: "Mid", AddedBy: "owner", Credential: sealedLogin(t, secret, loginNow.Add(2*24*time.Hour))},
		},
		Shares: []Share{{AccountID: "a1", PersonID: "user"}},
	}
	ctx := context.Background()

	owner := s.checkinNotices(ctx, "owner", d)
	var ids []string
	for _, n := range owner {
		ids = append(ids, n.ID)
	}
	want := []string{
		fmt.Sprintf("loginend:a1:%d:1d", loginNow.Add(20*time.Hour).Unix()),
		fmt.Sprintf("loginend:a2:%d:ended", loginNow.Add(-2*24*time.Hour).Unix()),
		fmt.Sprintf("loginend:a5:%d:3d", loginNow.Add(2*24*time.Hour).Unix()),
	}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("owner's notices = %v\nwant %v", ids, want)
	}
	if !strings.Contains(owner[0].Body, "Saif's login ends in 20 hours") {
		t.Errorf("body = %q", owner[0].Body)
	}

	if got := s.checkinNotices(ctx, "user", d); len(got) != 1 || !strings.Contains(got[0].Body, "Saif") {
		t.Errorf("a person who can use only Saif got %+v, want that one notice", got)
	}
	if got := s.checkinNotices(ctx, "stranger", d); len(got) != 0 {
		t.Errorf("a stranger got %+v, want nothing", got)
	}
}

// A login that already ended is announced by the ended notice, not twice: the
// "broke — re-add it" notice a failed refresh also produces stands down.
func TestAnEndedLoginIsNotAnnouncedTwice(t *testing.T) {
	s, secret := loginServer(t)
	colAt := loginNow.Add(-time.Hour)
	s.usage = &fakeUsage{collisions: map[string]time.Time{"a1": colAt, "a2": colAt}}
	d := Data{
		Accounts: []Account{
			{ID: "a1", Name: "Ended", Credential: sealedLogin(t, secret, loginNow.Add(-24*time.Hour))},
			{ID: "a2", Name: "Collided", Credential: sealedLogin(t, secret, loginNow.Add(20*24*time.Hour))},
		},
		Shares: []Share{{AccountID: "a1", PersonID: "p"}, {AccountID: "a2", PersonID: "p"}},
	}

	var bodies []string
	for _, n := range s.checkinNotices(context.Background(), "p", d) {
		bodies = append(bodies, n.Body)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], "Collided's shared login broke") || !strings.Contains(bodies[1], "Ended's login ended 24 hours ago") {
		t.Errorf("notices = %q, want the collision for Collided and the ended notice for Ended, once each", bodies)
	}
}

// credentialEnding is a stored login (as a machine hands it up, base64) whose
// refresh token ends at the given time; a zero time leaves the date out.
func credentialEnding(ends time.Time) string {
	date := ""
	if !ends.IsZero() {
		date = fmt.Sprintf(`,"refreshTokenExpiresAt":%d`, ends.UnixMilli())
	}
	return base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1` + date + `}}`))
}

// Handing up the sign-in the panel already holds (or an older copy of it) is
// refused with the reason, because it changes nothing for a login that has ended
// and would overwrite the live login with a stale one for one that has not. A
// newer sign-in replaces it and the machine is told when it ends.
func TestRefreshingTheLoginNeedsANewerSignIn(t *testing.T) {
	h := newHarness(t)
	h.setUp()
	_, tok := h.join("Alice", "alice-laptop")
	first := h.clock.Add(10 * 24 * time.Hour)

	post := func(ends time.Time) (int, map[string]any) {
		return h.do("POST", "/api/v1/accounts", map[string]string{"name": "Team", "email": "team@example.com", "credential": credentialEnding(ends)}, tok)
	}

	code, body := post(first)
	if code != 201 || body["loginEndsAt"] == nil {
		t.Fatalf("first add = %d %v; want 201 with the end date", code, body)
	}
	if got, _ := body["loginEndsAt"].(string); !strings.HasPrefix(got, first.UTC().Format("2006-01-02T15:04:05")) {
		t.Errorf("loginEndsAt = %q, want %v", got, first.UTC())
	}

	for name, ends := range map[string]time.Time{"the same sign-in": first, "an older copy": first.Add(-24 * time.Hour)} {
		code, body := post(ends)
		if code != 409 {
			t.Errorf("%s: status = %d %v, want 409", name, code, body)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, "same sign-in Team already has") || !strings.Contains(msg, "Sign in to the account again") {
			t.Errorf("%s: message = %q, want the reason and what to do", name, msg)
		}
	}

	newer := first.Add(28 * 24 * time.Hour)
	code, body = post(newer)
	if code != 200 || body["refreshed"] != true {
		t.Fatalf("newer sign-in = %d %v, want 200 refreshed", code, body)
	}
	_, pb := h.do("GET", "/api/panel", nil, "")
	accts, _ := pb["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("accounts = %v", pb["accounts"])
	}

	// A credential with no date cannot be compared, so it is let through.
	if code, body := post(time.Time{}); code != 200 {
		t.Errorf("an undated credential = %d %v, want it accepted", code, body)
	}
}

// Every machine is told when each of its accounts' sign-ins ends, in the windows
// that ride on its check-in, so the page can say so and the refresh dialog can
// compare it with the sign-in on the machine.
func TestMachinesAreToldWhenALoginEnds(t *testing.T) {
	s, secret := loginServer(t)
	ends := loginNow.Add(3 * 24 * time.Hour)
	s.usage = &fakeUsage{windows: []AccountWindow{{AccountID: "a1", SevenD: 0.2, UpdatedAt: loginNow}}}
	d := Data{
		Accounts: []Account{{ID: "a1", Name: "Saif", Email: "saif@devhouse.co", Credential: sealedLogin(t, secret, ends)}},
		Shares:   []Share{{AccountID: "a1", PersonID: "p"}},
	}

	got := s.checkinWindows(context.Background(), "p", d)
	if len(got) != 1 || !got[0].LoginEndsAt.Equal(ends) {
		t.Fatalf("windows = %+v, want one carrying the end %v", got, ends)
	}
}
