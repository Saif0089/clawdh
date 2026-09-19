package gateway

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// capRec captures the one metering Event the gateway records.
type capRec struct{ ch chan Event }

func (c *capRec) Record(e Event) {
	select {
	case c.ch <- e:
	default:
	}
}

// The gateway must meter a streamed (SSE) response: pull the model and input
// tokens from message_start and the final output tokens from message_delta,
// attributed to the member's account+person, without buffering the stream.
func TestGatewayMetersAStreamedResponse(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":5,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req_abc")
		io.WriteString(w, sse)
	}))
	defer anthropic.Close()

	rec := &capRec{ch: make(chan Event, 1)}
	h := New(fakeUpstream{key: "member-key", token: "T"}, rec, nil)
	srv := httptest.NewServer(rewriteHost(h, anthropic.Listener.Addr().String()))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) // drain so the metering body reaches EOF
	resp.Body.Close()

	select {
	case ev := <-rec.ch:
		if ev.Model != "claude-sonnet-4-6" {
			t.Errorf("model = %q", ev.Model)
		}
		if ev.Input != 100 || ev.Output != 42 || ev.CacheCreation != 10 || ev.CacheRead != 5 {
			t.Errorf("tokens = in %d out %d cc %d cr %d, want 100/42/10/5", ev.Input, ev.Output, ev.CacheCreation, ev.CacheRead)
		}
		if ev.AccountID != "acct-1" || ev.PersonID != "person-1" {
			t.Errorf("attribution = account %q person %q, want acct-1/person-1", ev.AccountID, ev.PersonID)
		}
		if ev.RequestID != "req_abc" {
			t.Errorf("request id = %q, want req_abc", ev.RequestID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway recorded no usage for a streamed response")
	}
}

// A real client (Claude Code) sends `Accept-Encoding: gzip, br`, so Anthropic
// answers with a compressed body. The gateway must still meter it: the token
// scanner reads the response body, and a body left compressed reads as zero
// usage — which is why usage_events stayed empty while the window headers
// recorded fine. The gateway drops the client's Accept-Encoding so the response
// reaches the meter (and the client) as plaintext; this proves a gzip'd upstream
// response is metered end to end. Without the fix the scanner sees gzip bytes,
// finds no message_start, and this test times out.
func TestGatewayMetersACompressedResponse(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":200,"cache_creation_input_tokens":20,"cache_read_input_tokens":8,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":77}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("request-id", "req_gz")
		gz := gzip.NewWriter(w)
		io.WriteString(gz, sse)
		gz.Close()
	}))
	defer anthropic.Close()

	rec := &capRec{ch: make(chan Event, 1)}
	h := New(fakeUpstream{key: "member-key", token: "T"}, rec, nil)
	srv := httptest.NewServer(rewriteHost(h, anthropic.Listener.Addr().String()))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer member-key")
	req.Header.Set("Accept-Encoding", "gzip, br") // what a real client sends
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The client got the decoded stream, not gzip bytes.
	if !strings.Contains(string(body), "message_start") {
		t.Errorf("client received a body that was not the decoded stream: %q", body)
	}

	select {
	case ev := <-rec.ch:
		if ev.Model != "claude-opus-5" {
			t.Errorf("model = %q, want claude-opus-5", ev.Model)
		}
		if ev.Input != 200 || ev.Output != 77 || ev.CacheCreation != 20 || ev.CacheRead != 8 {
			t.Errorf("tokens = in %d out %d cc %d cr %d, want 200/77/20/8", ev.Input, ev.Output, ev.CacheCreation, ev.CacheRead)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway metered nothing for a gzip-compressed response")
	}
}

// fixedLimiter reports the same quota standing for every member, to drive the
// deny and warn paths.
type fixedLimiter struct{ st QuotaStatus }

func (f fixedLimiter) Status(string, string) QuotaStatus { return f.st }

// parseWindows normalises a percentage reading to 0..1 and keeps an already
// fractional one, and reports absence when the headers aren't there.
func TestParseWindows(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-7d-utilization", "42")   // a percentage
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.8")  // already a fraction
	h.Set("anthropic-ratelimit-unified-7d-reset", "1800000000") // unix seconds
	w, has := parseWindows(h)
	if !has {
		t.Fatal("has = false, want true when utilisation headers are present")
	}
	if w.SevenD != 0.42 {
		t.Errorf("7d utilisation = %v, want 0.42 (42%% normalised)", w.SevenD)
	}
	if w.FiveH != 0.8 {
		t.Errorf("5h utilisation = %v, want 0.8 (kept as a fraction)", w.FiveH)
	}
	if w.SevenDReset.Unix() != 1800000000 {
		t.Errorf("7d reset = %v, want the unix time parsed", w.SevenDReset.Unix())
	}
	if _, has := parseWindows(http.Header{}); has {
		t.Error("no utilisation headers should report has = false")
	}
}

// winRec is a Recorder that also records windows, for the capture test.
type winRec struct{ ch chan Windows }

