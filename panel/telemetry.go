package panel

import (
	"net/http"
	"time"
)

// A person's usage over time, at whatever grain they want to look at it.
//
// There is one source of truth, the gateway's metering, and two ways to read it
// so that no view costs more than it has to. The fine grains (seconds, minutes)
// are cut from the raw events, which are exact to the request but are only ever
// asked for a few minutes or hours back. Everything from an hour up is summed
// from the hourly roll-up the gateway keeps in the same transaction as each
// event, so the two cannot disagree, and a month of history is a few hundred
// rows rather than a few hundred thousand.
//
// Nothing here estimates. A bucket is the weighted tokens the gateway metered in
// it, and a bucket with nothing in it is simply not returned.

// Resolution is how finely a person's usage is cut.
type Resolution struct {
	Name string
	// Unit is the Postgres date_trunc unit a bucket is cut to. It is never taken
	// from a request, only from this table.
	Unit string
	// Bucket is a bucket's length; zero for a month, which has none.
	Bucket time.Duration
	// Span is how far back the view reaches; zero means all of the history.
	Span time.Duration
	// Raw reads the events themselves (exact, with a request count) instead of
	// the hourly roll-up, which holds no request counts and nothing finer than an
	// hour.
	Raw bool
}

// Resolutions are the grains the boards offer, finest first.
var Resolutions = []Resolution{
	{Name: "second", Unit: "second", Bucket: time.Second, Span: 5 * time.Minute, Raw: true},
	{Name: "minute", Unit: "minute", Bucket: time.Minute, Span: 2 * time.Hour, Raw: true},
	{Name: "hour", Unit: "hour", Bucket: time.Hour, Span: 72 * time.Hour},
	{Name: "day", Unit: "day", Bucket: 24 * time.Hour, Span: 30 * 24 * time.Hour},
	{Name: "week", Unit: "week", Bucket: 7 * 24 * time.Hour, Span: 26 * 7 * 24 * time.Hour},
	{Name: "month", Unit: "month", Span: 730 * 24 * time.Hour},
	{Name: "all", Unit: "month"},
}

// ResolutionByName finds a grain by the name a request gave.
func ResolutionByName(name string) (Resolution, bool) {
	for _, r := range Resolutions {
		if r.Name == name {
			return r, true
		}
	}
	return Resolution{}, false
}

// SeriesPoint is one bucket of a person's usage.
type SeriesPoint struct {
	// T is the start of the bucket, UTC.
	T time.Time `json:"t"`
	// Weighted is the bucket's weighted tokens (see meter.Measure), the unit
	// every figure on the boards is in; Tokens is the raw count they came from,
	// every class added up.
	Weighted float64 `json:"weighted"`
	Tokens   int64   `json:"tokens"`
	// Requests is how many requests made it; present only where the events are
	// read (seconds and minutes) — the hourly roll-up does not count them.
	Requests int64 `json:"requests,omitempty"`
	// The bucket split by token class. Each class's raw tokens add up to Tokens
	// and its weighted share to Weighted, so a board can show any one class, or
	// all of them stacked, without a figure that does not reconcile.
	Input      ClassUsage `json:"input"`
	Output     ClassUsage `json:"output"`
	CacheWrite ClassUsage `json:"cacheWrite"`
	CacheRead  ClassUsage `json:"cacheRead"`
}

// ClassUsage is one token class's part of a bucket.
type ClassUsage struct {
	Tokens   int64   `json:"tokens"`
	Weighted float64 `json:"weighted"`
}

