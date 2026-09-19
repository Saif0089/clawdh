package usage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sync"
	"time"

	"clawdh/internal/config"
)

// Snapshot is what the page shows for one account: whether the login
// still works, how much of the plan is used, and how long the login
// itself has left.
type Snapshot struct {
	// State is the account's live condition, decided here rather than
	// guessed at from the error text in the browser.
	State State `json:"state"`

	// Usage is nil when plan usage could not be read; Error then says
	// why, in words meant for the person reading the page.
	Usage *Report `json:"usage,omitempty"`
	Error string  `json:"error,omitempty"`
	// Note is a quiet remark about where the numbers came from — "read through
	// the gateway" — for a login whose usage isn't probed locally.
	Note string `json:"note,omitempty"`

	// Session describes the login rather than the plan.
	Session *SessionInfo `json:"session,omitempty"`
}

// State is what the dot next to an account's name means.
type State string

const (
	// StateLinked: this account will work if you run it now.
	StateLinked State = "linked"
	// StateSignedOut: nobody has signed this account in.
	StateSignedOut State = "signed-out"
	// StateExpired: there is a login, but it is no longer accepted.
	StateExpired State = "expired"
	// StateUnknown: clawdh could not tell — usually no network.
	StateUnknown State = "unknown"
)

// SessionInfo answers "how long am I signed in for?".
//
// There are two clocks, and conflating them is what makes the question
// confusing: the access token is short-lived and Claude Code refreshes
// it in the background without anyone noticing, while the refresh token
// is what actually ends the login when it expires.
type SessionInfo struct {
	AccessExpiresAt  *time.Time `json:"accessExpiresAt,omitempty"`
	SessionExpiresAt *time.Time `json:"sessionExpiresAt,omitempty"`
	Plan             string     `json:"plan,omitempty"`
	RateLimitTier    string     `json:"rateLimitTier,omitempty"`
}

// TTL is how long a fetched report is reused.
//
// The page polls every few seconds and never asks anyone to press a
// refresh button, so this is what decides how often that polling
// actually reaches Anthropic: a dozen page polls share one upstream
// call. This endpoint is not a documented API and publishes no
// rate-limit budget of any kind, and a shorter TTL here — four calls a
// minute per account — was enough to earn a 429 in an afternoon. One a
// minute per account is the rate a page left open all day can hold.
const TTL = time.Minute

// shortTTL is how long a snapshot is reused when Anthropic did not
// answer it: the login was read from disk and found wanting, a cooldown
// is being waited out, or the request never left this machine.
//
// None of those cost Anthropic anything to look at again, and each can
// change at any moment — Claude Code refreshes a token, a cooldown ends,
// the network comes back after a laptop wakes. Holding them for the
// whole TTL is what left cards a minute behind the thing that fixed them.
const shortTTL = 10 * time.Second

// expireFloor is how young a snapshot Expire leaves alone. Whatever
// calls Expire — switching accounts — can call it several times in a
// row, and every one of them honoured would be another upstream call.
const expireFloor = 15 * time.Second

// Rate-limit backoff. Nothing tells us what the limit is, so a refusal
// is answered by waiting longer each time rather than by guessing a
// safe rate: a minute, then two, four, and five from then on. One
// success clears it.
//
// Five minutes is already a fifth of the rate the TTL allows. Waiting
// longer than that protected nothing more, and left a card frozen for a
// quarter of an hour at a time.
const (
	backoffFirst = time.Minute
	backoffMax   = 5 * time.Minute
)

// cooldown is how long one account is not asking Anthropic anything.
type cooldown struct {
	until time.Time
	step  time.Duration
	// failedAt is when Anthropic last answered with neither numbers nor a
	// refusal after this cooldown ended. That answer is held for the
	// whole TTL, so the refusal that comes a minute after it is the same
	// spell going on, not a new one after a quiet spell.
	failedAt time.Time
}

type cacheEntry struct {
	snapshot Snapshot
	at       time.Time
	// ttl is how long this snapshot may be served: TTL for an answer
	// from Anthropic, shortTTL for anything else.
	ttl time.Duration
}

// load is one in-flight fetch for one account, which every request that
// arrives while it runs waits on instead of starting its own.
type load struct {
	done     chan struct{}
	snapshot Snapshot
}

