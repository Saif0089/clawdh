package panel

import (
	"context"
	"encoding/base64"
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
