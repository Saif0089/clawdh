package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"clawdh/internal/gateway"
	"clawdh/internal/meter"
	"clawdh/panel"
	"clawdh/panelpg"
)

// dbUpstream serves the gateway from the same database the admin panel manages.
// A member's key maps (by hash) to a share, a share to an account, and an
// account to a live, self-refreshing token. Many members' keys can point at one
// account — that is how one login serves the whole team at once.
type dbUpstream struct {
	store  *panel.Store
	pg     *panelpg.Backend // the concrete backend, for metering writes
	secret *panel.Secret

	mu         sync.Mutex
	data       panel.Data
	dataAt     time.Time
	managers   map[string]*gateway.Manager // accountID -> token manager
	limitCache map[string]limitCacheEntry  // personID -> recent quota standing
}

// limitCacheEntry is a person's cached quota standing, so the gateway checks the
// database at most every limitCacheTTL per person rather than every request.
type limitCacheEntry struct {
	status gateway.QuotaStatus
	at     time.Time
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
	return &dbUpstream{
		store:    panel.NewStoreWithBackend(back),
		pg:       back,
		secret:   secret,
		managers: map[string]*gateway.Manager{},
	}, nil
}

// dataTTL is how long the loaded panel state is reused before re-reading, so a
// new share or a removed one takes effect within a few seconds without a DB hit
// per request.
const dataTTL = 15 * time.Second

func (u *dbUpstream) snapshot() panel.Data {
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Since(u.dataAt) < dataTTL {
		return u.data
	}
	return u.reloadLocked()
}

// reloadLocked re-reads the panel now, ignoring the TTL. u.mu must be held.
func (u *dbUpstream) reloadLocked() panel.Data {
	if d, err := u.store.Load(); err == nil {
		u.data = d
		u.dataAt = time.Now()
	}
	return u.data
}

