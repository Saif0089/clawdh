package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"clawdh/internal/gateway"
	"clawdh/panel"
	"clawdh/panelpg"
)

// These pin what a member's request may and may not wait for. Every one of them
// is about a database that is slow or failing, because those are the only
// conditions under which the difference shows — and on 2026-09-28 it showed as
// prompts that sat unsent for ten seconds at a time.

const testMemberKey = "member-key"

// fakeBackend is the panel state's storage, made to hang or fail on demand.
type fakeBackend struct {
	mu    sync.Mutex
	raw   []byte
	ver   int64
	loads int
	fail  error         // Load and Save fail with this when set
	block chan struct{} // Load waits on this when set
}

func (b *fakeBackend) Load() ([]byte, int64, error) {
	b.mu.Lock()
	b.loads++
	block, fail, raw, ver := b.block, b.fail, b.raw, b.ver
	b.mu.Unlock()
	if block != nil {
		<-block
	}
	if fail != nil {
		return nil, 0, fail
	}
	return raw, ver, nil
}

func (b *fakeBackend) Save(raw []byte, expected int64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return false, b.fail
	}
	if expected != b.ver {
		return false, nil
	}
	b.raw, b.ver = append([]byte(nil), raw...), b.ver+1
	return true, nil
}

func (b *fakeBackend) set(fn func(*fakeBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

func (b *fakeBackend) loadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loads
}

// fakeDB is everything else the gateway reads and writes, with the same knobs.
type fakeDB struct {
	mu         sync.Mutex
	limit      panelpg.LimitStatus
	limitBlock chan struct{}
	limitReads int
	usage      []panelpg.UsageEvent
	usageErr   error
	usageBlock chan struct{}
}

func (f *fakeDB) RecordWindows(string, float64, float64, time.Time, time.Time, []panel.ModelWindow) {}
func (f *fakeDB) RecordCollision(context.Context, string, string) error                             { return nil }
func (f *fakeDB) AccountWindows(context.Context) ([]panel.AccountWindow, error)                     { return nil, nil }
func (f *fakeDB) LatestEventAt(context.Context) (time.Time, error)                                  { return time.Time{}, nil }
func (f *fakeDB) UsageBySubject(context.Context, string, time.Time) ([]panel.SubjectUsage, error) {
	return nil, nil
}

func (f *fakeDB) MemberLimitStatus(context.Context, string, string) (panelpg.LimitStatus, error) {
	f.mu.Lock()
	f.limitReads++
	block, st := f.limitBlock, f.limit
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return st, nil
}

func (f *fakeDB) RecordUsage(_ context.Context, ev panelpg.UsageEvent) error {
	f.mu.Lock()
	block, err := f.usageBlock, f.usageErr
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.usage = append(f.usage, ev)
	f.mu.Unlock()
	return nil
}

func (f *fakeDB) set(fn func(*fakeDB)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func testSecret(t *testing.T) *panel.Secret {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := panel.SecretFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sealedLogin(t *testing.T, s *panel.Secret, access, refreshTok string, expires time.Time) []byte {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refreshTok, "expiresAt": expires.UnixMilli(),
		"subscriptionType": "max",
	}})
	sealed, err := s.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// newTestUpstream is one shared account with one member, over storage and a
// database the test controls. The login is fresh, so serving it needs no
// refresh and nothing here reaches a real token service.
func newTestUpstream(t *testing.T) (*dbUpstream, *fakeBackend, *fakeDB, *panel.Secret) {
	t.Helper()
	s := testSecret(t)
	d := panel.Data{
		Accounts: []panel.Account{{ID: "acct1", Name: "Shared", Credential: sealedLogin(t, s, "at-1", "rt-1", time.Now().Add(8*time.Hour))}},
		Shares:   []panel.Share{{ID: "sh1", AccountID: "acct1", PersonID: "p1", KeyHash: panel.HashToken(testMemberKey)}},
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{raw: raw, ver: 1}
	db := &fakeDB{}
	u, err := newUpstream(panel.NewStoreWithBackend(b), db, s)
	if err != nil {
		t.Fatal(err)
	}
	return u, b, db, s
}

// ageState makes the served state old enough that a re-read is due.
func ageState(u *dbUpstream, by time.Duration) {
	u.mu.Lock()
	u.dataAt = time.Now().Add(-by)
	u.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// notBlocked runs fn and fails the test if it has not returned within a bound
// far longer than it should ever take. The databases in these tests hang
// forever, so code that waits on one never returns at all: the bound only has
// to separate "returned" from "stuck", which keeps it steady on a slow runner.
func notBlocked(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s is still waiting — it must not wait for the database", what)
	}
}

func resolveQuickly(t *testing.T, u *dbUpstream, key string) (res gateway.Resolution, err error) {
	t.Helper()
	notBlocked(t, "Resolve", func() { res, err = u.Resolve(key) })
	return res, err
}

// A database that hangs must cost a member nothing: requests are answered from
// the state already read while one re-read waits in the background.
func TestRequestsAreServedFromMemoryWhileTheDatabaseHangs(t *testing.T) {
	u, b, _, _ := newTestUpstream(t)
	hang := make(chan struct{})
	defer close(hang)
	b.set(func(b *fakeBackend) { b.block = hang })
	ageState(u, time.Minute)
	before := b.loadCount()

	for i := 0; i < 20; i++ {
		res, err := resolveQuickly(t, u, testMemberKey)
		if err != nil || res.AccessToken != "at-1" || res.PersonID != "p1" {
			t.Fatalf("Resolve = %+v, %v; want the share served from memory", res, err)
		}
	}
	if n := b.loadCount() - before; n != 1 {
		t.Errorf("%d re-reads started for 20 requests; want exactly one, in the background", n)
	}
}

// A database that refuses must not be asked again on every request — the old
// code did exactly that once its reads started failing, because it only moved
// its clock on success.
func TestAFailingDatabaseIsNotAskedOnEveryRequest(t *testing.T) {
	u, b, _, _ := newTestUpstream(t)
	b.set(func(b *fakeBackend) { b.fail = errors.New("ERROR: Your account or project has exceeded the quota") })
	ageState(u, time.Minute)
	before := b.loadCount()

	for i := 0; i < 50; i++ {
		if _, err := resolveQuickly(t, u, testMemberKey); err != nil {
			t.Fatalf("request %d failed although the state was in memory: %v", i, err)
		}
	}
	waitFor(t, "the failed re-read to be noted", func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.flight == nil && !u.retryAt.IsZero()
	})
	if n := b.loadCount() - before; n != 1 {
		t.Fatalf("%d reads for 50 requests against a failing database; want 1", n)
	}
	// Inside the backoff window no request starts another read. (The window
	// itself is pinned here rather than raced: how long 50 requests take is
	// the runner's business.)
	u.mu.Lock()
	u.retryAt = time.Now().Add(time.Minute)
	u.mu.Unlock()
	for i := 0; i < 50; i++ {
		resolveQuickly(t, u, testMemberKey)
	}
	if n := b.loadCount() - before; n != 1 {
		t.Errorf("%d reads while backing off; want none beyond the first", n)
	}
}

// A share granted moments ago works on its first request: an unknown key gets
// one fresh read before it is turned away.
func TestAShareGrantedMomentsAgoResolvesOnItsFirstRequest(t *testing.T) {
	u, b, _, s := newTestUpstream(t)
	d := panel.Data{
		Accounts: []panel.Account{{ID: "acct1", Name: "Shared", Credential: sealedLogin(t, s, "at-1", "rt-1", time.Now().Add(8*time.Hour))}},
		Shares: []panel.Share{
			{ID: "sh1", AccountID: "acct1", PersonID: "p1", KeyHash: panel.HashToken(testMemberKey)},
			{ID: "sh2", AccountID: "acct1", PersonID: "p2", KeyHash: panel.HashToken("new-key")},
		},
	}
	raw, _ := json.Marshal(d)
	b.set(func(b *fakeBackend) { b.raw, b.ver = raw, b.ver+1 })
	ageState(u, 5*time.Second) // older than freshReadEvery, newer than dataTTL

	res, err := u.Resolve("new-key")
	if err != nil || res.PersonID != "p2" {
		t.Fatalf("Resolve(new share) = %+v, %v; want it found by a fresh read", res, err)
	}
}

// ...but while the database is failing, an unknown key is turned away at once
// instead of waiting on a read that will not come: a withdrawn member's
// session retrying in a loop must not queue up reads, or anyone behind them.
func TestAnUnknownKeyDoesNotWaitOnAFailingDatabase(t *testing.T) {
	u, b, _, _ := newTestUpstream(t)
	b.set(func(b *fakeBackend) { b.fail = errors.New("connection refused") })
	ageState(u, time.Minute)
	u.snapshot()
	waitFor(t, "the failed re-read to be noted", func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.flight == nil && !u.retryAt.IsZero()
	})

	if _, err := resolveQuickly(t, u, "no-such-key"); !errors.Is(err, gateway.ErrUnknownKey) {
		t.Fatalf("err = %v; want ErrUnknownKey", err)
	}
}

