package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"clawdh/internal/gateway"
	"clawdh/panel"
	"clawdh/panelpg"
)

// dbUpstream serves the gateway from the same database the admin panel manages.
// A member's key maps (by hash) to a share, a share to an account, and an
// account to a live, self-refreshing token. Many members' keys can point at one
// account — that is how one login serves the whole team at once.
//
// Nothing a member's request does waits for the database. It is served from the
// panel state last read, which is re-read in the background; its quota standing
// is read in the background too; and every write goes through the background
// writer (dbwriter.go). The one exception is a key the state does not know yet,
// which gets one bounded fresh read (see freshRead).
type dbUpstream struct {
	store  *panel.Store
	pg     panelDB // metering, windows, collisions and quota standings
	secret *panel.Secret
	w      *writer

	mu           sync.Mutex
	data         panel.Data
	dataAt       time.Time                   // when data was last read successfully
	flight       *flight                     // the re-read in progress, if any
	loadFailures int                         // consecutive failed re-reads
	retryAt      time.Time                   // no re-read starts before this after a failure
	managers     map[string]*gateway.Manager // accountID -> token manager
	limitCache   map[string]limitCacheEntry  // person+account -> recent quota standing
}

// panelDB is the part of the panel database the gateway uses besides the panel
// state itself. *panelpg.Backend is the real one; tests stand in a database
// that is slow, or failing, on demand — the only conditions that matter here.
type panelDB interface {
	RecordWindows(accountID string, fiveH, sevenD float64, fiveHReset, sevenDReset time.Time, models []panel.ModelWindow)
	RecordUsage(ctx context.Context, ev panelpg.UsageEvent) error
	RecordCollision(ctx context.Context, accountID, note string) error
	MemberLimitStatus(ctx context.Context, personID, accountID string) (panelpg.LimitStatus, error)
	AccountWindows(ctx context.Context) ([]panel.AccountWindow, error)
	UsageBySubject(ctx context.Context, subjectType string, since time.Time) ([]panel.SubjectUsage, error)
	LatestEventAt(ctx context.Context) (time.Time, error)
}

// flight is one read of the panel state, which anyone who needs it waits on
// rather than starting another.
type flight struct {
	done chan struct{}
	data panel.Data
	err  error
}

// limitCacheEntry is a person's quota standing on an account as last read.
type limitCacheEntry struct {
	status  gateway.QuotaStatus
	at      time.Time // when it was last read, successfully or not
	reading bool      // a background read is in flight
}

const limitCacheTTL = 20 * time.Second

func newDBUpstream(ctx context.Context, dsn, keyB64 string) (*dbUpstream, error) {
	secret, err := panel.SecretFromBase64(keyB64)
	if err != nil {
		return nil, err
	}
	back, err := panelpg.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	u, err := newUpstream(panel.NewStoreWithBackend(back), back, secret)
	if err != nil {
		return nil, err
	}
	go u.runWriter(ctx)
	return u, nil
}

// newUpstream builds the upstream over a store and database and reads the panel
// state once. Every later read happens in the background, so this is the only
// one a start-up waits for. The background writer is not started here.
func newUpstream(store *panel.Store, db panelDB, secret *panel.Secret) (*dbUpstream, error) {
	u := &dbUpstream{
		store:      store,
		pg:         db,
		secret:     secret,
		w:          newWriter(),
		managers:   map[string]*gateway.Manager{},
		limitCache: map[string]limitCacheEntry{},
	}
	d, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("reading the panel: %w", err)
	}
	u.data, u.dataAt = d, time.Now()
	return u, nil
}

// dataTTL is how long the panel state is served before a background re-read
// starts, so a new share or a removed one takes effect within seconds.
const dataTTL = 15 * time.Second

// snapshot is the panel state a request is served from, and it never waits for
// the database: when the state is older than dataTTL it starts a re-read in the
// background and answers with what it has.
//
// It used to re-read in line, holding the one lock every request needs, and it
// only moved its clock on a successful read. So once the database became
// unreachable, every request from every member made a connection attempt of
// its own — about 2.8s each, one at a time.
func (u *dbUpstream) snapshot() panel.Data {
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Since(u.dataAt) >= dataTTL && !time.Now().Before(u.retryAt) {
		u.reloadLocked()
	}
	return u.data
}

// current is the state last read, without starting anything.
func (u *dbUpstream) current() panel.Data {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.data
}

// reloadLocked starts a background re-read unless one is running, and returns
// it to wait on. u.mu must be held.
func (u *dbUpstream) reloadLocked() *flight {
	if u.flight != nil {
		return u.flight
	}
	f := &flight{done: make(chan struct{})}
	u.flight = f
	go func() {
		d, err := u.store.Load()
		u.mu.Lock()
		u.noteLoadLocked(d, err)
		u.flight = nil
		u.mu.Unlock()
		f.data, f.err = d, err
		close(f.done)
	}()
	return f
}

