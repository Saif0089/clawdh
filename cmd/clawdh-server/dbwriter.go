package main

import (
	"context"
	"log"
	"sync"
	"time"

	"clawdh/internal/gateway"
	"clawdh/internal/meter"
	"clawdh/panel"
	"clawdh/panelpg"
)

// Everything the gateway writes to the panel database — usage rows, window
// readings, collisions, and the credentials it rotates — goes through one
// background writer. A request hands its write over and moves on; it never
// waits for one.
//
// Two writes used to sit on every response's path: the window reading before
// the first byte went back, and the usage row before the stream was allowed to
// end. Against a database an ocean away that cost a few hundred milliseconds a
// response. Against an unreachable one it cost seconds, twice, on every call
// Claude Code made — which is what shared sessions felt like on 2026-09-28.

// maxPendingEvents bounds the usage rows held while the database cannot be
// written. Past it the oldest are dropped: metering is best-effort, a member's
// request never is.
const maxPendingEvents = 5000

// collisionNote is what the panel is told when a shared login fails to refresh.
const collisionNote = "the shared login failed to refresh — the account is likely being used first-party outside the gateway"

type pendingEvent struct {
	seq uint64
	ev  gateway.Event
}

type writer struct {
	mu         sync.Mutex
	creds      map[string]gateway.Credential // accountID -> newest rotation not yet stored
	windows    map[string]gateway.Windows    // accountID -> newest reading not yet stored
	collisions map[string]bool               // accountIDs with a collision to report
	events     []pendingEvent                // usage rows not yet stored, oldest first
	seq        uint64                        // sequence of the last event queued
	dropped    int                           // usage rows discarded since last reported
	kick       chan struct{}

	flushing sync.Mutex // one flush at a time, so no usage row is written twice
}

func newWriter() *writer {
	return &writer{
		creds:      map[string]gateway.Credential{},
		windows:    map[string]gateway.Windows{},
		collisions: map[string]bool{},
		kick:       make(chan struct{}, 1),
	}
}

func (w *writer) wake() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// credential queues a rotated credential. Only the newest per account matters:
// each rotation spends the one before it.
func (w *writer) credential(accountID string, c gateway.Credential) {
	w.mu.Lock()
	w.creds[accountID] = c
	w.mu.Unlock()
	w.wake()
}

// window queues an account's latest reading; a newer one replaces it unwritten.
func (w *writer) window(accountID string, win gateway.Windows) {
	w.mu.Lock()
	w.windows[accountID] = win
	w.mu.Unlock()
	w.wake()
}

func (w *writer) collision(accountID string) {
	w.mu.Lock()
	w.collisions[accountID] = true
	w.mu.Unlock()
	w.wake()
}

func (w *writer) event(ev gateway.Event) {
	w.mu.Lock()
	if len(w.events) >= maxPendingEvents {
		w.events = w.events[1:]
		w.dropped++
	}
	w.seq++
	w.events = append(w.events, pendingEvent{seq: w.seq, ev: ev})
	w.mu.Unlock()
	w.wake()
}

// pending reports how much is still waiting to be written, for log lines.
func (w *writer) pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.creds) + len(w.windows) + len(w.collisions) + len(w.events)
}

// runWriter drains the queue for as long as ctx lives: promptly while writes
// succeed, backing off while they fail so an unreachable database costs a
// failed attempt a minute rather than one per request.
func (u *dbUpstream) runWriter(ctx context.Context) {
	failures := 0
	for {
		if failures == 0 {
			select {
			case <-u.w.kick:
			case <-time.After(time.Minute):
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case <-time.After(backoff(failures)):
			case <-ctx.Done():
				return
			}
		}
		if err := u.flush(); err != nil {
			failures++
			if failures == 1 {
				log.Printf("gateway: database writes are failing; %d kept to retry in the background: %v", u.w.pending(), err)
			}
			continue
		}
		if failures > 0 {
			log.Printf("gateway: database writes work again after %d failed attempts", failures)
		}
		failures = 0
	}
}

// drain makes one last attempt to write what is queued, bounded by timeout. It
// is for shutdown: a deploy restarts the gateway, and a rotated credential
// still in the queue would otherwise be a login lost.
func (u *dbUpstream) drain(timeout time.Duration) {
	done := make(chan error, 1)
	go func() { done <- u.flush() }()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("gateway: shutting down with %d database writes unwritten: %v", u.w.pending(), err)
		}
	case <-time.After(timeout):
		log.Printf("gateway: shutting down with %d database writes still in flight", u.w.pending())
	}
}

