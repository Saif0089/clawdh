package panel

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"clawdh/internal/accounts"
)

// setupSharing gets a harness to the point where "Work" has a login and Alice
// has a machine enrolled, and returns the machine's config plus the ids. It
// does not share anything yet — each test decides that.
func setupSharing(t *testing.T, h *harness) (cfg ClientConfig, accountID, personID string) {
	t.Helper()
	h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, "")
	h.do("POST", "/api/accounts", map[string]string{"name": "Work"}, "")
	h.do("POST", "/api/people", map[string]string{"name": "Alice"}, "")

	_, panelBody := h.do("GET", "/api/panel", nil, "")
	accountID = panelBody["accounts"].([]any)[0].(map[string]any)["id"].(string)
	personID = panelBody["people"].([]any)[0].(map[string]any)["id"].(string)

	login := base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"lent"}}`))
	h.do("POST", "/api/accounts/"+accountID+"/login", map[string]string{"credential": login}, "")

	_, codeBody := h.do("POST", "/api/people/"+personID+"/code", nil, "")
	_, enrolled := h.do("POST", "/api/v1/enroll",
		map[string]any{"code": codeBody["code"], "machine": "alice-mbp"}, "")
	return ClientConfig{
		Server:   h.srv.URL,
		DeviceID: enrolled["deviceId"].(string),
		Token:    enrolled["token"].(string),
	}, accountID, personID
}

func newTestManager(t *testing.T) *accounts.Manager {
	t.Helper()
	dir := t.TempDir()
	return accounts.NewManager(
		accounts.NewStore(filepath.Join(dir, "accounts.json")),
		filepath.Join(dir, "accounts"))
}

// firstShareID reads the id of the single share on the first account.
func firstShareID(t *testing.T, h *harness) string {
	t.Helper()
	_, panelBody := h.do("GET", "/api/panel", nil, "")
	shared := panelBody["accounts"].([]any)[0].(map[string]any)["shared"].([]any)
	if len(shared) == 0 {
		t.Fatal("no share on the account")
	}
	return shared[0].(map[string]any)["shareId"].(string)
}

// A share reaches the machine as a cached gateway key — never a credential on
// disk — and disappears the moment it is revoked. That difference is the whole
// point of the gateway: the login stays on the server.
func TestCheckInCachesSharesAndDropsThemOnRevoke(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	cfg, accountID, personID := setupSharing(t, h)

	sharesPath := filepath.Join(t.TempDir(), "shares.json")
	mgr := newTestManager(t)
	// An account this machine's owner made themselves. The panel has no business
	// touching it, whatever happens.
	mine, err := mgr.Add("Personal")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Config: cfg, Accounts: mgr, SharesPath: sharesPath}

	// Nothing shared yet.
	if change, err := c.CheckIn(context.Background()); err != nil || !change.Empty() {
		t.Fatalf("first check-in: %+v %v", change, err)
	}

	h.do("POST", "/api/accounts/"+accountID+"/share", map[string]string{"personId": personID}, "")

	change, err := c.CheckIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Gained) != 1 || change.Gained[0] != "Work" {
		t.Fatalf("gained %v, want [Work]", change.Gained)
	}

	shares, err := LoadShares(sharesPath)
	if err != nil || len(shares) != 1 {
		t.Fatalf("cached shares = %+v %v, want one", shares, err)
	}
	if shares[0].Account != "Work" || shares[0].Gateway != "https://gw.example" || shares[0].Key == "" {
		t.Errorf("cached share = %+v, want Work on the gateway with a key", shares[0])
	}
	// The login never became a local account and never touched disk.
	list, _ := mgr.List()
	for _, a := range list {
		if a.PanelID != "" {
			t.Error("a gateway share should not create a local account")
		}
	}

	// Take the access away.
	h.do("POST", "/api/shares/"+firstShareID(t, h)+"/revoke", nil, "")

	change, err = c.CheckIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Lost) != 1 || change.Lost[0] != "Work" {
		t.Fatalf("lost %v, want [Work]", change.Lost)
	}
	if shares, _ := LoadShares(sharesPath); len(shares) != 0 {
		t.Errorf("shares still cached after revoke: %+v", shares)
	}

	// The account the user made is untouched throughout.
	if _, err := mgr.Get(mine.ID); err != nil {
		t.Errorf("the panel removed an account it was never lent: %v", err)
	}
}

// A share that is revoked and granted again between two check-ins keeps its
// slug and gets a new key. The cache has to take the new key — a running
// session watches the file for exactly that — and it has to carry the login's
// email, which is how a machine whose local copy of the same login has died
// finds the share that now runs it.
func TestCheckInRewritesARekeyedShareAndCarriesTheEmail(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	cfg, accountID, personID := setupSharing(t, h)
	h.do("POST", "/api/accounts/"+accountID+"/login", map[string]string{
		"credential": base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"lent"}}`)),
		"email":      "work@example.com",
	}, "")
	sharesPath := filepath.Join(t.TempDir(), "shares.json")
	c := &Client{Config: cfg, Accounts: newTestManager(t), SharesPath: sharesPath}

	h.do("POST", "/api/accounts/"+accountID+"/share", map[string]string{"personId": personID}, "")
	if _, err := c.CheckIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := LoadShares(sharesPath)
	if len(before) != 1 || before[0].Email != "work@example.com" {
		t.Fatalf("cached share = %+v, want one carrying work@example.com", before)
	}

	// Revoke and re-grant with no check-in in between: same slug, new key.
	h.do("POST", "/api/shares/"+firstShareID(t, h)+"/revoke", nil, "")
	h.do("POST", "/api/accounts/"+accountID+"/share", map[string]string{"personId": personID}, "")

	change, err := c.CheckIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !change.Empty() {
		t.Errorf("a re-key is not a gain or a loss, got %+v", change)
	}
	after, _ := LoadShares(sharesPath)
	if len(after) != 1 || after[0].Slug != before[0].Slug {
		t.Fatalf("cached shares after re-key = %+v, want the same one", after)
	}
	if after[0].Key == before[0].Key {
		t.Error("the cache still holds the revoked key")
	}
}