// The quota standing is read in the background: a hanging read delays nobody,
// and one read is enough for any number of requests.
func TestQuotaStandingNeverWaitsForTheDatabase(t *testing.T) {
	u, _, db, _ := newTestUpstream(t)
	hang := make(chan struct{})
	db.set(func(f *fakeDB) {
		f.limitBlock = hang
		f.limit = panelpg.LimitStatus{Over: true, Fraction: 1.2, Message: "over"}
	})

	for i := 0; i < 10; i++ {
		var st gateway.QuotaStatus
		notBlocked(t, "Status", func() { st = u.Status("p1", "acct1") })
		if st.Over {
			t.Fatal("a standing was reported before any read finished")
		}
	}
	close(hang)
	waitFor(t, "the standing to be read", func() bool { return u.Status("p1", "acct1").Over })
	db.mu.Lock()
	reads := db.limitReads
	db.mu.Unlock()
	if reads != 1 {
		t.Errorf("%d standing reads for 11 requests; want 1", reads)
	}
}

// Metering and window readings are handed to the writer, never written on the
// response's path — a database that hangs must not hold a stream open.
func TestWritesDoNotHoldUpAResponse(t *testing.T) {
	u, _, db, _ := newTestUpstream(t)
	hang := make(chan struct{})
	defer close(hang)
	db.set(func(f *fakeDB) { f.usageBlock = hang })

	notBlocked(t, "recording a response", func() {
		u.Record(gateway.Event{AccountID: "acct1", PersonID: "p1", Model: "claude-sonnet-5", Input: 10, Output: 5})
		u.RecordWindows("acct1", gateway.Windows{FiveH: 0.4, SevenD: 0.7})
	})
	// Even with a flush stuck on the hanging database, the next response's
	// recording only queues.
	go u.flush()
	notBlocked(t, "recording while a flush is stuck", func() {
		u.Record(gateway.Event{AccountID: "acct1", PersonID: "p1", Model: "claude-sonnet-5", Input: 1})
	})
}

