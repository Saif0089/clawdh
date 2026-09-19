package usage

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fileCredentials keeps a test's credential reads on the file it wrote,
// and out of the Keychain of whoever runs the suite.
func fileCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("CLAWDH_CREDENTIALS_FILE", "1")
}

// writeCredsFile stores a login whose clocks are relative to now: fixed
// timestamps in a fixture quietly become "expired" as the months pass,
// and the test then fails for a reason that has nothing to do with it.
func writeCredsFile(t *testing.T, dir string, access, refresh time.Time) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".credentials.json"), fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"max","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		access.UnixMilli(), refresh.UnixMilli()))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestServiceCachesAndForgets(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})

	first := svc.Get(context.Background(), "work", dir)
	if first.Usage == nil {
		t.Fatalf("no usage: %s", first.Error)
	}
	if first.Session == nil || first.Session.Plan != "max" {
		t.Fatalf("session = %+v", first.Session)
	}
	if first.Session.AccessExpiresAt == nil || first.Session.SessionExpiresAt == nil {
		t.Fatal("both clocks should be reported")
	}

	svc.Get(context.Background(), "work", dir)
	if calls != 1 {
		t.Errorf("made %d calls, want the second one served from cache", calls)
	}

	svc.Forget("work")
	svc.Get(context.Background(), "work", dir)
	if calls != 2 {
		t.Errorf("made %d calls, want a refetch after Forget", calls)
	}
}