// A machine that has been cut off forgets every shared account, without needing
// to be told account by account.
func TestACutOffMachineForgetsEverything(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	cfg, accountID, personID := setupSharing(t, h)
	sharesPath := filepath.Join(t.TempDir(), "shares.json")
	mgr := newTestManager(t)
	c := &Client{Config: cfg, Accounts: mgr, SharesPath: sharesPath}

	h.do("POST", "/api/accounts/"+accountID+"/share", map[string]string{"personId": personID}, "")
	if _, err := c.CheckIn(context.Background()); err != nil {
		t.Fatal(err)
	}

	if code, _ := h.do("DELETE", "/api/devices/"+cfg.DeviceID, nil, ""); code != 204 {
		t.Fatal("cutting the machine off failed")
	}

	change, err := c.CheckIn(context.Background())
	if err != ErrNotEnrolled {
		t.Fatalf("check-in after being cut off = %v, want ErrNotEnrolled", err)
	}
	if len(change.Lost) != 1 {
		t.Errorf("forgot %v, want the one account it could use", change.Lost)
	}
	if shares, _ := LoadShares(sharesPath); len(shares) != 0 {
		t.Errorf("a cut-off machine still has cached shares: %+v", shares)
	}
}

// clawdh without a panel is clawdh as it always was.
func TestAMachineWithNoPanelDoesNothing(t *testing.T) {
	mgr := newTestManager(t)
	if _, err := mgr.Add("Personal"); err != nil {
		t.Fatal(err)
	}
	c := &Client{Accounts: mgr}
	change, err := c.CheckIn(context.Background())
	if err != nil || !change.Empty() {
		t.Fatalf("check-in with no panel configured = %+v %v, want a no-op", change, err)
	}
	if list, _ := mgr.List(); len(list) != 1 {
		t.Error("an unconfigured check-in changed the account list")
	}
}

// The gateway's usage reading for each shared account rides back on the
// check-in and reaches the caller — it is what draws the bars on a machine's
// page for a login the gateway holds. (It was parsed and then dropped once.)
func TestCheckInCarriesTheWindowsThrough(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"gateway":[],"jobs":null,"notices":null,"windows":[{"slug":"ehtisham","email":"ehtisham@devhouse.co","fiveH":0.12,"sevenD":0.4,"updatedAt":"2026-09-19T20:06:06Z"}]}`)
	}))
	defer ts.Close()
	c := &Client{Config: ClientConfig{Server: ts.URL, Token: "t", DeviceID: "d"}, Accounts: newTestManager(t), SharesPath: filepath.Join(t.TempDir(), "shares.json")}
	change, err := c.CheckIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Windows) != 1 || change.Windows[0].Email != "ehtisham@devhouse.co" || change.Windows[0].SevenD != 0.4 {
		t.Fatalf("windows = %+v, want the one the panel sent", change.Windows)
	}
}
