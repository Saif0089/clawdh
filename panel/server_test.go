package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	store  *Store
	client *http.Client
	clock  time.Time
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// newHarnessWith is newHarness with a remote-jobs channel wired, for the jobs
// tests; the plain newHarness leaves it nil, as a local file panel does.
func newHarnessWith(t *testing.T, jobs Jobs) *harness {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "panel.json"))
	secret, err := LoadSecret(filepath.Join(dir, "panel.key"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: store, clock: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	store.now = func() time.Time { return h.clock }

	ps := NewServer(store, secret, nil, jobs)
	ps.now = func() time.Time { return h.clock }
	h.srv = httptest.NewServer(ps.Handler())
	t.Cleanup(h.srv.Close)

	jar := &cookieJar{}
	h.client = &http.Client{Jar: jar}
	return h
}

// do sends a request and returns status plus the decoded body.
func (h *harness) do(method, path string, body any, bearer string) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, h.srv.URL+path, &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The whole point, end to end: an account is lent to a person's machine, the
// machine is told about it, the account is taken back, and the next thing the
// machine hears is that it has nothing.
func TestAMachineIsToldWhatItCanUseAndWhenItStops(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)

	if code, body := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Fatalf("setup = %d %v", code, body)
	}

	// An account with a login to share, and someone to share it with.
	if code, _ := h.do("POST", "/api/accounts", map[string]string{"name": "Work"}, ""); code != 201 {
		t.Fatalf("adding an account = %d", code)
	}
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Alice"}, ""); code != 201 {
		t.Fatalf("adding a person = %d", code)
	}
	_, panelBody := h.do("GET", "/api/panel", nil, "")
	accountID := panelBody["accounts"].([]any)[0].(map[string]any)["id"].(string)
	personID := panelBody["people"].([]any)[0].(map[string]any)["id"].(string)

	login := []byte(`{"claudeAiOauth":{"accessToken":"fake"}}`)
	if code, body := h.do("POST", "/api/accounts/"+accountID+"/login",
		map[string]string{"credential": base64.StdEncoding.EncodeToString(login)}, ""); code != 200 {
		t.Fatalf("storing the login = %d %v", code, body)
	}

	// Alice enrols a machine with a one-shot code.
	_, codeBody := h.do("POST", "/api/people/"+personID+"/code", nil, "")
	joinCode, _ := codeBody["code"].(string)
	status, enrolled := h.do("POST", "/api/v1/enroll",
		map[string]string{"code": joinCode, "machine": "alice-mbp"}, "")
	if status != 200 {
		t.Fatalf("enrolling = %d %v", status, enrolled)
	}
	token, _ := enrolled["token"].(string)
	if token == "" {
		t.Fatal("enrolling returned no token")
	}

	// Nothing yet: enrolled is not the same as shared-with.
	if _, body := h.do("POST", "/api/v1/checkin", nil, token); body["gateway"] != nil {
		t.Errorf("a machine with no share was told it can use %v", body["gateway"])
	}

	if code, body := h.do("POST", "/api/accounts/"+accountID+"/share",
		map[string]string{"personId": personID}, ""); code != 200 {
		t.Fatalf("sharing = %d %v", code, body)
	}

	_, body := h.do("POST", "/api/v1/checkin", nil, token)
	list, _ := body["gateway"].([]any)
	if len(list) != 1 {
		t.Fatalf("the machine was told it can use %d accounts, want 1", len(list))
	}
	got := list[0].(map[string]any)
	if got["account"] != "Work" || got["gateway"] != "https://gw.example" || got["key"] == "" {
		t.Errorf("share handed to the machine = %v, want Work on the gateway with a key", got)
	}
	// The login itself must never travel to the machine — that is the gateway.
	if got["credential"] != nil {
		t.Error("the check-in handed the machine a credential; only the server holds the login")
	}

	// Access taken away.
	_, pb2 := h.do("GET", "/api/panel", nil, "")
	shareID := pb2["accounts"].([]any)[0].(map[string]any)["shared"].([]any)[0].(map[string]any)["shareId"].(string)
	if code, _ := h.do("POST", "/api/shares/"+shareID+"/revoke", nil, ""); code != 200 {
		t.Fatalf("taking access away = %d", code)
	}

	// The next check-in is the machine finding out. A complete list, so an
	// empty one means "let go of everything".
	_, body = h.do("POST", "/api/v1/checkin", nil, token)
	if list, _ := body["gateway"].([]any); len(list) != 0 {
		t.Errorf("after access was taken away the machine can still use %v", list)
	}
}

