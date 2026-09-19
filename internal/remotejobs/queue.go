// Package remotejobs holds a machine's incoming remote-help requests until its
// owner decides on them.
//
// Remote help is consent-based: a panel may ask an enrolled machine to look at
// itself, but a request that would ship anything off the machine — the list of
// its shared-account sessions, a transcript — is not run until the person there
// allows it, on their own clawdh page. So the panel sees "waiting for approval"
// until they do, "declined" if they don't, and a person is never surprised by
// what left their machine.
//
// Deciding every single request during a debugging session is tedious, so an
// owner can also allow requests for a while ("the next 30 minutes"), during
// which requests run on arrival — still announced, still logged here — and the
// window can be ended early. The queue persists to disk, so a service restart
// forgets nothing an owner hasn't answered.
package remotejobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Request is one thing the panel asked, held for a decision.
type Request struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Params      string    `json:"params,omitempty"`
	RequestedBy string    `json:"requestedBy,omitempty"` // who on the panel asked
	Describe    string    `json:"describe"`              // "send the transcript of session …"
	ReceivedAt  time.Time `json:"receivedAt"`
}

// Decision is one settled request, kept as recent history so the owner can see
// what was asked of their machine and what they did about it.
type Decision struct {
	Request
	Outcome   string    `json:"outcome"` // allowed | denied | auto (ran inside an allow window) | expired
	DecidedAt time.Time `json:"decidedAt"`
}

// State is the whole picture the page shows.
type State struct {
	Pending    []Request  `json:"pending"`
	TrustUntil *time.Time `json:"trustUntil,omitempty"` // requests run without asking until this time
	History    []Decision `json:"history"`
}

// maxHistory bounds the remembered decisions.
const maxHistory = 30

// MaxPendingAge is how long an unanswered request waits before it is declined
// on the owner's behalf, so the panel isn't left "waiting" for ever.
const MaxPendingAge = time.Hour

// Queue is the machine's held requests plus the hooks that run and report them.
// Run and Report are set by the service; nil hooks make Allow a no-op error, so
// the queue is safe to read before it is wired.
type Queue struct {
	mu   sync.Mutex
	path string

	pending    map[string]Request
	trustUntil time.Time
	history    []Decision

	// Run executes a request on this machine; Report posts the outcome to the
	// panel. Both are injected so this package needs to know nothing about either.
	Run    func(kind, params string) (result, status string)
	Report func(ctx context.Context, id, status, result string) error
	Now    func() time.Time
}

// Default is the service's queue.
var Default = New("")

// New makes a queue persisted at path ("" = memory only) and loads what was
// there.
func New(path string) *Queue {
	q := &Queue{path: path, pending: map[string]Request{}, Now: time.Now}
	q.load()
	return q
}

// SetPath points the queue at its state file and loads it.
func (q *Queue) SetPath(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.path = path
	q.load()
}

type persisted struct {
	Pending    []Request  `json:"pending"`
	TrustUntil time.Time  `json:"trustUntil"`
	History    []Decision `json:"history"`
}

func (q *Queue) load() {
	if q.path == "" {
		return
	}
	raw, err := os.ReadFile(q.path)
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	q.pending = map[string]Request{}
	for _, r := range p.Pending {
		q.pending[r.ID] = r
	}
	q.trustUntil = p.TrustUntil
	q.history = p.History
}

// save writes the state; best-effort (a failed save costs at most persistence
// across a restart, never a decision). Caller holds mu.
func (q *Queue) save() {
	if q.path == "" {
		return
	}
	p := persisted{TrustUntil: q.trustUntil, History: q.history}
	for _, r := range q.pending {
		p.Pending = append(p.Pending, r)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(q.path), 0o700)
	_ = os.WriteFile(q.path, raw, 0o600)
}

// Trusted reports whether requests currently run without asking.
func (q *Queue) Trusted() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.trustedLocked()
}

func (q *Queue) trustedLocked() bool {
	return !q.trustUntil.IsZero() && q.Now().Before(q.trustUntil)
}