// Resolve maps a member key to the account's current access token.
//
// A key that is not in the cached snapshot triggers one fresh read before it is
// declared unknown, so a share created moments ago works on the first request
// rather than after the cache's TTL — the window that produced a spurious
// "access has been withdrawn" right after granting access.
func (u *dbUpstream) Resolve(memberKey string) (gateway.Resolution, error) {
	keyHash := panel.HashToken(memberKey)
	d := u.snapshot()
	share, found := d.ShareByKeyHash(keyHash)
	if !found {
		u.mu.Lock()
		d = u.reloadLocked()
		u.mu.Unlock()
		if share, found = d.ShareByKeyHash(keyHash); !found {
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
		u.recordCollision(acct.ID)
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
// manager adopts what the database holds now, or spends the refresh token. If
// neither works the login is dead — drop the cached manager so a re-added
// login is picked up, and record the collision for the panel to show.
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
		u.recordCollision(accountID)
		return "", err
	}
	log.Printf("gateway: renewed the login for account %s after Anthropic refused its token", accountID)
	return tok, nil
}

// Record meters one forwarded response. It prices the raw token counts here
// (weighted tokens + USD, via the model-weight table) so the gateway data plane
// stays free of pricing, then stores it. An unknown model is recorded under a
// visible "unknown:" label with no fabricated weight or cost. Best-effort: a
// metering failure is logged, never surfaced, and never blocks a request.
func (u *dbUpstream) Record(ev gateway.Event) {
	m := meter.Measure(ev.Model, meter.Usage{
		Input: ev.Input, Output: ev.Output,
		CacheCreation: ev.CacheCreation, CacheRead: ev.CacheRead,
	})
	model := ev.Model
	if !m.Known {
		model = "unknown:" + ev.Model
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := u.pg.RecordUsage(ctx, panelpg.UsageEvent{
		PersonID: ev.PersonID, AccountID: ev.AccountID, Model: model,
		Input: ev.Input, Output: ev.Output,
		CacheCreation: ev.CacheCreation, CacheRead: ev.CacheRead,
		Weighted: m.Weighted, CostUSD: m.CostUSD, RequestID: ev.RequestID,
	}); err != nil {
		log.Printf("metering: recording usage for account %s: %v", ev.AccountID, err)
	}
}

// RecordWindows stores an account's real 5h / weekly utilisation, read by the
// gateway off Anthropic's headers. Implements gateway.WindowRecorder, so the
// same dbUpstream that meters usage also captures the window snapshot.
// Nil models: a forwarded response's headers carry the two totals and nothing
// per-model, so this leaves whatever the usage poller last stored standing.
func (u *dbUpstream) RecordWindows(accountID string, w gateway.Windows) {
	u.pg.RecordWindows(accountID, w.FiveH, w.SevenD, w.FiveHReset, w.SevenDReset, nil)
}

// RecordWindowsAndModels stores a reading from the usage endpoint, which —
// unlike the headers — reports each model's own weekly allowance too.
func (u *dbUpstream) RecordWindowsAndModels(accountID string, w gateway.Windows, models []panel.ModelWindow) {
	u.pg.RecordWindows(accountID, w.FiveH, w.SevenD, w.FiveHReset, w.SevenDReset, models)
}

// Status reports a person's quota standing (over the cap, and how close), cached
// briefly so it costs at most one DB read per person per limitCacheTTL. It fails
// open: if the quota check itself errors, the member is served unconstrained — a
// metering hiccup must never lock the whole team out of a subscription they are
// entitled to.
func (u *dbUpstream) Status(personID, accountID string) gateway.QuotaStatus {
	// Keyed by both: the same person can be under different standings on different
	// accounts (an account cap applies to whoever is using that account).
	key := personID + "\x00" + accountID
	u.mu.Lock()
	if e, ok := u.limitCache[key]; ok && time.Since(e.at) < limitCacheTTL {
		u.mu.Unlock()
		return e.status
	}
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := u.pg.MemberLimitStatus(ctx, personID, accountID)
	if err != nil {
		return gateway.QuotaStatus{}
	}
	status := gateway.QuotaStatus{Over: st.Over, Fraction: st.Fraction, ResetAt: st.ResetAt, Message: st.Message}
	u.mu.Lock()
	if u.limitCache == nil {
		u.limitCache = map[string]limitCacheEntry{}
	}
	u.limitCache[key] = limitCacheEntry{status: status, at: time.Now()}
	u.mu.Unlock()
	return status
}

// recordCollision notes, best-effort, that an account's shared login just failed
// to refresh — the panel turns this into a "used outside the gateway" warning.
func (u *dbUpstream) recordCollision(accountID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := u.pg.RecordCollision(ctx, accountID,
		"the shared login failed to refresh — the account is likely being used first-party outside the gateway"); err != nil {
		log.Printf("health: recording collision for account %s: %v", accountID, err)
	}
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
		u.persist(accountID, fresh)
	})
	// Before spending its refresh token, the manager re-reads what the database
	// holds now — so a credential rotated out of process (a re-added login,
	// `clawdh-server diagnose` testing the refresh) is adopted, not fought.
	m.Reload(func() (gateway.Credential, bool) {
		d, err := u.store.Load()
		if err != nil {
			return gateway.Credential{}, false
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

// persist seals a rotated credential back into the account, so the single-use
// refresh token that just replaced the old one is not lost on restart.
func (u *dbUpstream) persist(accountID string, fresh gateway.Credential) {
	_ = u.store.Mutate(func(d *panel.Data) error {
		acct, ok := d.Account(accountID)
		if !ok {
			return nil
		}
		raw, err := u.secret.Open(acct.Credential)
		if err != nil {
			return nil
		}
		updated := updateCredential(raw, fresh)
		sealed, err := u.secret.Seal(updated)
		if err != nil {
			return nil
		}
		acct.Credential = sealed
		return nil
	})
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