// flush writes everything queued, credentials first — a rotation that never
// reaches the database is a login lost on the next restart. It stops at the
// first failure and leaves whatever was not written queued for next time.
func (u *dbUpstream) flush() error {
	w := u.w
	w.flushing.Lock()
	defer w.flushing.Unlock()

	w.mu.Lock()
	creds := make(map[string]gateway.Credential, len(w.creds))
	for id, c := range w.creds {
		creds[id] = c
	}
	windows := make(map[string]gateway.Windows, len(w.windows))
	for id, win := range w.windows {
		windows[id] = win
	}
	collisions := make([]string, 0, len(w.collisions))
	for id := range w.collisions {
		collisions = append(collisions, id)
	}
	events := append([]pendingEvent(nil), w.events...)
	dropped := w.dropped
	w.dropped = 0
	w.mu.Unlock()

	if dropped > 0 {
		log.Printf("metering: %d usage rows were dropped while the database could not be written", dropped)
	}

	for id, c := range creds {
		if err := u.storeCredential(id, c); err != nil {
			return err
		}
		w.mu.Lock()
		if cur, ok := w.creds[id]; ok && cur == c {
			delete(w.creds, id)
		}
		w.mu.Unlock()
	}
	for id, win := range windows {
		u.pg.RecordWindows(id, win.FiveH, win.SevenD, win.FiveHReset, win.SevenDReset, nil)
		w.mu.Lock()
		if cur, ok := w.windows[id]; ok && cur == win {
			delete(w.windows, id)
		}
		w.mu.Unlock()
	}
	for _, id := range collisions {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := u.pg.RecordCollision(ctx, id, collisionNote)
		cancel()
		if err != nil {
			return err
		}
		w.mu.Lock()
		delete(w.collisions, id)
		w.mu.Unlock()
	}
	var written uint64
	defer func() {
		// Drop what was stored from the front of the queue by sequence, not by
		// position: overflow may have trimmed the front while this was writing.
		w.mu.Lock()
		for len(w.events) > 0 && w.events[0].seq <= written {
			w.events = w.events[1:]
		}
		w.mu.Unlock()
	}()
	for _, pe := range events {
		if err := u.recordUsage(pe.ev); err != nil {
			return err
		}
		written = pe.seq
	}
	return nil
}

// storeCredential seals a rotated credential into its account — unless the
// database already holds a newer one (the login was re-added, or rotated
// elsewhere, after ours was issued), which an older one must never overwrite.
func (u *dbUpstream) storeCredential(accountID string, fresh gateway.Credential) error {
	return u.store.Mutate(func(d *panel.Data) error {
		acct, ok := d.Account(accountID)
		if !ok {
			return nil // the account was removed; there is nothing to keep
		}
		raw, err := u.secret.Open(acct.Credential)
		if err != nil {
			return nil
		}
		if _, _, stored := parseCredential(raw); stored.After(fresh.ExpiresAt) {
			return nil
		}
		sealed, err := u.secret.Seal(updateCredential(raw, fresh))
		if err != nil {
			return nil
		}
		acct.Credential = sealed
		return nil
	})
}

// recordUsage prices one forwarded response's raw token counts (weighted tokens
// + USD, via the model-weight table) so the gateway data plane stays free of
// pricing, then stores it. An unknown model is recorded under a visible
// "unknown:" label with no fabricated weight or cost.
func (u *dbUpstream) recordUsage(ev gateway.Event) error {
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
	return u.pg.RecordUsage(ctx, panelpg.UsageEvent{
		PersonID: ev.PersonID, AccountID: ev.AccountID, Model: model,
		Input: ev.Input, Output: ev.Output,
		CacheCreation: ev.CacheCreation, CacheRead: ev.CacheRead,
		Weighted: m.Weighted, CostUSD: m.CostUSD, RequestID: ev.RequestID,
	})
}

// backoff is how long to wait after n consecutive failures: 2s, doubling, to 1m.
func backoff(n int) time.Duration {
	d := 2 * time.Second
	for i := 1; i < n && d < time.Minute; i++ {
		d *= 2
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}
