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

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil, nil) }

// newHarnessUsage is newHarness with a metering reader wired, for the board
// and quota tests; the plain newHarness leaves it nil, as a local file panel does.
func newHarnessUsage(t *testing.T, usage UsageReader) *harness { return newHarnessWith(t, usage, nil) }

// newHarnessWith is newHarness with a metering reader and a remote-jobs
// channel wired; the plain newHarness leaves both nil, as a local file panel does.
func newHarnessWith(t *testing.T, usage UsageReader, jobs Jobs) *harness {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "panel.json"))
	secret, err := LoadSecret(filepath.Join(dir, "panel.key"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: store, clock: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	store.now = func() time.Time { return h.clock }

	ps := NewServer(store, secret, usage, jobs)
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

// setUp makes the harness's panel usable: an admin, signed in.
func (h *harness) setUp() {
	h.t.Helper()
	if code, body := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		h.t.Fatalf("setup = %d %v", code, body)
	}
}

// join adds a person and enrols one machine of theirs from an invite, the way a
// real machine arrives; it returns the person's id and the machine's token.
func (h *harness) join(person, machine string) (personID, token string) {
	personID, token, _ = h.joinDevice(person, machine)
	return personID, token
}

// joinDevice is join, also returning the machine's device id.
func (h *harness) joinDevice(person, machine string) (personID, token, deviceID string) {
	h.t.Helper()
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": person}, ""); code != 201 {
		h.t.Fatalf("adding %s failed", person)
	}
	_, pb := h.do("GET", "/api/panel", nil, "")
	for _, p := range pb["people"].([]any) {
		if pm := p.(map[string]any); pm["name"] == person {
			personID = pm["id"].(string)
		}
	}
	_, inv := h.do("POST", "/api/people/"+personID+"/invite", nil, "")
	code, _ := inv["code"].(string)
	status, enrolled := h.do("POST", "/api/v1/enroll", map[string]string{"code": code, "machine": machine}, "")
	if status != 200 {
		h.t.Fatalf("enrolling %s = %d %v", machine, status, enrolled)
	}
	token, _ = enrolled["token"].(string)
	if token == "" {
		h.t.Fatal("enrolling returned no token")
	}
	deviceID, _ = enrolled["deviceId"].(string)
	return personID, token, deviceID
}

