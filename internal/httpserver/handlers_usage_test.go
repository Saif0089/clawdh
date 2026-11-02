package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"clawdh/internal/usage"
)

const usagePayload = `{"limits":[
	{"kind":"session","group":"session","percent":5,"severity":"normal","resets_at":"2026-09-07T05:29:59Z","is_active":true},
	{"kind":"weekly_all","group":"weekly","percent":59,"severity":"normal","resets_at":"2026-09-08T13:59:59Z","is_active":true}
],"extra_usage":{"is_enabled":false}}`

func TestAccountUsage(t *testing.T) {
	t.Setenv("CLAWDH_CREDENTIALS_FILE", "1") // the file written below, never the Keychain
	srv, _ := newTestServer(t)

	account, err := srv.manager.Add("Work")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Relative clocks: a fixed expiry in a fixture silently turns the
	// account "expired" months later and fails the test for the wrong
	// reason.
	creds := fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"max","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		time.Now().Add(8*time.Hour).UnixMilli(), time.Now().Add(30*24*time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(account.ConfigDir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatalf("writing credentials: %v", err)
	}

	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(usagePayload))
	}))
	defer api.Close()
	srv.usage = usage.NewServiceWithClient(&usage.Client{Endpoint: api.URL, HTTPClient: api.Client()})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var snapshot usage.Snapshot
	get := func(path string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d", path, resp.StatusCode)
		}
		snapshot = usage.Snapshot{}
		if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}

	get("/api/accounts/" + account.ID + "/usage")
	if snapshot.Usage == nil {
		t.Fatalf("no usage returned: %s", snapshot.Error)
	}
	if len(snapshot.Usage.Limits) != 1 {
		t.Fatalf("got %d limits, want 1: the weekly one, the leftover session entry dropped", len(snapshot.Usage.Limits))
	}
	if snapshot.Usage.Limits[0].Label != "This week, all models" {
		t.Errorf("first label = %q", snapshot.Usage.Limits[0].Label)
	}
	if snapshot.Session == nil || snapshot.Session.SessionExpiresAt == nil {
		t.Fatal("want the login's own expiry alongside the plan usage")
	}

	// Tokens must never reach the browser. This is the whole reason the
	// server reads credentials instead of the page doing it.
	raw, _ := json.Marshal(snapshot)
	if bytesContain(raw, "tok") {
		t.Errorf("response leaked the access token: %s", raw)
	}

	// A plain reload is served from cache, and so is a leftover ?refresh=1:
	// a GET must never be a way to make clawdh ask Anthropic sooner.
	get("/api/accounts/" + account.ID + "/usage")
	get("/api/accounts/" + account.ID + "/usage?refresh=1")
	if calls != 1 {
		t.Errorf("calls = %d, want every repeat read served from cache", calls)
	}

	// The expire route answers without reading anything, and leaves numbers
	// read a moment ago alone: switching accounts can send several in a row.
	resp, err := http.Post(ts.URL+"/api/accounts/"+account.ID+"/usage/expire", "", nil)
	if err != nil {
		t.Fatalf("POST usage/expire: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST usage/expire: status = %d, want 204", resp.StatusCode)
	}
	get("/api/accounts/" + account.ID + "/usage")
	if calls != 1 {
		t.Errorf("calls = %d, want numbers read a moment ago kept after an expire", calls)
	}
}

func TestAccountUsageUnknownAccount(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/accounts/nope/usage")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	expired, err := http.Post(ts.URL+"/api/accounts/nope/usage/expire", "", nil)
	if err != nil {
		t.Fatalf("POST usage/expire: %v", err)
	}
	defer expired.Body.Close()
	if expired.StatusCode != http.StatusNotFound {
		t.Errorf("POST usage/expire: status = %d, want 404", expired.StatusCode)
	}
}

func bytesContain(haystack []byte, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if string(haystack[i:i+len(needle)]) == needle {
					return true
				}
			}
			return false
		}()
}