// Service caches usage per account.
type Service struct {
	client *Client
	now    func() time.Time

	// CachePath overrides where the last good numbers are kept, for
	// tests. Empty means ~/.clawdh/usage.json.
	CachePath string

	// Gateway, when set, answers for a login the gateway holds: the panel
	// refreshes that login now, so its local token is stale by design and must
	// not be used or refreshed here. Given the account's config dir it returns
	// the gateway's own reading and a note for the page, or ok=false when this
	// login isn't one the gateway knows.
	Gateway func(configDir string) (report *Report, note string, ok bool)

	mu       sync.Mutex
	entries  map[string]cacheEntry
	inflight map[string]*load
	// cooldowns holds off accounts Anthropic has refused; lastReport
	// keeps the numbers they had when it did, because a rate limit is a
	// reason to stop asking, not a reason to blank a card that was
	// showing something true a minute ago.
	cooldowns  map[string]cooldown
	lastReport map[string]*Report
}

// NewService returns a Service using the default endpoint, keeping its
// last good numbers under ~/.clawdh so a restart does not lose them.
func NewService() *Service {
	s := NewServiceWithClient(NewClient())
	if path, err := config.UsageCacheFile(); err == nil {
		s.CachePath = path
		s.loadReports()
	}
	return s
}

// NewServiceWithClient returns a Service using a specific client, for
// tests.
func NewServiceWithClient(c *Client) *Service {
	// No CachePath: nothing is read from or written to disk. Only the
	// running service sets one, so a test constructing a Service can
	// never reach into the real ~/.clawdh — which it did, once.
	return &Service{
		client:     c,
		now:        time.Now,
		entries:    map[string]cacheEntry{},
		inflight:   map[string]*load{},
		cooldowns:  map[string]cooldown{},
		lastReport: map[string]*Report{},
	}
}

// Get returns the snapshot for one account, fetching it when the cached
// one has expired. accountID only names the cache slot; configDir is
// what identifies the account to Claude Code (empty for the default).
func (s *Service) Get(ctx context.Context, accountID, configDir string) Snapshot {
	s.mu.Lock()
	if entry, ok := s.entries[accountID]; ok && s.now().Sub(entry.at) < entry.ttl {
		s.mu.Unlock()
		return entry.snapshot
	}
	// One fetch per account at a time. The page polls every few seconds
	// and a second tab, or a slow answer from Anthropic, would otherwise
	// have every overlapping request start its own upstream call for the
	// same numbers.
	if pending, ok := s.inflight[accountID]; ok {
		s.mu.Unlock()
		select {
		case <-pending.done:
			return pending.snapshot
		case <-ctx.Done():
			return Snapshot{State: StateUnknown, Error: "Reading plan usage was interrupted."}
		}
	}
	pending := &load{done: make(chan struct{})}
	s.inflight[accountID] = pending
	s.mu.Unlock()

	snapshot, ttl := s.fetch(ctx, accountID, configDir)

	s.mu.Lock()
	// A request the browser gave up on (a reload, a closed tab) cancels
	// its context mid-fetch. That failure says nothing about the
	// account, and caching it would blame it for the next TTL.
	if ctx.Err() == nil {
		s.entries[accountID] = cacheEntry{snapshot: snapshot, at: s.now(), ttl: ttl}
	}
	delete(s.inflight, accountID)
	s.mu.Unlock()

	pending.snapshot = snapshot
	close(pending.done)
	return snapshot
}

// Forget drops an account's cached snapshot, after it is removed or
// reconnected.
func (s *Service) Forget(accountID string) {
	s.mu.Lock()
	delete(s.entries, accountID)
	// The saved numbers go too. Every caller means "what clawdh knows
	// about this account no longer applies" — it was removed, or signed
	// in as someone else — and numbers that outlived that would come
	// back under an account they were never about.
	delete(s.lastReport, accountID)
	delete(s.cooldowns, accountID)
	s.mu.Unlock()
	s.saveReports()
}

// Expire drops an account's cached snapshot so the next Get reads it
// again, for callers that know its numbers have just moved.
//
// It is not Forget. The saved numbers stay, and so does any cooldown:
// asking for newer numbers must never become a way to ask Anthropic
// sooner than a rate limit allows. A snapshot younger than expireFloor
// is kept as it is, and so is one still being read, which is younger
// still: letting an Expire void a read under way would let a caller that
// expires on every read turn every read into another call.
func (s *Service) Expire(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[accountID]; ok && s.now().Sub(entry.at) >= expireFloor {
		delete(s.entries, accountID)
	}
}

// waiting reports how long this account is still holding off, and the
// numbers it last managed to read.
func (s *Service) waiting(accountID string) (time.Time, *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := time.Time{}
	if c, ok := s.cooldowns[accountID]; ok && s.now().Before(c.until) {
		until = c.until
	}
	return until, s.lastGoodLocked(accountID)
}