func TestServiceExpiresCache(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"tok"}}`)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	now := time.Now()
	svc.now = func() time.Time { return now }

	svc.Get(context.Background(), "work", dir)
	now = now.Add(TTL + time.Second)
	svc.Get(context.Background(), "work", dir)

	if calls != 2 {
		t.Errorf("made %d calls, want a refetch once the TTL passed", calls)
	}
}

// An account that was never signed in still has to render: the page says
// so in words rather than showing an empty card.
func TestServiceWithoutCredentials(t *testing.T) {
	fileCredentials(t)
	// Not NewService: that one reads and writes the real ~/.clawdh.
	snapshot := NewServiceWithClient(&Client{}).Get(context.Background(), "missing", filepath.Join(t.TempDir(), "nope"))
	if snapshot.State != StateSignedOut {
		t.Errorf("State = %q, want %q", snapshot.State, StateSignedOut)
	}
	if snapshot.Error == "" {
		t.Fatal("want an error explaining the login could not be read")
	}
	if snapshot.Usage != nil {
		t.Error("want no usage when there are no credentials")
	}
}

// The states are the whole point of the dot next to an account's name;
// each one has to come from a different real condition, or the dot is
// just decoration.
func TestServiceStates(t *testing.T) {
	fileCredentials(t)
	future, past := time.Now().Add(30*24*time.Hour), time.Now().Add(-time.Hour)

	t.Run("linked", func(t *testing.T) {
		dir := t.TempDir()
		writeCredsFile(t, dir, time.Now().Add(time.Hour), future)
		srv := stubUsage(t, http.StatusOK, realPayload)
		got := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			Get(context.Background(), "a", dir)
		if got.State != StateLinked {
			t.Errorf("State = %q, want %q (error: %s)", got.State, StateLinked, got.Error)
		}
	})

	// The refresh token is the clock that actually ends a login, so this
	// is decided without asking the API at all.
	t.Run("expired refresh token", func(t *testing.T) {
		dir := t.TempDir()
		writeCredsFile(t, dir, past, past)
		srv := stubUsage(t, http.StatusOK, realPayload)
		got := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			Get(context.Background(), "a", dir)
		if got.State != StateExpired {
			t.Errorf("State = %q, want %q", got.State, StateExpired)
		}
	})

	// The bug this guards: Claude Code only refreshes the access token
	// when it runs, so between runs the stored one is stale while the
	// login is perfectly good. Sending it earned a 401 and the account
	// read as "login expired" — including the account the person was
	// using at that moment.
	t.Run("stale access token is still a linked account", func(t *testing.T) {
		dir := t.TempDir()
		writeCredsFile(t, dir, past, future)
		var called bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		got := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			Get(context.Background(), "a", dir)
		if got.State != StateLinked {
			t.Errorf("State = %q, want %q", got.State, StateLinked)
		}
		if called {
			t.Error("a token we already know is stale must not be sent")
		}
		if got.Error == "" {
			t.Error("want a note saying why the numbers are missing")
		}
		if got.Session == nil || got.Session.SessionExpiresAt == nil {
			t.Error("want the login clock, which is the one still running")
		}
	})

	t.Run("token rejected", func(t *testing.T) {
		dir := t.TempDir()
		writeCredsFile(t, dir, time.Now().Add(time.Hour), future)
		srv := stubUsage(t, http.StatusUnauthorized, "")
		got := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			Get(context.Background(), "a", dir)
		if got.State != StateExpired {
			t.Errorf("State = %q, want %q", got.State, StateExpired)
		}
	})

	// A server error says nothing about whether this account works, so
	// the page must not accuse it of being broken.
	t.Run("api unavailable", func(t *testing.T) {
		dir := t.TempDir()
		writeCredsFile(t, dir, time.Now().Add(time.Hour), future)
		srv := stubUsage(t, http.StatusInternalServerError, "")
		got := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			Get(context.Background(), "a", dir)
		if got.State != StateUnknown {
			t.Errorf("State = %q, want %q", got.State, StateUnknown)
		}
		if got.Session == nil {
			t.Error("want the session clock even when the API is down")
		}
	})
}

func stubUsage(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// When the API is unreachable the session clock is still known, and
// losing it would drop the answer to "how long am I signed in for?".
func TestServiceKeepsSessionWhenAPIFails(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	snapshot := svc.Get(context.Background(), "work", dir)

	if snapshot.Usage != nil {
		t.Error("want no usage when the token was rejected")
	}
	if snapshot.Error == "" {
		t.Error("want an error explaining why")
	}
	if snapshot.Session == nil || snapshot.Session.SessionExpiresAt == nil {
		t.Error("want the session clock even without usage")
	}
}

// The page polls every few seconds and a second tab doubles that, so
// overlapping requests for one account must share a single call to
// Anthropic rather than each starting their own.
func TestOverlappingRequestsShareOneFetch(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Long enough that every caller below arrives while it runs.
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
				t.Errorf("no usage returned: %s", got.Error)
			}
		}()
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("made %d upstream calls for one cache miss, want 1", n)
	}
}

// A request the browser abandoned must not leave its failure in the
// cache for everyone who asks next.
func TestACancelledRequestIsNotCached(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	release := make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()
	defer close(release)

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Snapshot, 1)
	go func() { done <- svc.Get(ctx, "work", dir) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	if got := <-done; got.Usage != nil {
		t.Fatal("want no usage from the abandoned request")
	}

	svc.mu.Lock()
	_, cached := svc.entries["work"]
	svc.mu.Unlock()
	if cached {
		t.Error("the abandoned request's failure was cached")
	}
}

// A 429 is not a broken account and not a reason to blank the card. It
// is a reason to stop asking for a while — which is the part that has
// to be tested, because the wrong answer here is an endless retry loop
// that keeps the rate limit alive.
func TestRateLimitBacksOffAndKeepsTheLastNumbers(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	var calls atomic.Int64
	limited := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "0") // what this endpoint actually sends
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	now := time.Now()
	svc.now = func() time.Time { return now }

	// A good read first, so there is something to keep showing.
	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
		t.Fatalf("no usage on the first read: %s", got.Error)
	}

	limited.Store(true)
	now = now.Add(TTL + time.Second)
	got := svc.Get(context.Background(), "work", dir)

	if got.State != StateLinked {
		t.Errorf("State = %q, want %q: being throttled says the login works", got.State, StateLinked)
	}
	if got.Usage == nil {
		t.Error("want the last numbers kept on screen rather than an empty card")
	}
	if !strings.Contains(got.Error, "rate-limiting") {
		t.Errorf("note = %q, want it to say what happened", got.Error)
	}

	// Now the part that matters: retries follow the backoff, not the
	// cache. Ten cache misses over ten minutes must not be ten more
	// requests — the backoff schedule (1, 2, 4, then 5 minutes) allows
	// three, and every poll in between is answered without asking.
	before := calls.Load()
	for i := 0; i < 10; i++ {
		now = now.Add(TTL)
		svc.Get(context.Background(), "work", dir)
	}
	if after := calls.Load() - before; after > 4 {
		t.Errorf("made %d requests across 10 cache misses, want the backoff to allow ~3", after)
	}

	// Once the current wait expires, it tries again — and a success
	// clears the backoff.
	limited.Store(false)
	now = now.Add(backoffMax + time.Second)
	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil || got.Error != "" {
		t.Errorf("want a clean read once the backoff expired, got error %q", got.Error)
	}
	svc.mu.Lock()
	_, stillWaiting := svc.cooldowns["work"]
	svc.mu.Unlock()
	if stillWaiting {
		t.Error("a successful read must clear the backoff")
	}
}

// Each refusal waits longer than the last, up to five minutes, so a
// limit that is not letting up is not met with the same request rate
// forever — and a card is never frozen for longer than that.
func TestBackoffDoubles(t *testing.T) {
	svc := NewServiceWithClient(&Client{})
	now := time.Now()
	svc.now = func() time.Time { return now }

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, expected := range want {
		until := svc.refused("work", 0)
		if got := until.Sub(now); got != expected {
			t.Errorf("refusal %d waits %s, want %s", i+1, got, expected)
		}
	}
}

// A Retry-After worth waiting is honoured; the "0" this endpoint sends
// is not a delay and must not shorten the backoff.
func TestRetryAfterHeader(t *testing.T) {
	if got := retryAfter("120"); got != 2*time.Minute {
		t.Errorf("retryAfter(\"120\") = %s, want 2m", got)
	}
	if got := retryAfter("0"); got != 0 {
		t.Errorf("retryAfter(\"0\") = %s, want 0", got)
	}
	if got := retryAfter(""); got != 0 {
		t.Errorf("retryAfter(\"\") = %s, want 0", got)
	}
	if got := retryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)); got < time.Minute {
		t.Errorf("an HTTP-date Retry-After gave %s, want about 90s", got)
	}

	svc := NewServiceWithClient(&Client{})
	now := time.Now()
	svc.now = func() time.Time { return now }
	if got := svc.refused("work", 10*time.Minute).Sub(now); got != 10*time.Minute {
		t.Errorf("a 10m Retry-After produced %s, want it honoured", got)
	}
	// ...including when a quiet spell has just started the backoff over.
	now = now.Add(3 * time.Hour)
	if got := svc.refused("work", 10*time.Minute).Sub(now); got != 10*time.Minute {
		t.Errorf("a 10m Retry-After after a quiet spell produced %s, want it honoured", got)
	}
	// ...but it never shortens what the backoff already decided.
	svc2 := NewServiceWithClient(&Client{})
	svc2.now = func() time.Time { return now }
	if got := svc2.refused("work", time.Second).Sub(now); got != backoffFirst {
		t.Errorf("a 1s Retry-After produced %s, want the %s backoff to win", got, backoffFirst)
	}
}

// A refusal that comes long after the last cooldown ran out starts a
// new spell of backoff. Carrying the old step over is what sent one
// refusal, hours after the last, straight to the longest wait.
func TestBackoffStartsOverAfterAQuietSpell(t *testing.T) {
	cases := []struct {
		name string
		gap  time.Duration // from the end of a two-minute cooldown to the next refusal
		// failed is when, after that cooldown ended, Anthropic answered
		// with neither numbers nor a refusal; zero for not at all.
		failed time.Duration
		want   time.Duration
	}{
		{"as the cooldown ends", 0, 0, 4 * time.Minute},
		{"within a step of its end", 2*time.Minute - time.Second, 0, 4 * time.Minute},
		{"more than a step after its end", 2*time.Minute + time.Second, 0, backoffFirst},
		{"hours later", 3 * time.Hour, 0, backoffFirst},
		// A failed answer is held for the TTL, so the spell runs on from
		// the end of that, not from the end of the cooldown.
		{"a step after its end, with a failure between", 2*time.Minute + time.Second, 10 * time.Second, 4 * time.Minute},
		{"hours after a failure", 3 * time.Hour, 10 * time.Second, backoffFirst},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := NewServiceWithClient(&Client{})
			now := time.Now()
			svc.now = func() time.Time { return now }

			svc.refused("work", 0)
			until := svc.refused("work", 0) // the two-minute step

			if c.failed > 0 {
				now = until.Add(c.failed)
				svc.failed("work")
			}
			now = until.Add(c.gap)
			if got := svc.refused("work", 0).Sub(now); got != c.want {
				t.Errorf("waits %s, want %s", got, c.want)
			}
		})
	}
}

// Under strain Anthropic can answer a refusal, then a 500, then another
// refusal. Each 500 is held for the whole TTL, so the next refusal came
// more than a minute after the cooldown ended and was counted as a new
// spell: the wait went back to a minute every time and never grew.
func TestBackoffGrowsThroughOtherFailures(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	// One good read, then refusals and 500s by turns.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n := calls.Add(1); {
		case n == 1:
			_, _ = w.Write([]byte(realPayload))
		case n%2 == 0:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	start := time.Now()
	now := start
	svc.now = func() time.Time { return now }

	// Half an hour of a page polling. Every seven seconds rather than
	// five: real requests take time, and a fake clock's polls landing
	// exactly on the minute would hide the gap that caused this.
	var waits []time.Duration
	for ; now.Sub(start) < 30*time.Minute; now = now.Add(7 * time.Second) {
		before := calls.Load()
		svc.Get(context.Background(), "work", dir)
		if n := calls.Load(); n != before && n%2 == 0 {
			svc.mu.Lock()
			waits = append(waits, svc.cooldowns["work"].step)
			svc.mu.Unlock()
		}
	}

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}
	if len(waits) < len(want) {
		t.Fatalf("refusals waited %v, want them to grow %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("refusals waited %v, want them to grow %v", waits, want)
		}
	}
}

// "These numbers are from 15:04" was not enough to go on: read an hour
// later, or just after midnight, it looks like a moment ago.
func TestPausedNoteSaysHowOldTheNumbersAre(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 30, 0, 0, time.Local)
	last := &Report{FetchedAt: now.Add(-7 * time.Minute)}
	want := "Anthropic is rate-limiting plan usage. These numbers are from 10:23, 7 min ago; trying again at 10:34."
	if got := pausedNote(now.Add(4*time.Minute), last, now); got != want {
		t.Errorf("note = %q\nwant   %q", got, want)
	}

	// Just after midnight, yesterday's numbers carry their date.
	now = time.Date(2026, 9, 15, 0, 20, 0, 0, time.Local)
	last = &Report{FetchedAt: time.Date(2026, 9, 14, 22, 5, 0, 0, time.Local)}
	got := pausedNote(now.Add(time.Minute), last, now)
	for _, part := range []string{"from Sep 14 22:05, 2 h 15 min ago", "trying again at 00:21"} {
		if !strings.Contains(got, part) {
			t.Errorf("note = %q, want it to contain %q", got, part)
		}
	}

	if got := pausedNote(now.Add(time.Minute), nil, now); !strings.Contains(got, "no recent numbers") {
		t.Errorf("note = %q, want it to say there is nothing to show rather than date nothing", got)
	}
}

// usageStub stands in for the endpoint: it answers with realPayload until
// a test sets another status, and counts every request that reaches it.
type usageStub struct {
	*httptest.Server
	status atomic.Int64
	calls  atomic.Int64
}

func newUsageStub(t *testing.T) *usageStub {
	t.Helper()
	stub := &usageStub{}
	stub.status.Store(http.StatusOK)
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		if code := int(stub.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(realPayload))
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (stub *usageStub) usageClient() *Client {
	return &Client{Endpoint: stub.URL, HTTPClient: stub.Client()}
}

// Between runs of Claude Code the stored access token goes stale while
// the login is fine, and the card used to go blank for as long as that
// lasted — often hours. The numbers read before it went stale are still
// the best answer there is.
func TestStaleAccessTokenKeepsTheLastNumbers(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	start := time.Now()
	writeCredsFile(t, dir, start.Add(time.Hour), start.Add(30*24*time.Hour))

	stub := newUsageStub(t)
	svc := NewServiceWithClient(stub.usageClient())
	now := start
	svc.now = func() time.Time { return now }

	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
		t.Fatalf("no usage on the first read: %s", got.Error)
	}

	// Two hours on, nobody has run Claude Code and the token is stale.
	now = start.Add(2*time.Hour + 30*time.Second)
	got := svc.Get(context.Background(), "work", dir)
	if got.State != StateLinked {
		t.Errorf("State = %q, want %q", got.State, StateLinked)
	}
	if got.Usage == nil {
		t.Fatal("want the last numbers kept on the card, not an empty one")
	}
	for _, part := range []string{"refreshes", "2 h ago"} {
		if !strings.Contains(got.Error, part) {
			t.Errorf("note = %q, want it to say why the numbers are old and how old (%q)", got.Error, part)
		}
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("made %d upstream calls, want the stale token never sent", n)
	}

	// Claude Code runs and refreshes the token. The card catches up
	// within seconds rather than a whole TTL later.
	writeCredsFile(t, dir, now.Add(8*time.Hour), now.Add(30*24*time.Hour))
	now = now.Add(11 * time.Second)
	got = svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 2 || got.Error != "" {
		t.Errorf("want a fresh read once the token was refreshed, got %d calls and note %q", n, got.Error)
	}
}

// Numbers past StaleReportAge are wrong, not old, and that is as true of
// numbers held in memory as of numbers read back from disk. A server
// left running all day used to keep serving them on every path that
// shows the last good read.
func TestNumbersTooOldToMeanAnythingAreNotServed(t *testing.T) {
	fileCredentials(t)
	stale := StaleReportAge + time.Minute

	// readThenAge reads good numbers once, then moves the clock on past
	// StaleReportAge.
	readThenAge := func(t *testing.T, accessLasts time.Duration) (*Service, *usageStub, string, *time.Time) {
		t.Helper()
		dir := t.TempDir()
		start := time.Now()
		writeCredsFile(t, dir, start.Add(accessLasts), start.Add(30*24*time.Hour))
		stub := newUsageStub(t)
		svc := NewServiceWithClient(stub.usageClient())
		now := start
		svc.now = func() time.Time { return now }
		if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
			t.Fatalf("no usage on the first read: %s", got.Error)
		}
		now = start.Add(stale)
		return svc, stub, dir, &now
	}

	t.Run("stale access token", func(t *testing.T) {
		svc, _, dir, _ := readThenAge(t, time.Hour)
		if got := svc.Get(context.Background(), "work", dir); got.Usage != nil {
			t.Errorf("served numbers from %s ago: %q", stale, got.Error)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		svc, stub, dir, now := readThenAge(t, 8*time.Hour)
		stub.status.Store(http.StatusTooManyRequests)
		if got := svc.Get(context.Background(), "work", dir); got.Usage != nil {
			t.Errorf("the refusal served numbers from %s ago", stale)
		}
		*now = now.Add(shortTTL + time.Second)
		got := svc.Get(context.Background(), "work", dir)
		if got.Usage != nil {
			t.Errorf("the cooldown served numbers from %s ago", stale)
		}
		if !strings.Contains(got.Error, "no recent numbers") {
			t.Errorf("note = %q, want it to say there is nothing recent to show", got.Error)
		}
	})

	t.Run("server error", func(t *testing.T) {
		svc, stub, dir, _ := readThenAge(t, 8*time.Hour)
		stub.status.Store(http.StatusInternalServerError)
		if got := svc.Get(context.Background(), "work", dir); got.Usage != nil {
			t.Errorf("served numbers from %s ago: %q", stale, got.Error)
		}
	})
}

// roundTripFunc lets a test count every request a Client makes,
// including the ones that never reach a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closedURL is an address nothing listens on, so a request to it fails
// the way one does with no network: before anything is sent.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	return url
}

// No network yet after a laptop wakes: the card keeps the last numbers
// rather than blanking, and asks again within seconds, because a request
// that never left this machine costs Anthropic nothing.
func TestUnreachableKeepsTheNumbersAndRetriesSoon(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	stub := newUsageStub(t)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	var attempts atomic.Int64
	client := &Client{
		Endpoint: stub.URL,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts.Add(1)
			return transport.RoundTrip(r)
		})},
	}
	svc := NewServiceWithClient(client)
	now := time.Now()
	svc.now = func() time.Time { return now }

	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
		t.Fatalf("no usage on the first read: %s", got.Error)
	}

	client.Endpoint = closedURL(t)
	now = now.Add(TTL + time.Second)
	got := svc.Get(context.Background(), "work", dir)
	if got.State != StateUnknown {
		t.Errorf("State = %q, want %q", got.State, StateUnknown)
	}
	if got.Usage == nil {
		t.Error("want the last numbers kept on the card while Anthropic is out of reach")
	}
	for _, part := range []string{"These numbers are from", "could not reach Anthropic"} {
		if !strings.Contains(got.Error, part) {
			t.Errorf("note = %q, want it to contain %q", got.Error, part)
		}
	}

	// Seconds later, not a TTL later.
	now = now.Add(11 * time.Second)
	svc.Get(context.Background(), "work", dir)
	if n := attempts.Load(); n != 3 {
		t.Errorf("made %d attempts, want an unreachable Anthropic tried again within seconds", n)
	}

	client.Endpoint = stub.URL
	now = now.Add(11 * time.Second)
	if got := svc.Get(context.Background(), "work", dir); got.Error != "" || got.State != StateLinked {
		t.Errorf("want a clean read once the network is back, got %q (%s)", got.State, got.Error)
	}
}

// A 500 is an answer from Anthropic, so it is not asked again any sooner
// than a success would be: an outage is no reason to call six times a
// minute. The card keeps the last numbers meanwhile.
func TestServerErrorKeepsTheNumbersForATTL(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	stub := newUsageStub(t)
	svc := NewServiceWithClient(stub.usageClient())
	now := time.Now()
	svc.now = func() time.Time { return now }

	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil {
		t.Fatalf("no usage on the first read: %s", got.Error)
	}

	stub.status.Store(http.StatusInternalServerError)
	now = now.Add(TTL + time.Second)
	got := svc.Get(context.Background(), "work", dir)
	if got.State != StateUnknown || got.Usage == nil {
		t.Errorf("want an unknown state with the last numbers kept, got %q with usage %v", got.State, got.Usage != nil)
	}
	for _, part := range []string{"These numbers are from", "unavailable"} {
		if !strings.Contains(got.Error, part) {
			t.Errorf("note = %q, want it to contain %q", got.Error, part)
		}
	}

	now = now.Add(11 * time.Second)
	svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 2 {
		t.Errorf("made %d calls, want a server error held for the TTL like any other answer", n)
	}

	now = now.Add(TTL)
	svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 3 {
		t.Errorf("made %d calls, want one more once the TTL passed", n)
	}
}

// A card waiting out a rate limit used to keep its "paused" answer for a
// whole TTL, so the retry could come most of a minute after the cooldown
// had already ended.
func TestTheEndOfACooldownIsNoticedPromptly(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	stub := newUsageStub(t)
	svc := NewServiceWithClient(stub.usageClient())
	now := time.Now()
	svc.now = func() time.Time { return now }

	svc.Get(context.Background(), "work", dir)
	stub.status.Store(http.StatusTooManyRequests)
	now = now.Add(TTL + time.Second)
	svc.Get(context.Background(), "work", dir) // refused: one minute
	now = now.Add(time.Minute + time.Second)
	svc.Get(context.Background(), "work", dir) // refused again: two minutes

	svc.mu.Lock()
	until := svc.cooldowns["work"].until
	svc.mu.Unlock()

	// Partway through, the card is answered without asking anyone.
	now = now.Add(70 * time.Second)
	svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 3 {
		t.Fatalf("made %d calls, want none during the cooldown", n)
	}

	// Seconds after it ends, Anthropic is asked again.
	stub.status.Store(http.StatusOK)
	now = until.Add(6 * time.Second)
	got := svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 4 || got.Error != "" {
		t.Errorf("want a fresh read %s after the cooldown ended, got %d calls and note %q", 6*time.Second, n, got.Error)
	}
}

// Expire asks for newer numbers and nothing more. It must not refetch a
// snapshot that was only just read — whatever calls it may call it in a
// burst — and must not do to a rate limit what Forget does.
func TestExpire(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))

	stub := newUsageStub(t)
	svc := NewServiceWithClient(stub.usageClient())
	now := time.Now()
	svc.now = func() time.Time { return now }

	svc.Get(context.Background(), "work", dir)

	now = now.Add(expireFloor - time.Second)
	svc.Expire("work")
	svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("made %d calls, want a snapshot younger than %s kept", n, expireFloor)
	}

	now = now.Add(2 * time.Second)
	svc.Expire("work")
	if got := svc.Get(context.Background(), "work", dir); got.Usage == nil || stub.calls.Load() != 2 {
		t.Errorf("made %d calls, want one fresh read once the snapshot was old enough to expire", stub.calls.Load())
	}

	// In a rate limit, the cooldown and the numbers both survive it.
	stub.status.Store(http.StatusTooManyRequests)
	now = now.Add(TTL + time.Second)
	svc.Get(context.Background(), "work", dir) // refused
	now = now.Add(expireFloor + time.Second)
	svc.Expire("work")
	got := svc.Get(context.Background(), "work", dir)
	if n := stub.calls.Load(); n != 3 {
		t.Errorf("made %d calls, want Expire to leave the cooldown in force", n)
	}
	if got.Usage == nil {
		t.Error("Expire threw away the last numbers")
	}

	// Nothing cached, nothing to do.
	svc.Expire("nobody")
}

// The numbers survive a restart, which is the whole point: clawdh
// restarts itself whenever it updates, and landing in the middle of a
// rate limit with an empty card is exactly the case this exists for.
func TestLastGoodNumbersSurviveARestart(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))
	cache := filepath.Join(t.TempDir(), "usage.json")

	limited := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(realPayload))
	}))
	defer srv.Close()

	// One good read, by the service that is about to "restart".
	before := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	before.CachePath = cache
	if got := before.Get(context.Background(), "work", dir); got.Usage == nil {
		t.Fatalf("no usage on the first read: %s", got.Error)
	}

	// A new process, the same machine, and Anthropic now refusing.
	limited.Store(true)
	after := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	after.CachePath = cache
	after.loadReports()

	got := after.Get(context.Background(), "work", dir)
	if got.Usage == nil || len(got.Usage.Limits) == 0 {
		t.Fatalf("want the saved numbers on the card, got error %q", got.Error)
	}
	if !strings.Contains(got.Error, "rate-limiting") {
		t.Errorf("note = %q, want it to say why they are not fresh", got.Error)
	}
	// And the note has to say how old they are, or they read as current.
	if !strings.Contains(got.Error, got.Usage.FetchedAt.Local().Format("15:04")) {
		t.Errorf("note = %q, want it to name when the numbers were read", got.Error)
	}
}

// Numbers older than the shortest window on the page have rolled over,
// so they are not stale — they are wrong, and must not come back.
func TestReportsTooOldToMeanAnythingAreDropped(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "usage.json")
	old := time.Now().Add(-StaleReportAge - time.Hour)
	writeFile(t, cache, fmt.Sprintf(
		`{"accounts":{"work":{"limits":[{"kind":"session","label":"Current session","percent":42}],"fetchedAt":%q}}}`,
		old.UTC().Format(time.RFC3339)))

	svc := NewServiceWithClient(&Client{})
	svc.CachePath = cache
	svc.loadReports()

	svc.mu.Lock()
	_, kept := svc.lastReport["work"]
	svc.mu.Unlock()
	if kept {
		t.Error("a report from before the session window rolled was kept")
	}
}

// A Service nobody pointed at a cache file writes nothing, anywhere.
// This is what keeps a test run from reaching into the real ~/.clawdh.
func TestPersistenceIsOffWithoutACachePath(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))
	srv := stubUsage(t, http.StatusOK, realPayload)

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	if svc.cachePath() != "" {
		t.Fatalf("cachePath = %q, want persistence off by default", svc.cachePath())
	}
	svc.Get(context.Background(), "work", dir)
	svc.Forget("work")
	// Nothing to assert about the filesystem beyond this: with no path
	// there is no file to write, and cachePath being empty is what the
	// save and load paths both check first.
}

// Forgetting an account takes its saved numbers with it, so an account
// removed and recreated under the same id cannot inherit them.
func TestForgetDropsTheSavedNumbers(t *testing.T) {
	fileCredentials(t)
	dir := t.TempDir()
	writeCredsFile(t, dir, time.Now().Add(8*time.Hour), time.Now().Add(30*24*time.Hour))
	cache := filepath.Join(t.TempDir(), "usage.json")
	srv := stubUsage(t, http.StatusOK, realPayload)

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	svc.CachePath = cache
	svc.Get(context.Background(), "work", dir)

	svc.Forget("work")

	reborn := NewServiceWithClient(&Client{})
	reborn.CachePath = cache
	reborn.loadReports()
	reborn.mu.Lock()
	_, kept := reborn.lastReport["work"]
	reborn.mu.Unlock()
	if kept {
		t.Error("the removed account's numbers were still on disk")
	}
}

// A login the gateway holds: its stale local token is expected (the gateway
// refreshes it now), so the gateway's own reading is shown instead of "run it
// once" — advice that would rotate the token out from under the gateway.
func TestStaleTokenUsesTheGatewayReading(t *testing.T) {
	dir := t.TempDir()
	past, future := time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour)
	writeCredsFile(t, dir, past, future)
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	svc := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	svc.Gateway = func(configDir string) (*Report, string, bool) {
		if configDir != dir {
			return nil, "", false
		}
		return &Report{Limits: []Limit{{Kind: "session", Label: "Current session", Percent: 28}}, FetchedAt: time.Now()}, "Read through the gateway.", true
	}
	got := svc.Get(context.Background(), "shared", dir)
	if called {
		t.Error("a stale token must not be sent even when the gateway answers")
	}
	if got.Usage == nil || len(got.Usage.Limits) != 1 || got.Usage.Limits[0].Percent != 28 {
		t.Fatalf("Usage = %+v, want the gateway's reading", got.Usage)
	}
	if got.Error != "" || got.Note == "" {
		t.Errorf("Error = %q, Note = %q; want no error and the gateway note", got.Error, got.Note)
	}
	if got.State != StateLinked {
		t.Errorf("State = %q, want linked", got.State)
	}

	// A login the gateway doesn't know keeps the honest local explanation.
	svc2 := NewServiceWithClient(&Client{Endpoint: srv.URL, HTTPClient: srv.Client()})
	svc2.Gateway = func(string) (*Report, string, bool) { return nil, "", false }
	if got := svc2.Get(context.Background(), "local", dir); got.Error == "" || got.Usage != nil {
		t.Errorf("without a gateway reading the stale-token note should remain, got %+v", got)
	}
}