func (winRec) Record(Event) {}
func (w winRec) RecordWindows(_ string, win Windows) {
	select {
	case w.ch <- win:
	default:
	}
}

// The gateway reads the subscription's real window utilisation off Anthropic's
// headers and hands it to a WindowRecorder — the data behind "% of the weekly
// window" on the boards.
func TestGatewayCapturesWindowUtilization(t *testing.T) {
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "37")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer anthropic.Close()
	testTargetHost = strings.TrimPrefix(anthropic.URL, "http://")
	defer func() { testTargetHost = "" }()

	rec := winRec{ch: make(chan Windows, 1)}
	srv := httptest.NewServer(New(fakeUpstream{key: "member-key", token: "T"}, rec, nil))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	select {
	case w := <-rec.ch:
		if w.SevenD != 0.37 {
			t.Errorf("captured 7d utilisation = %v, want 0.37", w.SevenD)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the gateway did not capture the window utilisation")
	}
}

// An over-quota member is turned away with a 429 billing_error before the
// request ever reaches Anthropic — the shape a real spend limit uses.
func TestGatewayRejectsOverQuotaWith429(t *testing.T) {
	over := fixedLimiter{QuotaStatus{
		Over: true, Fraction: 1.0,
		ResetAt: time.Now().Add(time.Hour),
		Message: "your clawdh daily quota is reached; it resets soon.",
	}}
	h := New(fakeUpstream{key: "member-key", token: "T"}, nil, over)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("over quota -> %d, want 429", resp.StatusCode)
	}
	if ra, _ := strconv.Atoi(resp.Header.Get("retry-after")); ra < 3500 || ra > 3600 {
		t.Errorf("retry-after = %q, want ~3600 (until the window reset)", resp.Header.Get("retry-after"))
	}
	if resp.Header.Get("x-should-retry") != "false" {
		t.Error("a quota 429 must tell Claude Code not to retry it")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "billing_error") {
		t.Errorf("body = %s, want a billing_error", body)
	}
}

// A member who is forwarded but past 75% of their clawdh quota gets an
// allowed_warning on the way back — the signal Claude Code turns into an
// "approaching usage limit" notice — and the upstream's own rate-limit headers,
// which describe the shared login as a whole, are stripped in favour of it.
func TestGatewayWarnsApproachingQuota(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour)
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upstream reports its own (whole-login) utilization; clawdh replaces it.
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "12")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer anthropic.Close()
	testTargetHost = strings.TrimPrefix(anthropic.URL, "http://")
	defer func() { testTargetHost = "" }()

	warn := fixedLimiter{QuotaStatus{Fraction: 0.82, ResetAt: reset,
		Message: "your clawdh daily quota is reached; it resets soon."}}
	srv := httptest.NewServer(New(fakeUpstream{key: "member-key", token: "T"}, nil, warn))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("approaching-quota member -> %d, want 200 (still served)", resp.StatusCode)
	}
	if got := resp.Header.Get("anthropic-ratelimit-unified-status"); got != "allowed_warning" {
		t.Errorf("unified-status = %q, want allowed_warning at 82%%", got)
	}
	if got := resp.Header.Get("anthropic-ratelimit-unified-reset"); got != strconv.FormatInt(reset.Unix(), 10) {
		t.Errorf("unified-reset = %q, want the window reset %d", got, reset.Unix())
	}
	if resp.Header.Get("anthropic-ratelimit-unified-5h-utilization") != "" {
		t.Error("the upstream's own unified rate-limit headers must be stripped")
	}
}

// recLimiter records the (person, account) it was asked about, so a test can
// prove the gateway passes the account a request is using — the data an
// account-scoped quota is enforced from.
type recLimiter struct {
	person, account string
	st              QuotaStatus
}

func (r *recLimiter) Status(personID, accountID string) QuotaStatus {
	r.person, r.account = personID, accountID
	return r.st
}

// An account-scoped quota needs the gateway to tell the limiter which account a
// request is using, not just who is making it. This proves both identities reach
// the limiter, so a limit set on a subscription can actually be enforced.
func TestGatewayPassesAccountToTheLimiter(t *testing.T) {
	// Over the cap, so the request is denied here and never dials Anthropic; the
	// limiter still records the identity it was asked about first.
	lim := &recLimiter{st: QuotaStatus{Over: true, ResetAt: time.Now().Add(time.Hour), Message: "capped"}}
	h := New(fakeUpstream{key: "member-key", token: "T"}, nil, lim)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if lim.person != "person-1" || lim.account != "acct-1" {
		t.Errorf("limiter was asked about person %q account %q, want person-1/acct-1", lim.person, lim.account)
	}
}

type fakeUpstream struct{ key, token string }

func (f fakeUpstream) Resolve(k string) (Resolution, error) {
	if k == f.key {
		return Resolution{AccessToken: f.token, Label: "acct", AccountID: "acct-1", PersonID: "person-1"}, nil
	}
	return Resolution{}, ErrUnknownKey
}