// lastGood is the numbers this account last managed to read, for a card
// that has nothing newer to show.
func (s *Service) lastGood(accountID string) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastGoodLocked(accountID)
}

// lastGoodLocked is lastGood for a caller already holding s.mu.
//
// StaleReportAge is applied here and not only when usage.json is read:
// a server left running all day would otherwise keep serving numbers
// from memory long after their windows had rolled over.
func (s *Service) lastGoodLocked(accountID string) *Report {
	report := s.lastReport[accountID]
	if tooOld(report, s.now()) {
		return nil
	}
	return report
}

// refused starts or lengthens an account's cooldown after a rate limit,
// and returns when it may ask again.
func (s *Service) refused(accountID string, retryAfter time.Duration) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	prev := s.cooldowns[accountID]
	// When Anthropic could next have been asked: the end of the cooldown,
	// or, if it answered with a failure after that, the end of the TTL
	// that answer was held for. Counting from the cooldown alone made a
	// 500 in between look like a quiet spell — it holds the next call off
	// for longer than the first step — so a limit mixed with errors never
	// got past a minute's wait.
	free := prev.until
	if held := prev.failedAt.Add(TTL); held.After(free) {
		free = held
	}
	step := prev.step
	switch {
	// A refusal that comes more than a step after that starts a new
	// spell rather than continuing the old one — the page was closed, or
	// the limit lifted and came back. Carrying the old step over is what
	// sent a refusal hours after the last one straight to the longest
	// wait.
	case step <= 0, now.Sub(free) > prev.step:
		step = backoffFirst
	case step < backoffMax:
		step *= 2
	}
	if step > backoffMax {
		step = backoffMax
	}
	// A Retry-After worth waiting wins, but never shortens the backoff:
	// the server has already said no more than it is willing to say.
	if retryAfter > step {
		step = retryAfter
	}
	until := now.Add(step)
	s.cooldowns[accountID] = cooldown{until: until, step: step}
	return until
}

// allowed clears an account's cooldown after a successful read and
// keeps the numbers for the next time one is refused.
func (s *Service) allowed(accountID string, report *Report) {
	s.mu.Lock()
	delete(s.cooldowns, accountID)
	changed := false
	if report != nil {
		s.lastReport[accountID] = report
		changed = true
	}
	s.mu.Unlock()

	if changed {
		s.saveReports()
	}
}

// failed notes an answer from Anthropic that was neither numbers nor a
// refusal, for an account whose last refusal no success has cleared yet.
// refused counts the spell on from it.
func (s *Service) failed(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cooldowns[accountID]; ok {
		c.failedAt = s.now()
		s.cooldowns[accountID] = c
	}
}