// Receive takes a request the panel just handed over. Inside an allow window it
// runs at once (outcome "auto") and the result goes back; otherwise it is held
// and the panel is told it is awaiting approval. It reports whether it was held,
// so the caller can tell the owner.
func (q *Queue) Receive(ctx context.Context, r Request) (held bool) {
	q.mu.Lock()
	if r.ReceivedAt.IsZero() {
		r.ReceivedAt = q.Now()
	}
	if q.trustedLocked() {
		q.mu.Unlock()
		q.settle(ctx, r, "auto")
		return false
	}
	q.pending[r.ID] = r
	q.save()
	q.mu.Unlock()
	if q.Report != nil {
		_ = q.Report(ctx, r.ID, "awaiting", "")
	}
	return true
}

// Allow runs one held request and sends its answer back.
func (q *Queue) Allow(ctx context.Context, id string) error {
	r, ok := q.take(id)
	if !ok {
		return errors.New("that request is no longer waiting")
	}
	q.settle(ctx, r, "allowed")
	return nil
}

// Deny refuses one held request; the panel is told the owner declined.
func (q *Queue) Deny(ctx context.Context, id string) error {
	r, ok := q.take(id)
	if !ok {
		return errors.New("that request is no longer waiting")
	}
	if q.Report != nil {
		_ = q.Report(ctx, r.ID, "denied", "The owner of this machine declined this request.")
	}
	q.record(r, "denied")
	return nil
}

// Trust allows requests without asking for the next d, and runs everything
// currently held — the owner has just said yes to all of it.
func (q *Queue) Trust(ctx context.Context, d time.Duration) {
	q.mu.Lock()
	q.trustUntil = q.Now().Add(d)
	held := make([]Request, 0, len(q.pending))
	for _, r := range q.pending {
		held = append(held, r)
	}
	q.pending = map[string]Request{}
	q.save()
	q.mu.Unlock()
	sort.Slice(held, func(i, j int) bool { return held[i].ReceivedAt.Before(held[j].ReceivedAt) })
	for _, r := range held {
		q.settle(ctx, r, "allowed")
	}
}

// Untrust ends an allow window early; new requests ask again.
func (q *Queue) Untrust() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.trustUntil = time.Time{}
	q.save()
}

// Expire declines, on the owner's behalf, anything held longer than MaxPendingAge.
func (q *Queue) Expire(ctx context.Context) {
	q.mu.Lock()
	var old []Request
	for id, r := range q.pending {
		if q.Now().Sub(r.ReceivedAt) > MaxPendingAge {
			old = append(old, r)
			delete(q.pending, id)
		}
	}
	if len(old) > 0 {
		q.save()
	}
	q.mu.Unlock()
	for _, r := range old {
		if q.Report != nil {
			_ = q.Report(ctx, r.ID, "denied", "Nobody answered this request on the machine within an hour, so it was declined.")
		}
		q.record(r, "expired")
	}
}

// Snapshot is the state for the page: pending oldest-first, history newest-first.
func (q *Queue) Snapshot() State {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := State{Pending: make([]Request, 0, len(q.pending)), History: append([]Decision(nil), q.history...)}
	for _, r := range q.pending {
		st.Pending = append(st.Pending, r)
	}
	sort.Slice(st.Pending, func(i, j int) bool { return st.Pending[i].ReceivedAt.Before(st.Pending[j].ReceivedAt) })
	if q.trustedLocked() {
		t := q.trustUntil
		st.TrustUntil = &t
	}
	return st
}

func (q *Queue) take(id string) (Request, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.pending[id]
	if ok {
		delete(q.pending, id)
		q.save()
	}
	return r, ok
}

// settle runs a request and reports its answer, then records the outcome.
func (q *Queue) settle(ctx context.Context, r Request, outcome string) {
	result, status := "This machine can't run requests right now.", "error"
	if q.Run != nil {
		result, status = q.Run(r.Kind, r.Params)
	}
	if q.Report != nil {
		_ = q.Report(ctx, r.ID, status, result)
	}
	q.record(r, outcome)
}

func (q *Queue) record(r Request, outcome string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.history = append([]Decision{{Request: r, Outcome: outcome, DecidedAt: q.Now()}}, q.history...)
	if len(q.history) > maxHistory {
		q.history = q.history[:maxHistory]
	}
	q.save()
}
