package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The refresh request must carry the grant type, the single-use refresh token,
// and Claude Code's client id — checked against a stand-in token service so no
// real login is rotated.
func TestRefreshRequestShapeAndRotation(t *testing.T) {
	var got map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
		})
	}))
	defer ts.Close()

	// Point refresh at the fake by swapping the package endpoint for the test.
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	cred, err := refresh(context.Background(), ts.Client(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if got["grant_type"] != "refresh_token" || got["refresh_token"] != "old-refresh" || got["client_id"] != claudeCodeClientID {
		t.Errorf("refresh sent %v", got)
	}
	if cred.AccessToken != "new-access" || cred.RefreshToken != "new-refresh" {
		t.Errorf("rotated credential = %+v", cred)
	}
	if cred.ExpiresAt.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("expiry not set from expires_in: %v", cred.ExpiresAt)
	}
}

// The manager refreshes a stale token and hands back the fresh one, persisting it.
func TestTokenManagerRefreshesWhenStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "r2", "expires_in": 3600})
	}))
	defer ts.Close()
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	var persisted Credential
	m := newTokenManager(Credential{AccessToken: "expired", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Minute)},
		func(c Credential) { persisted = c })
	m.httpc = ts.Client()
	tok, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "fresh" {
		t.Errorf("got %q, want the refreshed token", tok)
	}
	if persisted.RefreshToken != "r2" {
		t.Error("the rotated credential was not persisted")
	}
}

// A credential rotated by another process (diagnose, a re-added login) is
// adopted from the store instead of refreshing with our now-spent token.
func TestTokenManagerAdoptsAReloadedCredentialBeforeRefreshing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("refresh was called although the store held a fresh credential")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	m := newTokenManager(Credential{AccessToken: "expired", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Minute)}, nil)
	m.httpc = ts.Client()
	m.Reload(func() (Credential, bool) {
		return Credential{AccessToken: "rotated-elsewhere", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)}, true
	})
	tok, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "rotated-elsewhere" {
		t.Errorf("got %q, want the credential the store now holds", tok)
	}
}

// A store that missed rotations — the database was unreachable while the
// gateway kept refreshing in memory — holds an earlier credential whose refresh
// token is already spent. The manager must keep rolling its own forward, not
// swap to that one: on 2026-09-28 the running gateway would have done exactly
// that to every shared login the moment the database came back.
func TestTokenManagerNeverAdoptsAnOlderStoredCredential(t *testing.T) {
	var usedRefresh string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]string
		json.NewDecoder(r.Body).Decode(&got)
		usedRefresh = got["refresh_token"]
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "r-next", "expires_in": 28800})
	}))
	defer ts.Close()
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	var persisted Credential
	// Due for a refresh (inside refreshLead) but issued long after the stored one.
	m := newTokenManager(Credential{AccessToken: "current", RefreshToken: "r-current", ExpiresAt: time.Now().Add(5 * time.Minute)},
		func(c Credential) { persisted = c })
	m.httpc = ts.Client()
	m.Reload(func() (Credential, bool) {
		return Credential{AccessToken: "before-outage", RefreshToken: "r-spent", ExpiresAt: time.Now().Add(-3 * time.Hour)}, true
	})

	tok, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usedRefresh != "r-current" {
		t.Errorf("refreshed with %q; the store's older credential must not replace the one in memory", usedRefresh)
	}
	if tok != "fresh" || persisted.RefreshToken != "r-next" {
		t.Errorf("got token %q, persisted %+v; want the rotation of our own credential", tok, persisted)
	}
}

// The same rule on the 401 path: a refused token is renewed from our own
// refresh token when the store only has something older.
func TestRenewNeverAdoptsAnOlderStoredCredential(t *testing.T) {
	var usedRefresh string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]string
		json.NewDecoder(r.Body).Decode(&got)
		usedRefresh = got["refresh_token"]
		json.NewEncoder(w).Encode(map[string]any{"access_token": "renewed", "refresh_token": "r-next", "expires_in": 28800})
	}))
	defer ts.Close()
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	m := newTokenManager(Credential{AccessToken: "refused", RefreshToken: "r-current", ExpiresAt: time.Now().Add(2 * time.Hour)}, nil)
	m.httpc = ts.Client()
	m.Reload(func() (Credential, bool) {
		return Credential{AccessToken: "older", RefreshToken: "r-spent", ExpiresAt: time.Now().Add(-time.Hour)}, true
	})

	tok, err := m.Renew(context.Background(), "refused")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "renewed" || usedRefresh != "r-current" {
		t.Errorf("Renew returned %q after refreshing with %q; want our own credential rolled forward", tok, usedRefresh)
	}
}

// And the legitimate case still holds on the 401 path: a credential rotated
// elsewhere after ours was issued is adopted without spending our refresh token.
func TestRenewAdoptsANewerStoredCredential(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("refresh was called although the store held a newer credential")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()
	old := testTokenEndpoint
	testTokenEndpoint = ts.URL
	defer func() { testTokenEndpoint = old }()

	m := newTokenManager(Credential{AccessToken: "refused", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}, nil)
	m.httpc = ts.Client()
	m.Reload(func() (Credential, bool) {
		return Credential{AccessToken: "rotated-elsewhere", RefreshToken: "r2", ExpiresAt: time.Now().Add(8 * time.Hour)}, true
	})
	tok, err := m.Renew(context.Background(), "refused")
	if err != nil || tok != "rotated-elsewhere" {
		t.Errorf("Renew = %q, %v; want the newer stored credential", tok, err)
	}
}