// fetch reads one account's snapshot, and says how long it may be
// reused: TTL when Anthropic answered, shortTTL when it did not.
func (s *Service) fetch(ctx context.Context, accountID, configDir string) (Snapshot, time.Duration) {
	creds, err := ReadCredentials(configDir)
	switch {
	case errors.Is(err, errNoLogin), errors.Is(err, fs.ErrNotExist):
		return Snapshot{
			State: StateSignedOut,
			Error: "Not signed in yet. Use Connect to link a Claude login.",
		}, shortTTL
	case err != nil:
		return Snapshot{
			State: StateUnknown,
			Error: "Could not read this account's login: " + err.Error(),
		}, shortTTL
	}

	snapshot := Snapshot{State: StateLinked, Session: sessionInfo(creds)}

	// A refresh token that has run out is the one case where the login
	// really is over; the short-lived access token expiring is routine.
	if !creds.RefreshExpiresAt.IsZero() && s.now().After(creds.RefreshExpiresAt) {
		snapshot.State = StateExpired
		snapshot.Error = "This login has expired. Reconnect the account to use it again."
		return snapshot, shortTTL
	}

	// The stored access token is short-lived and only Claude Code
	// refreshes it — it does that when it runs, not on a timer. Between
	// runs the stored one goes stale while the login itself is fine, so
	// sending it would earn a 401 and make a working account read as
	// rejected. Say what is actually true instead: linked, numbers
	// pending the next run.
	//
	// That can last for hours, and the numbers read before the token went
	// stale are still the best answer there is, so they stay on the card
	// with their age beside them.
	if !creds.ExpiresAt.IsZero() && !s.now().Before(creds.ExpiresAt) {
		// A login the gateway holds: its token going stale here is expected, and
		// the gateway's reading is the real one. Never tell someone to "run it
		// once" — that refreshes the token locally and invalidates the gateway's.
		if s.Gateway != nil {
			if report, note, ok := s.Gateway(configDir); ok && report != nil {
				snapshot.Usage = report
				snapshot.Note = note
				return snapshot, shortTTL
			}
		}
		snapshot.Error = "Plan usage will show again once Claude Code refreshes this account's token — run it once."
		if last := s.lastGood(accountID); last != nil {
			snapshot.Usage = last
			snapshot.Error = readAt(last, s.now()) +
				"; they will update once Claude Code refreshes this account's token — run it once."
		}
		return snapshot, shortTTL
	}

	// Still serving a rate limit: say so, keep the last numbers on
	// screen, and ask nobody anything.
	if until, last := s.waiting(accountID); !until.IsZero() {
		snapshot.Usage = last
		snapshot.Error = pausedNote(until, last, s.now())
		return snapshot, shortTTL
	}

	report, err := s.client.Fetch(ctx, creds)
	if err != nil {
		// The session clock is still worth showing even when the plan
		// numbers aren't: it answers a different question.
		snapshot.Error = err.Error()

		var limited *RateLimited
		switch {
		case errors.Is(err, ErrLoginRejected):
			snapshot.State = StateExpired
		case errors.As(err, &limited):
			// A refusal for asking too often says the login is fine —
			// it was accepted and then throttled. Leave the account
			// linked, keep whatever numbers it last had, and wait.
			_, last := s.waiting(accountID)
			snapshot.Usage = last
			snapshot.Error = pausedNote(s.refused(accountID, limited.RetryAfter), last, s.now())
			// The cooldown just started is what decides when Anthropic
			// is asked next, so this snapshot need not outlast shortTTL.
			return snapshot, shortTTL
		default:
			// Reaching Anthropic failed, which says nothing about
			// whether this account works. Don't claim it is broken, and
			// don't blank numbers that were true a minute ago either.
			snapshot.State = StateUnknown
			if last := s.lastGood(accountID); last != nil {
				snapshot.Usage = last
				snapshot.Error = readAt(last, s.now()) + "; " + err.Error() + "."
			}
			if neverSent(err) {
				return snapshot, shortTTL
			}
		}
		// Anthropic answered, just not with numbers. Asking again any
		// sooner than after a success would be exactly the rate the TTL
		// exists to avoid: a rejected login or an outage is no reason to
		// call six times a minute.
		s.failed(accountID)
		return snapshot, TTL
	}
	s.allowed(accountID, report)
	snapshot.Usage = report
	return snapshot, TTL
}

// neverSent reports whether a fetch failed before its request left this
// machine: no network yet after waking, a name that did not resolve, a
// connection refused. Anthropic heard nothing, so asking again soon
// costs nothing against its limit.
func neverSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// pausedNote is what the card says while an account is waiting out a
// rate limit: what happened, how old the numbers under it are, and when
// clawdh will try again.
func pausedNote(until time.Time, last *Report, now time.Time) string {
	when := "There are no recent numbers to show"
	if last != nil && !last.FetchedAt.IsZero() {
		when = readAt(last, now)
	}
	return "Anthropic is rate-limiting plan usage. " + when +
		"; trying again at " + clock(until, now) + "."
}

// readAt says how old the numbers on a card are. A time of day alone was
// not enough to go on: read an hour later, or just after midnight, it
// looks like a moment ago.
func readAt(last *Report, now time.Time) string {
	return "These numbers are from " + clock(last.FetchedAt, now) + ", " + ago(last.FetchedAt, now)
}

// clock names a moment the way a card reads it: the time of day when it
// falls today, and the date as well when it does not.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// ago is how long before now t was, in minutes and hours: numbers any
// older than StaleReportAge are never shown, so no other unit is needed.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "less than a minute ago"
	}
	hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case hours == 0:
		return fmt.Sprintf("%d min ago", minutes)
	case minutes == 0:
		return fmt.Sprintf("%d h ago", hours)
	default:
		return fmt.Sprintf("%d h %d min ago", hours, minutes)
	}
}

func sessionInfo(creds Credentials) *SessionInfo {
	info := &SessionInfo{
		Plan:          creds.SubscriptionType,
		RateLimitTier: creds.RateLimitTier,
	}
	if !creds.ExpiresAt.IsZero() {
		t := creds.ExpiresAt.UTC()
		info.AccessExpiresAt = &t
	}
	if !creds.RefreshExpiresAt.IsZero() {
		t := creds.RefreshExpiresAt.UTC()
		info.SessionExpiresAt = &t
	}
	return info
}