// noteLoadLocked keeps a successful read, or backs off after a failed one while
// the state already read goes on being served. u.mu must be held.
func (u *dbUpstream) noteLoadLocked(d panel.Data, err error) {
	if err != nil {
		u.loadFailures++
		u.retryAt = time.Now().Add(backoff(u.loadFailures))
		if u.loadFailures == 1 {
			log.Printf("gateway: re-reading the panel failed; serving the state read %s ago and retrying in the background: %v",
				time.Since(u.dataAt).Round(time.Second), err)
		}
		return
	}
	if u.loadFailures > 0 {
		log.Printf("gateway: re-reading the panel works again after %d failed attempts", u.loadFailures)
	}
	u.data, u.dataAt = d, time.Now()
	u.loadFailures, u.retryAt = 0, time.Time{}
}

// freshReadWait bounds how long a request waits on a fresh read — the one case
// in which a request reads the database at all — and freshReadEvery how often
// one may start, so a withdrawn member's session retrying in a loop cannot turn
// into a stream of reads.
const (
	freshReadWait  = 3 * time.Second
	freshReadEvery = 2 * time.Second
)

// freshRead waits, briefly, for a read newer than the state being served. It
// reports false without waiting when the state was read moments ago or the
// database is failing, and false after freshReadWait if it has not answered.
func (u *dbUpstream) freshRead() (panel.Data, bool) {
	u.mu.Lock()
	f := u.flight
	if f == nil {
		if time.Now().Before(u.retryAt) || time.Since(u.dataAt) < freshReadEvery {
			u.mu.Unlock()
			return panel.Data{}, false
		}
		f = u.reloadLocked()
	}
	u.mu.Unlock()
	select {
	case <-f.done:
		return f.data, f.err == nil
	case <-time.After(freshReadWait):
		return panel.Data{}, false
	}
}

// Resolve maps a member key to the account's current access token.
//
// A key that is not in the state being served gets one fresh read before it is
// declared unknown, so a share created moments ago works on its first request
// rather than after the next background re-read — the window that produced a
// spurious "access has been withdrawn" right after granting access.
func (u *dbUpstream) Resolve(memberKey string) (gateway.Resolution, error) {
	keyHash := panel.HashToken(memberKey)
	d := u.snapshot()
	share, found := d.ShareByKeyHash(keyHash)
	if !found {
		if fresh, ok := u.freshRead(); ok {
			d = fresh
			share, found = d.ShareByKeyHash(keyHash)
		}
		if !found {
			return gateway.Resolution{}, gateway.ErrUnknownKey
		}
	}
	acct, ok := d.Account(share.AccountID)
	if !ok {
		return gateway.Resolution{}, gateway.ErrUnknownKey // the account was removed
	}
	if !acct.HasLogin() {
		return gateway.Resolution{}, fmt.Errorf("%s has no stored login", acct.Name)
	}
	mgr, err := u.managerFor(*acct)
	if err != nil {
		return gateway.Resolution{}, fmt.Errorf("opening the shared login for %s: %w", acct.Name, err)
	}
	token, err := mgr.Token(context.Background())
	if err != nil {
		// The login can't be rolled forward — its refresh token is dead, which
		// happens when the same account is still being used first-party
		// somewhere. Drop the cached manager so a re-added login is picked up,
		// and record the collision so the panel can warn that this account is
		// being used outside the gateway.
		u.forget(acct.ID)
		u.w.collision(acct.ID)
		return gateway.Resolution{}, fmt.Errorf("refreshing the shared login for %s: %w", acct.Name, err)
	}
	return gateway.Resolution{
		AccessToken: token,
		Label:       acct.Name,
		AccountID:   acct.ID,
		PersonID:    share.PersonID,
	}, nil
}

