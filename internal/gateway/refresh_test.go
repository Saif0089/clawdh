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