// SessionUsage is one Claude Code session's usage: when it began and last ran,
// and what it came to, split by class. The session is named by its id only; its
// title lives on the machine that ran it, which is the only place it is shown.
type SessionUsage struct {
	ID    string    `json:"id"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// Usage is what the session came to; its T is when it last ran.
	Usage SeriesPoint `json:"usage"`
}

// Telemetry is one person's series (or everyone's, when Person is empty).
type Telemetry struct {
	Res     string `json:"res"`
	Person  string `json:"person,omitempty"`
	Session string `json:"session,omitempty"`
	// BucketSeconds is a bucket's length, 0 for months. From and Until bound the
	// span the view covers, so a bucket with nothing in it can be drawn as the
	// gap it is.
	BucketSeconds int           `json:"bucketSeconds"`
	From          time.Time     `json:"from"`
	Until         time.Time     `json:"until"`
	Points        []SeriesPoint `json:"points"`
}

// truncate cuts t to the start of its bucket the way Postgres's date_trunc does
// in UTC: weeks begin on Monday.
func (r Resolution) truncate(t time.Time) time.Time {
	t = t.UTC()
	switch r.Unit {
	case "week":
		day := t.Truncate(24 * time.Hour)
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return t.Truncate(r.Bucket)
	}
}

// from is where a view starts: its span back from now, cut to a bucket — or, for
// an incremental read, the bucket a client already holds the end of, so the
// newest (still filling) bucket is refreshed and nothing older is sent again.
func (r Resolution) from(now, since time.Time) time.Time {
	var start time.Time
	if r.Span > 0 {
		start = r.truncate(now.Add(-r.Span))
	}
	if !since.IsZero() {
		if s := r.truncate(since); s.After(start) {
			start = s
		}
	}
	return start
}

// readTelemetry answers a series request for a person ("" is everyone).
func (s *Server) readTelemetry(w http.ResponseWriter, r *http.Request, personID string) {
	res, ok := ResolutionByName(r.URL.Query().Get("res"))
	if !ok {
		fail(w, http.StatusBadRequest, "That is not a grain the board has. Use second, minute, hour, day, week, month or all.")
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(w, http.StatusBadRequest, "since is a time like 2026-10-27T12:00:00Z.")
			return
		}
		since = t
	}
	session := r.URL.Query().Get("session")
	if session != "" && personID == "" {
		fail(w, http.StatusBadRequest, "A session belongs to one person; name the person too.")
		return
	}
	now := s.now().UTC()
	from := res.from(now, since)
	if session != "" {
		// A session is read over its own span, however long ago it ran: it is
		// bounded by itself, not by how far back the grain's view reaches.
		from = time.Time{}
		if !since.IsZero() {
			from = res.truncate(since)
		}
	}
	points, err := s.usage.PersonSeries(r.Context(), personID, session, res, from)
	if err != nil {
		fail(w, http.StatusInternalServerError, "The usage could not be read: "+err.Error())
		return
	}
	if points == nil {
		points = []SeriesPoint{}
	}
	out := Telemetry{
		Res: res.Name, Person: personID, Session: session, BucketSeconds: int(res.Bucket / time.Second),
		From: from, Until: now, Points: points,
	}
	if res.Span == 0 && len(points) > 0 && since.IsZero() {
		out.From = points[0].T // all of it begins where the metering does
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTelemetry is the admin's read of one person's usage over time, or
// everyone's when no person is named.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	personID := r.URL.Query().Get("person")
	if personID != "" {
		d, _ := s.store.Load()
		if _, ok := d.Person(personID); !ok {
			fail(w, http.StatusNotFound, "There is no such person.")
			return
		}
	}
	s.readTelemetry(w, r, personID)
}

// handleOwnTelemetry is a machine's read of its own person's usage, and only
// that: the person is the enrolled machine's, never a parameter.
func (s *Server) handleOwnTelemetry(w http.ResponseWriter, r *http.Request, dev Device) {
	s.readTelemetry(w, r, dev.PersonID)
}

// sessionsReach is how far back a sessions list looks, and sessionsLimit how many
// sessions it names at most.
const (
	sessionsReach = 30 * 24 * time.Hour
	sessionsLimit = 100
)

// handleSessions is the admin's read of one person's sessions, newest first, by
// id: the panel never sees a title, so the board names a session by its id.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	personID := r.URL.Query().Get("person")
	if personID == "" {
		fail(w, http.StatusBadRequest, "Sessions belong to one person; name the person.")
		return
	}
	d, _ := s.store.Load()
	if _, ok := d.Person(personID); !ok {
		fail(w, http.StatusNotFound, "There is no such person.")
		return
	}
	s.writeSessions(w, r, personID)
}

// handleOwnSessions is a machine's read of its own person's sessions, newest
// first: what each Claude Code session came to, by id. The machine puts titles
// on them; the panel never sees one.
func (s *Server) handleOwnSessions(w http.ResponseWriter, r *http.Request, dev Device) {
	s.writeSessions(w, r, dev.PersonID)
}

// writeSessions answers with a person's sessions from the last sessionsReach.
func (s *Server) writeSessions(w http.ResponseWriter, r *http.Request, personID string) {
	list, err := s.usage.PersonSessions(r.Context(), personID, s.now().Add(-sessionsReach), sessionsLimit)
	if err != nil {
		fail(w, http.StatusInternalServerError, "The sessions could not be read: "+err.Error())
		return
	}
	if list == nil {
		list = []SessionUsage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}