// Renew replaces an access token Anthropic just refused (gateway.Renewer): the
// manager adopts a newer credential the database holds, or spends the refresh
// token. If neither works the login is dead — drop the cached manager so a
// re-added login is picked up, and record the collision for the panel to show.
func (u *dbUpstream) Renew(accountID, bad string) (string, error) {
	u.mu.Lock()
	m := u.managers[accountID]
	u.mu.Unlock()
	if m == nil {
		return "", fmt.Errorf("no login is open for account %s", accountID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := m.Renew(ctx, bad)
	if err != nil {
		log.Printf("gateway: renewing the login for account %s after a 401: %v", accountID, err)
		u.forget(accountID)
		u.w.collision(accountID)
		return "", err
	}
	log.Printf("gateway: renewed the login for account %s after Anthropic refused its token", accountID)
	return tok, nil
}

// Record meters one forwarded response (gateway.Recorder). It is queued, never
// written in line: a request must not wait on the database to finish.
func (u *dbUpstream) Record(ev gateway.Event) { u.w.event(ev) }

// RecordWindows keeps an account's real 5h / weekly utilisation, read off
// Anthropic's headers (gateway.WindowRecorder). Queued, like Record — it used
// to be written before a response's first byte went back to the member. Nil
// models: a response's headers carry the two totals and nothing per-model, so
// whatever the usage poller last stored stands.
func (u *dbUpstream) RecordWindows(accountID string, w gateway.Windows) { u.w.window(accountID, w) }

// RecordWindowsAndModels stores a reading from the usage endpoint, which —
// unlike the headers — reports each model's own weekly allowance too. It is
// called only by the usage poller, never on a member's request.
func (u *dbUpstream) RecordWindowsAndModels(accountID string, w gateway.Windows, models []panel.ModelWindow) {
	u.pg.RecordWindows(accountID, w.FiveH, w.SevenD, w.FiveHReset, w.SevenDReset, models)
}

// Status reports a person's quota standing on an account (over the cap, and how
// close) without waiting for the database: the standing is re-read in the
// background at most every limitCacheTTL, and a request is judged on the one
// last read. The very first request of a person on an account has none yet and
// is let through — the same fail-open rule as ever, since a metering hiccup must
// never lock a team out of a subscription it is entitled to.
func (u *dbUpstream) Status(personID, accountID string) gateway.QuotaStatus {
	// Keyed by both: the same person can be under different standings on different
	// accounts (an account cap applies to whoever is using that account).
	key := personID + "\x00" + accountID
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.limitCache[key]
	if !e.reading && time.Since(e.at) >= limitCacheTTL {
		e.reading = true
		u.limitCache[key] = e
		go u.readLimit(key, personID, accountID)
	}
	return e.status
}

// readLimit refreshes one cached standing. A failed read keeps the standing it
// had and waits a full limitCacheTTL before trying again.
func (u *dbUpstream) readLimit(key, personID, accountID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := u.pg.MemberLimitStatus(ctx, personID, accountID)
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.limitCache[key]
	e.reading, e.at = false, time.Now()
	if err == nil {
		e.status = gateway.QuotaStatus{Over: st.Over, Fraction: st.Fraction, ResetAt: st.ResetAt, Message: st.Message}
	}
	u.limitCache[key] = e
}

// forget drops an account's cached token manager, so the next request rebuilds
// it from whatever the database now holds (e.g. a login the admin re-added).
func (u *dbUpstream) forget(accountID string) {
	u.mu.Lock()
	delete(u.managers, accountID)
	u.mu.Unlock()
}

// managerFor returns the one refreshing token-manager for an account, building
// it from the sealed credential the panel stored. All members of an account
// share the same manager, so the token is refreshed once, centrally.
func (u *dbUpstream) managerFor(acct panel.Account) (*gateway.Manager, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m, ok := u.managers[acct.ID]; ok {
		return m, nil
	}
	raw, err := u.secret.Open(acct.Credential)
	if err != nil {
		return nil, err
	}
	access, refreshTok, expires := parseCredential(raw)
	accountID := acct.ID
	m := gateway.NewManager(access, refreshTok, expires, func(fresh gateway.Credential) {
		// Queued and retried until it is stored: refresh tokens are single-use,
		// so this rotation is the only one that works from now on.
		u.w.credential(accountID, fresh)
	})
	// Before spending its refresh token, the manager looks for a newer credential
	// stored since — a re-added login, or `clawdh-server diagnose` testing the
	// refresh — so one rotated out of process is adopted, not fought. A fresh
	// read when the database answers promptly, the state already read when it
	// does not; the manager only ever adopts one newer than its own.
	m.Reload(func() (gateway.Credential, bool) {
		d, ok := u.freshRead()
		if !ok {
			d = u.current()
		}
		acct, ok := d.Account(accountID)
		if !ok {
			return gateway.Credential{}, false
		}
		raw, err := u.secret.Open(acct.Credential)
		if err != nil {
			return gateway.Credential{}, false
		}
		access, refreshTok, expires := parseCredential(raw)
		return gateway.Credential{AccessToken: access, RefreshToken: refreshTok, ExpiresAt: expires}, true
	})
	u.managers[acct.ID] = m
	return m, nil
}

// parseCredential pulls the tokens out of a stored claudeAiOauth blob.
func parseCredential(raw []byte) (access, refreshTok string, expires time.Time) {
	var b struct {
		O struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return "", "", time.Time{}
	}
	if b.O.ExpiresAt > 0 {
		expires = time.UnixMilli(b.O.ExpiresAt)
	}
	return b.O.AccessToken, b.O.RefreshToken, expires
}

// updateCredential writes fresh tokens back into a stored blob, preserving every
// other field (scopes, subscriptionType, ...).
func updateCredential(raw []byte, fresh gateway.Credential) []byte {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		m = map[string]any{}
	}
	oauth, _ := m["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		oauth = map[string]any{}
	}
	oauth["accessToken"] = fresh.AccessToken
	if fresh.RefreshToken != "" {
		oauth["refreshToken"] = fresh.RefreshToken
	}
	oauth["expiresAt"] = fresh.ExpiresAt.UnixMilli()
	m["claudeAiOauth"] = oauth
	out, _ := json.Marshal(m)
	return out
}