// The machine that pushes a login is recorded as a member with its own gateway
// access, so it shows in the panel and can be cut off there. Re-pushing does not
// duplicate the record, and taking the access away removes only the share — the
// person stays, the escrowed login stays, and (structurally, since the panel
// never reaches a client's account store) the login on that machine is untouched.
func TestPushingALoginRecordsThePusherAsAMember(t *testing.T) {
	h := newHarness(t)

	if code, _ := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Fatal("setup failed")
	}
	if code, _ := h.do("POST", "/api/accounts", map[string]string{"name": "Work"}, ""); code != 201 {
		t.Fatalf("adding an account = %d", code)
	}
	_, pb := h.do("GET", "/api/panel", nil, "")
	accountID := pb["accounts"].([]any)[0].(map[string]any)["id"].(string)

	login := base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"fake"}}`))
	push := func() int {
		code, _ := h.do("POST", "/api/accounts/"+accountID+"/login",
			map[string]string{"credential": login, "pusher": "hassan-mbp"}, "")
		return code
	}
	if code := push(); code != 200 {
		t.Fatalf("pushing the login = %d", code)
	}

	// sharesAndPusher reads the one account's shares plus the pusher's person row.
	sharesAndPusher := func() ([]any, map[string]any) {
		_, pb := h.do("GET", "/api/panel", nil, "")
		shared, _ := pb["accounts"].([]any)[0].(map[string]any)["shared"].([]any)
		var person map[string]any
		for _, p := range pb["people"].([]any) {
			if pm := p.(map[string]any); pm["name"] == "hassan-mbp" {
				person = pm
			}
		}
		return shared, person
	}

	shared, person := sharesAndPusher()
	if len(shared) != 1 {
		t.Fatalf("the pusher got %d shares, want 1", len(shared))
	}
	if person == nil {
		t.Fatal("the pusher was not recorded as a member")
	}
	if shared[0].(map[string]any)["personName"] != "hassan-mbp" {
		t.Errorf("the account's share is not the pusher's: %v", shared[0])
	}
	if can, _ := person["can"].([]any); len(can) != 1 || can[0] != "Work" {
		t.Errorf("the pusher cannot use the account they added: %v", person["can"])
	}

	// Re-pushing the same login keeps one member record, not two.
	if code := push(); code != 200 {
		t.Fatalf("re-push = %d", code)
	}
	shared, _ = sharesAndPusher()
	if len(shared) != 1 {
		t.Fatalf("re-pushing duplicated the member record: %d shares", len(shared))
	}

	// Take the access away.
	shareID := shared[0].(map[string]any)["shareId"].(string)
	if code, _ := h.do("POST", "/api/shares/"+shareID+"/revoke", nil, ""); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	_, pb2 := h.do("GET", "/api/panel", nil, "")
	acct := pb2["accounts"].([]any)[0].(map[string]any)
	if s, _ := acct["shared"].([]any); len(s) != 0 {
		t.Errorf("after revoke the account still has shares: %v", s)
	}
	if acct["hasLogin"] != true {
		t.Error("revoke removed the escrowed login; it must remove only gateway access")
	}
	if _, person := sharesAndPusher(); person == nil {
		t.Error("revoke removed the person; it must remove only their gateway access")
	}
}

// An invite is a single link that carries a join code and lands on a public
// page explaining what to do with it — the no-terminal way to set someone up.
func TestInviteLinkOpensAWelcomePage(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, "")
	h.do("POST", "/api/people", map[string]string{"name": "Ehtisham"}, "")
	_, pb := h.do("GET", "/api/panel", nil, "")
	personID := pb["people"].([]any)[0].(map[string]any)["id"].(string)

	code, body := h.do("POST", "/api/people/"+personID+"/invite", nil, "")
	if code != 200 {
		t.Fatalf("making an invite = %d %v", code, body)
	}
	url, _ := body["url"].(string)
	if url == "" || !strings.Contains(url, "/i/") {
		t.Fatalf("invite url = %q, want one containing /i/", url)
	}
	if body["expiresAt"] == nil {
		t.Error("an invite must say when it expires")
	}

	// The link is public — anyone the admin sends it to can open it without a
	// session — and it explains itself.
	resp, err := http.Get(h.srv.URL + "/i/" + body["code"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("opening the invite = %d", resp.StatusCode)
	}
	if !strings.Contains(string(page), "invited") {
		t.Error("the invite page does not welcome the person")
	}
}

func TestThePanelIsClosedToStrangers(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Fatal("setup failed")
	}
	// A fresh client: signed in nowhere.
	h.client = &http.Client{Jar: &cookieJar{}}

	for _, path := range []string{"/api/panel"} {
		if code, _ := h.do("GET", path, nil, ""); code != 401 {
			t.Errorf("GET %s without signing in = %d, want 401", path, code)
		}
	}
	if code, _ := h.do("POST", "/api/accounts", map[string]string{"name": "Sneaky"}, ""); code != 401 {
		t.Errorf("adding an account without signing in = %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/login", map[string]string{"password": "wrong", "name": "Tester"}, ""); code != 401 {
		t.Errorf("signing in with the wrong password = %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/login", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Errorf("signing in with the right password = %d, want 200", code)
	}
	if code, _ := h.do("GET", "/api/panel", nil, ""); code != 200 {
		t.Errorf("reading the panel after signing in = %d, want 200", code)
	}
}

// Cutting off a machine has to be immediate and total, whatever it still holds.
func TestACutOffMachineIsTurnedAway(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, "")
	h.do("POST", "/api/people", map[string]string{"name": "Bob"}, "")
	_, panelBody := h.do("GET", "/api/panel", nil, "")
	personID := panelBody["people"].([]any)[0].(map[string]any)["id"].(string)

	_, codeBody := h.do("POST", "/api/people/"+personID+"/code", nil, "")
	_, enrolled := h.do("POST", "/api/v1/enroll",
		map[string]any{"code": codeBody["code"], "machine": "bob-pc"}, "")
	token := enrolled["token"].(string)
	deviceID := enrolled["deviceId"].(string)

	if code, _ := h.do("POST", "/api/v1/checkin", nil, token); code != 200 {
		t.Fatal("an enrolled machine could not check in")
	}
	if code, _ := h.do("DELETE", "/api/devices/"+deviceID, nil, ""); code != 204 {
		t.Fatal("cutting the machine off failed")
	}
	if code, body := h.do("POST", "/api/v1/checkin", nil, token); code != 401 {
		t.Errorf("a cut-off machine checked in: %d %v", code, body)
	}
}

// A minimal cookie jar: net/http/cookiejar needs a public-suffix list to accept
// cookies for "127.0.0.1", and this only has to hold one.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *neturl.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *cookieJar) Cookies(_ *neturl.URL) []*http.Cookie             { return j.cookies }

// An account with no login is nothing to share; sharing one is the confusing
// state that would hand a member an empty account.
func TestCannotShareAnAccountWithNoLogin(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, "")
	h.do("POST", "/api/accounts", map[string]string{"name": "Empty"}, "")
	h.do("POST", "/api/people", map[string]string{"name": "Alice"}, "")
	_, pb := h.do("GET", "/api/panel", nil, "")
	acct := pb["accounts"].([]any)[0].(map[string]any)["id"].(string)
	person := pb["people"].([]any)[0].(map[string]any)["id"].(string)

	code, body := h.do("POST", "/api/accounts/"+acct+"/share", map[string]string{"personId": person}, "")
	if code != 400 {
		t.Fatalf("sharing a login-less account returned %d, want 400", code)
	}
	if body["error"] == nil {
		t.Error("expected an explanation of why it was refused")
	}
}

// The panel is flat — one shared password — but every change is recorded under
// the name the signer gave, so two team leads on the same password are told
// apart in the log. A sign-in without a name is refused, the name rides in the
// sealed cookie, and a second signer's changes carry their own name.
func TestChangesAreRecordedUnderTheSignersName(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Hassan"}, ""); code != 200 {
		t.Fatalf("setup = %d %v", code, body)
	}
	if code, _ := h.do("POST", "/api/accounts", map[string]string{"name": "Work"}, ""); code != 201 {
		t.Fatal("adding an account failed")
	}
	_, st := h.do("GET", "/api/status", nil, "")
	if st["actor"] != "Hassan" {
		t.Errorf("status actor = %v, want Hassan (the sealed session name)", st["actor"])
	}

	// A second team lead signs in on the same password under their own name.
	h.do("POST", "/api/logout", nil, "")
	if code, body := h.do("POST", "/api/login", map[string]string{"password": "a-long-enough-one"}, ""); code != 400 {
		t.Errorf("a nameless sign-in = %d %v, want 400 (say who you are)", code, body)
	}
	if code, _ := h.do("POST", "/api/login", map[string]string{"password": "a-long-enough-one", "name": "  Ibrahim  "}, ""); code != 200 {
		t.Fatal("named sign-in failed")
	}
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Alice"}, ""); code != 201 {
		t.Fatal("adding a person failed")
	}

	_, panelBody := h.do("GET", "/api/panel", nil, "")
	var whos []string
	for _, e := range panelBody["activity"].([]any) {
		ev := e.(map[string]any)
		whos = append(whos, ev["who"].(string)+" "+ev["what"].(string))
	}
	joined := strings.Join(whos, " | ")
	for _, want := range []string{"Hassan set this panel up", "Hassan added Work", "Ibrahim added Alice"} {
		if !strings.Contains(joined, want) {
			t.Errorf("activity lacks %q; got: %s", want, joined)
		}
	}
	if strings.Contains(joined, "You ") {
		t.Errorf("activity still attributes a change to \"You\": %s", joined)
	}
}

// The People tab names the clawdh build on each machine: the one it enrolled
// with until it checks in, then whichever it last reported. A check-in that
// says nothing about its build — one from a clawdh older than the reporting —
// keeps whatever the panel knew rather than blanking it. The panel names its
// own build on /api/status the way the local page's corner does.
func TestAMachineReportsItsBuildAndThePanelItsOwn(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Fatalf("setup = %d %v", code, body)
	}
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Alice"}, ""); code != 201 {
		t.Fatal("adding a person failed")
	}
	_, panelBody := h.do("GET", "/api/panel", nil, "")
	personID := panelBody["people"].([]any)[0].(map[string]any)["id"].(string)
	_, codeBody := h.do("POST", "/api/people/"+personID+"/code", nil, "")
	joinCode, _ := codeBody["code"].(string)

	status, enrolled := h.do("POST", "/api/v1/enroll",
		map[string]string{"code": joinCode, "machine": "alice-mbp", "version": " v1.4.0 · 7b506ea "}, "")
	if status != 200 {
		t.Fatalf("enrolling = %d %v", status, enrolled)
	}
	token, _ := enrolled["token"].(string)

	machine := func() map[string]any {
		t.Helper()
		_, body := h.do("GET", "/api/panel", nil, "")
		devices := body["people"].([]any)[0].(map[string]any)["devices"].([]any)
		if len(devices) != 1 {
			t.Fatalf("Alice has %d machines, want 1", len(devices))
		}
		return devices[0].(map[string]any)
	}
	if got := machine()["version"]; got != "v1.4.0 · 7b506ea" {
		t.Errorf("build after enrolling = %v, want the one it enrolled with, trimmed", got)
	}

	// The machine updates and checks in as the new build.
	if code, _ := h.do("POST", "/api/v1/checkin", map[string]string{"version": "main · d005986"}, token); code != 200 {
		t.Fatal("check-in failed")
	}
	if got := machine()["version"]; got != "main · d005986" {
		t.Errorf("build after a check-in = %v, want the one it reported", got)
	}

	// A check-in from before build reporting says nothing, and changes nothing.
	h.do("POST", "/api/v1/checkin", map[string]bool{"remote": false}, token)
	if got := machine()["version"]; got != "main · d005986" {
		t.Errorf("a check-in without a build changed it to %v", got)
	}

	// A test binary is stamped with nothing, so the panel's own tag falls back to
	// what Vercel sets for the deploy it built, and to "dev" with neither.
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
	t.Setenv("VERCEL_GIT_COMMIT_REF", "")
	if _, body := h.do("GET", "/api/status", nil, ""); body["tag"] != "dev" {
		t.Errorf("tag with nothing to name the build = %v, want dev", body["tag"])
	}
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "0f81cd2d3ad5b0f1e6d2c7a8b9e0f1a2b3c4d5e6")
	t.Setenv("VERCEL_GIT_COMMIT_REF", "main")
	if _, body := h.do("GET", "/api/status", nil, ""); body["tag"] != "main · 0f81cd2" {
		t.Errorf("tag on Vercel = %v, want the branch and short commit it deployed", body["tag"])
	}
}