func storedLogin(t *testing.T, b *fakeBackend, s *panel.Secret) (access, refreshTok string, expires time.Time) {
	t.Helper()
	b.mu.Lock()
	raw := append([]byte(nil), b.raw...)
	b.mu.Unlock()
	var d panel.Data
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	acct, ok := d.Account("acct1")
	if !ok {
		t.Fatal("the account is gone")
	}
	plain, err := s.Open(acct.Credential)
	if err != nil {
		t.Fatal(err)
	}
	return parseCredential(plain)
}

// A rotation the database cannot take is kept and retried until it lands. It
// used to be attempted once and dropped silently — and a refresh token is
// single-use, so a rotation that never reaches the database is a login lost at
// the next restart.
func TestARotatedCredentialIsRetriedUntilStored(t *testing.T) {
	u, b, _, s := newTestUpstream(t)
	fresh := gateway.Credential{AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: time.Now().Add(9 * time.Hour)}

	b.set(func(b *fakeBackend) { b.fail = errors.New("connection refused") })
	u.w.credential("acct1", fresh)
	if err := u.flush(); err == nil {
		t.Fatal("flush reported success against a failing database")
	}
	if u.w.pending() != 1 {
		t.Fatalf("pending = %d; the rotation must stay queued", u.w.pending())
	}

	b.set(func(b *fakeBackend) { b.fail = nil })
	if err := u.flush(); err != nil {
		t.Fatal(err)
	}
	if _, rt, _ := storedLogin(t, b, s); rt != "rt-2" {
		t.Errorf("stored refresh token = %q; want the rotation", rt)
	}
	if u.w.pending() != 0 {
		t.Errorf("pending = %d after a successful flush", u.w.pending())
	}
}

// And a queued credential never overwrites a newer one the database holds —
// the login re-added, or rotated elsewhere, after ours was issued.
func TestAnOlderCredentialNeverOverwritesANewerStoredOne(t *testing.T) {
	u, b, _, s := newTestUpstream(t) // stored: rt-1, expiring in 8h
	u.w.credential("acct1", gateway.Credential{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(time.Hour)})
	if err := u.flush(); err != nil {
		t.Fatal(err)
	}
	if _, rt, _ := storedLogin(t, b, s); rt != "rt-1" {
		t.Errorf("stored refresh token = %q; an older credential overwrote a newer one", rt)
	}
}

// Usage rows survive a spell of failed writes and are then written once each.
func TestUsageRowsSurviveAnOutageAndAreWrittenOnce(t *testing.T) {
	u, _, db, _ := newTestUpstream(t)
	db.set(func(f *fakeDB) { f.usageErr = errors.New("connection refused") })
	for i := 0; i < 3; i++ {
		u.Record(gateway.Event{AccountID: "acct1", PersonID: "p1", Model: "claude-sonnet-5", Input: 10})
	}
	if err := u.flush(); err == nil {
		t.Fatal("flush reported success against a failing database")
	}
	db.set(func(f *fakeDB) { f.usageErr = nil })
	if err := u.flush(); err != nil {
		t.Fatal(err)
	}
	if err := u.flush(); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	n := len(db.usage)
	db.mu.Unlock()
	if n != 3 {
		t.Errorf("%d usage rows written; want each of the 3 exactly once", n)
	}
}

// The usage queue is bounded: past it the oldest rows go, never the gateway.
func TestTheUsageQueueDropsItsOldestWhenFull(t *testing.T) {
	w := newWriter()
	for i := 0; i < maxPendingEvents+1; i++ {
		w.event(gateway.Event{Input: int64(i)})
	}
	if len(w.events) != maxPendingEvents || w.dropped != 1 || w.events[0].ev.Input != 1 {
		t.Errorf("queue holds %d (dropped %d, oldest input %d); want %d, 1 dropped, oldest 1",
			len(w.events), w.dropped, w.events[0].ev.Input, maxPendingEvents)
	}
}