// fakeLogin is a credentials file as Claude Code writes it, base64 for the wire.
var fakeLogin = base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"fake"}}`))

// contribute hands a login up from a joined machine and returns the account's
// id — the only way an account arrives on the panel.
func (h *harness) contribute(token, name, email string) string {
	h.t.Helper()
	code, body := h.do("POST", "/api/v1/accounts", map[string]string{"name": name, "email": email, "credential": fakeLogin}, token)
	if code != 201 && code != 200 {
		h.t.Fatalf("handing up %s = %d %v", name, code, body)
	}
	id, _ := body["accountId"].(string)
	if id == "" {
		h.t.Fatalf("handing up %s returned no account id: %v", name, body)
	}
	return id
}

// The whole point, end to end: an account is lent to a person's machine, the
// machine is told about it, the account is taken back, and the next thing the
// machine hears is that it has nothing.
func TestAMachineIsToldWhatItCanUseAndWhenItStops(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	h.setUp()

	// An account with a login to share — handed up by the person who has it —
	// and someone else to share it with.
	_, ownerToken := h.join("Hassan", "hassan-mbp")
	accountID := h.contribute(ownerToken, "Work", "work@example.com")
	personID, token := h.join("Alice", "alice-mbp")

	// Nothing yet: joined is not the same as shared-with.
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
	if got["account"] != "Work" || got["gateway"] != "https://gw.example" || got["key"] == "" || got["accountId"] != accountID {
		t.Errorf("share handed to the machine = %v, want Work on the gateway with a key", got)
	}
	// The login itself must never travel to the machine — that is the gateway.
	if got["credential"] != nil {
		t.Error("the check-in handed the machine a credential; only the server holds the login")
	}
	// Alice did not add it, so her page must not offer to take it back.
	if got["contributed"] != nil {
		t.Errorf("Alice's share of an account she did not add is marked contributed: %v", got)
	}

	// Access taken away.
	_, pb2 := h.do("GET", "/api/panel", nil, "")
	var shareID string
	for _, sh := range pb2["accounts"].([]any)[0].(map[string]any)["shared"].([]any) {
		if sm := sh.(map[string]any); sm["personId"] == personID {
			shareID = sm["shareId"].(string)
		}
	}
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

// A login handed up from a joined machine belongs to the person it joined as:
// no password, no name to match. They can use it through the gateway at once,
// re-adding it refreshes the login without rotating their key or making a
// second account, only they (or the admin) can take it back, and taking the
// admin's revoke of their access leaves the account and the person alone.
func TestALoginHandedUpBelongsToWhoeverHandedItUp(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	h.setUp()
	hassanID, hassan := h.join("Hassan", "hassan-mbp")
	_, alice := h.join("Alice", "alice-mbp")

	// Nobody may add without being joined.
	if code, _ := h.do("POST", "/api/v1/accounts", map[string]string{"name": "Work", "credential": fakeLogin}, ""); code != 401 {
		t.Errorf("an unjoined machine handed up a login = %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/v1/accounts", map[string]string{"name": "Work", "credential": "not-base64!"}, hassan); code != 400 {
		t.Errorf("a broken credential = %d, want 400", code)
	}

	accountID := h.contribute(hassan, "Work", "Work@Example.com")

	account := func() map[string]any {
		t.Helper()
		_, pb := h.do("GET", "/api/panel", nil, "")
		accounts := pb["accounts"].([]any)
		if len(accounts) != 1 {
			t.Fatalf("the panel has %d accounts, want 1", len(accounts))
		}
		return accounts[0].(map[string]any)
	}
	a := account()
	if a["addedBy"] != "Hassan" || a["hasLogin"] != true || a["email"] != "Work@Example.com" {
		t.Errorf("the account as the panel shows it = %v", a)
	}
	shared, _ := a["shared"].([]any)
	if len(shared) != 1 || shared[0].(map[string]any)["personId"] != hassanID {
		t.Errorf("the contributor's own access = %v, want one share, Hassan's", shared)
	}

	// Their machine hears about it, marked as theirs to take back.
	_, ci := h.do("POST", "/api/v1/checkin", nil, hassan)
	list, _ := ci["gateway"].([]any)
	if len(list) != 1 {
		t.Fatalf("Hassan's machine can use %d accounts, want 1", len(list))
	}
	mine := list[0].(map[string]any)
	if mine["contributed"] != true || mine["accountId"] != accountID {
		t.Errorf("the contributor's share = %v, want contributed with the account id", mine)
	}
	key := mine["key"]

	// Re-adding — the same address in another spelling — refreshes, not duplicates.
	code, body := h.do("POST", "/api/v1/accounts", map[string]string{"name": "Work again", "email": "work@example.com", "credential": fakeLogin}, hassan)
	if code != 200 || body["refreshed"] != true || body["accountId"] != accountID {
		t.Errorf("re-adding = %d %v, want 200 refreshed with the same id", code, body)
	}
	if a := account(); a["name"] != "Work" {
		t.Errorf("re-adding renamed the account to %v", a["name"])
	}
	_, ci = h.do("POST", "/api/v1/checkin", nil, hassan)
	if k := ci["gateway"].([]any)[0].(map[string]any)["key"]; k != key {
		t.Error("re-adding rotated the contributor's key out from under their sessions")
	}

	// Alice cannot take it back; Hassan can, and with it goes everyone's access.
	if code, body := h.do("DELETE", "/api/v1/accounts/"+accountID, nil, alice); code != 403 || !strings.Contains(body["error"].(string), "Hassan") {
		t.Errorf("Alice taking back Hassan's login = %d %v, want 403 naming Hassan", code, body)
	}
	if code, _ := h.do("DELETE", "/api/v1/accounts/"+accountID, nil, ""); code != 401 {
		t.Errorf("an unjoined machine taking a login back = %d, want 401", code)
	}
	if code, _ := h.do("DELETE", "/api/v1/accounts/"+accountID, nil, hassan); code != 204 {
		t.Fatalf("Hassan taking his login back = %d, want 204", code)
	}
	_, pb := h.do("GET", "/api/panel", nil, "")
	if n := len(pb["accounts"].([]any)); n != 0 {
		t.Errorf("after taking it back the panel still has %d accounts", n)
	}
	if _, ci := h.do("POST", "/api/v1/checkin", nil, hassan); ci["gateway"] != nil {
		t.Errorf("after taking it back Hassan's machine can still use %v", ci["gateway"])
	}
	if code, _ := h.do("DELETE", "/api/v1/accounts/"+accountID, nil, hassan); code != 404 {
		t.Errorf("taking back an account that is gone = %d, want 404", code)
	}
	// The person stays: taking a login back is not leaving.
	if people := pb["people"].([]any); len(people) != 2 {
		t.Errorf("taking a login back changed the people: %v", people)
	}
}

// The admin's revoke of a contributor's access takes only the access: the
// escrowed login stays, the person stays, and their page simply stops offering
// the account. The admin removing the account removes it for everyone.
func TestTheAdminCanRevokeAContributorWithoutRemovingTheirLogin(t *testing.T) {
	t.Setenv("CLAWDH_GATEWAY_URL", "https://gw.example")
	h := newHarness(t)
	h.setUp()
	_, hassan := h.join("Hassan", "hassan-mbp")
	accountID := h.contribute(hassan, "Work", "work@example.com")

	_, pb := h.do("GET", "/api/panel", nil, "")
	shareID := pb["accounts"].([]any)[0].(map[string]any)["shared"].([]any)[0].(map[string]any)["shareId"].(string)
	if code, _ := h.do("POST", "/api/shares/"+shareID+"/revoke", nil, ""); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	_, pb = h.do("GET", "/api/panel", nil, "")
	acct := pb["accounts"].([]any)[0].(map[string]any)
	if s, _ := acct["shared"].([]any); len(s) != 0 {
		t.Errorf("after revoke the account still has shares: %v", s)
	}
	if acct["hasLogin"] != true || acct["addedBy"] != "Hassan" {
		t.Errorf("revoke changed the account itself: %v", acct)
	}
	if len(pb["people"].([]any)) != 1 {
		t.Error("revoke removed the person; it must remove only their gateway access")
	}
	if code, _ := h.do("DELETE", "/api/accounts/"+accountID, nil, ""); code != 204 {
		t.Errorf("the admin removing the account = %d, want 204", code)
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
	// It says who is inviting whom, and it has one thing to do per situation:
	// a button that opens the local clawdh page with the invite (clawdh
	// installed), or an install command that carries the invite (not yet).
	// The person is never asked to copy the link they just opened.
	html := string(page)
	for _, want := range []string{
		"Tester invited you to Claude", "as <b>Ehtisham</b>",
		`href="http://127.0.0.1:47932/?invite=` + neturl.QueryEscape(url) + `"`,
		"install.sh | sh -s -- --join &#34;" + url + "&#34;", // in the copy box, HTML-escaped
		`$env:CLAWDH_JOIN = "` + url + `"; irm`,              // the Windows tab's command, in the page script
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the invite page lacks %q", want)
		}
	}
	for _, gone := range []string{"Paste your link", "Prefer the terminal", `<code id="invite-link">`} {
		if strings.Contains(html, gone) {
			t.Errorf("the invite page still asks the person to handle the link again: %q", gone)
		}
	}

	// Once used, the same link says so instead of offering anything.
	if code, body := h.do("POST", "/api/v1/enroll", map[string]string{"code": body["code"].(string), "machine": "e-mbp"}, ""); code != 200 {
		t.Fatalf("joining from the invite = %d %v", code, body)
	}
	resp2, err := http.Get(h.srv.URL + "/i/" + body["code"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	used, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(used), "already been used") || strings.Contains(string(used), "Join on this computer") {
		t.Error("a used invite still offers to join")
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
	if code, _ := h.do("DELETE", "/api/accounts/sneaky", nil, ""); code != 401 {
		t.Errorf("removing an account without signing in = %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Sneaky"}, ""); code != 401 {
		t.Errorf("adding a person without signing in = %d, want 401", code)
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

	_, codeBody := h.do("POST", "/api/people/"+personID+"/invite", nil, "")
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
	h.setUp()
	_, hassan := h.join("Hassan", "hassan-mbp")
	acct := h.contribute(hassan, "Empty", "")
	person, _ := h.join("Alice", "alice-mbp")
	// An account from before logins arrived with their accounts: no credential.
	if err := h.store.Mutate(func(d *Data) error {
		a, _ := d.Account(acct)
		a.Credential = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}

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
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Bob"}, ""); code != 201 {
		t.Fatal("adding a person failed")
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
	for _, want := range []string{"Hassan set this panel up", "Hassan added Bob", "Ibrahim added Alice"} {
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
	_, codeBody := h.do("POST", "/api/people/"+personID+"/invite", nil, "")
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