// The gateway must swap a member's key for the real subscription token and add
// the headers a subscription request needs — without the member ever seeing the
// token. This stands a fake Anthropic up and checks what actually arrives.
func TestGatewaySwapsInTheSubscriptionToken(t *testing.T) {
	var gotAuth, gotBeta, gotVersion string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotVersion = r.Header.Get("anthropic-version")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer anthropic.Close()

	// Point the gateway's target at the fake by overriding the host it dials.
	h := New(fakeUpstream{key: "member-key", token: "REAL-SUB-TOKEN"}, nil, nil)
	srv := httptest.NewServer(rewriteHost(h, anthropic.Listener.Addr().String()))
	defer srv.Close()

	// A client in gateway mode presents its member key and its own betas.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer member-key")
	req.Header.Set("anthropic-beta", "tool-search-2025-10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer REAL-SUB-TOKEN" {
		t.Errorf("Anthropic saw Authorization %q, want the subscription token (member key must not leak)", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") || !strings.Contains(gotBeta, "tool-search-2025-10-19") {
		t.Errorf("anthropic-beta = %q, want the oauth beta added AND the client's beta kept", gotBeta)
	}
}

func TestGatewayRejectsUnknownKey(t *testing.T) {
	h := New(fakeUpstream{key: "good", token: "t"}, nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, key := range []string{"", "bad"} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 401 {
			t.Errorf("key %q -> %d, want 401", key, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// A share that is real but whose login can't produce a token is a 502, not a
// 401: the member did nothing wrong, so telling them their access was withdrawn
// would be a lie and a retry might succeed.
func TestGatewayReports502WhenTheLoginIsUnusable(t *testing.T) {
	h := New(brokenUpstream{}, nil, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("a valid key with an unusable login -> %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("x-should-retry") != "false" {
		t.Error("a definitive gateway error must tell Claude Code not to retry it")
	}
}

// brokenUpstream stands for a share whose subscription login cannot be refreshed
// right now — a known key, but no token.
type brokenUpstream struct{}

func (brokenUpstream) Resolve(string) (Resolution, error) {
	return Resolution{}, errors.New("refreshing the shared login: the token service answered 400")
}

// rewriteHost points the proxy's outbound host at the test's fake Anthropic,
// since New hardcodes the real host.
func rewriteHost(h http.Handler, host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testTargetHost = host
		h.ServeHTTP(w, r)
	})
}

// renewingUpstream resolves one key to an account whose token can be renewed
// once: "old" → "new". It records what Renew was told was bad.
type renewingUpstream struct {
	token   string
	renewed []string
	fail    bool
}

func (u *renewingUpstream) Resolve(k string) (Resolution, error) {
	if k != "member-key" {
		return Resolution{}, ErrUnknownKey
	}
	return Resolution{AccessToken: u.token, AccountID: "acct-1", PersonID: "p-1"}, nil
}

func (u *renewingUpstream) Renew(accountID, bad string) (string, error) {
	u.renewed = append(u.renewed, bad)
	if u.fail {
		return "", errors.New("refresh token is dead")
	}
	u.token = "new"
	return "new", nil
}

// Anthropic revokes a login's previous access token when its credential
// rotates, so a token the clock still calls valid can come back 401. The
// gateway renews it and replays the request — same body — once, and the
// member sees the 200, never the 401.
func TestGatewayRenewsARevokedTokenAndReplays(t *testing.T) {
	var bodies []string
	var auths []string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer new" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`)
			return
		}
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer anthropic.Close()

	up := &renewingUpstream{token: "old"}
	srv := httptest.NewServer(rewriteHost(New(up, nil, nil), anthropic.Listener.Addr().String()))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("member got %d %s, want the replayed 200", resp.StatusCode, body)
	}
	if len(auths) != 2 || auths[0] != "Bearer old" || auths[1] != "Bearer new" {
		t.Errorf("upstream saw %v, want old then new", auths)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Errorf("the replay must carry the same body: %q", bodies)
	}
	if len(up.renewed) != 1 || up.renewed[0] != "old" {
		t.Errorf("Renew was told bad=%v, want [old]", up.renewed)
	}
}

// When the login can't be renewed, the member gets clawdh's definitive 502 —
// what to do and no retry — never Anthropic's bare 401.
func TestGatewayTurnsAnUnrenewableTokenIntoACollision(t *testing.T) {
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`)
	}))
	defer anthropic.Close()

	up := &renewingUpstream{token: "old", fail: true}
	srv := httptest.NewServer(rewriteHost(New(up, nil, nil), anthropic.Listener.Addr().String()))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer member-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d %s, want 502", resp.StatusCode, body)
	}
	if resp.Header.Get("x-should-retry") != "false" {
		t.Error("a dead login must not be retried into")
	}
	if !strings.Contains(string(body), "add its login to the panel again") {
		t.Errorf("body = %s, want the owner-facing fix", body)
	}
}
