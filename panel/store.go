package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// maxActivity caps the activity log. It is the only unbounded thing here, and
// a check-in every thirty seconds per machine would otherwise grow the file
// for ever. Check-ins are not logged for that reason; only decisions are.
const maxActivity = 2000

// Data is the whole panel, as it sits on disk.
//
// It is one JSON file rather than a database on purpose. This holds tens of
// rows, is written by exactly one process, and clawdh already keeps its accounts
// this way (internal/accounts/store.go); a SQL engine would be a megabytes-long
// dependency and a second way of doing the same thing. The invariants a
// database would enforce with a unique index are enforced here by holding the
// mutex across read-decide-write, which is the same guarantee for one writer.
type Data struct {
	Admin     *Admin     `json:"admin,omitempty"`
	People    []Person   `json:"people,omitempty"`
	Devices   []Device   `json:"devices,omitempty"`
	Accounts  []Account  `json:"accounts,omitempty"`
	JoinCodes []JoinCode `json:"joinCodes,omitempty"`
	Shares    []Share    `json:"shares,omitempty"`
	Activity  []Event    `json:"activity,omitempty"`
}

// Store is the panel's persistence. Every mutation takes the lock, re-reads the
// current state, decides, and writes it back — atomically for a file, and with
// a compare-and-swap for a shared database, so two writers can never both
// believe they won.
//
// On a single machine the mutex makes read-decide-write indivisible. On several
// machines behind one database — a panel deployed to a serverless host, where
// each request may be a fresh process — the mutex cannot, so the write is
// conditional on nothing having changed since the read, and a lost race simply
// runs the decision again against the winner's result.
type Store struct {
	mu      sync.Mutex
	backend Backend
	now     func() time.Time
}

// Backend is where a Store keeps its one blob of state. Load returns the
// current bytes and a version; Save writes new bytes only if the version has
// not moved, reporting whether it won. A brand-new store loads as (nil, 0).
//
// It is exported so a Postgres implementation can live in its own package,
// keeping that driver out of the clawdh client binary that ships to every machine.
type Backend interface {
	Load() (raw []byte, version int64, err error)
	Save(raw []byte, expected int64) (ok bool, err error)
}

// NewStore returns a Store kept in one JSON file, which need not exist yet.
// This is what `clawdh panel serve` uses: one machine, one writer.
func NewStore(path string) *Store {
	return &Store{backend: &fileBackend{path: path}, now: time.Now}
}

// NewStoreWithBackend returns a Store over any backend — a database, for a
// panel that runs as more than one process at once.
func NewStoreWithBackend(b Backend) *Store {
	return &Store{backend: b, now: time.Now}
}

// maxCASRetries bounds how many times a lost compare-and-swap is retried before
// giving up. Contention is a handful of admin clicks and a check-in every
// thirty seconds per machine, so a real collision is rare and clears in one
// retry; this only stops a pathological livelock.
const maxCASRetries = 8

// Load reads the panel. A missing file is an empty panel, not an error: that is
// what a machine looks like before anyone has set it up.
func (s *Store) Load() (Data, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (Data, error) {
	d, _, err := s.readLocked()
	return d, err
}

// readLocked returns the current panel and the version to write back against.
func (s *Store) readLocked() (Data, int64, error) {
	var d Data
	raw, version, err := s.backend.Load()
	if err != nil {
		return d, 0, err
	}
	if len(raw) == 0 {
		return d, version, nil // never set up yet
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, 0, fmt.Errorf("reading the panel state: %w", err)
	}
	return d, version, nil
}

// Mutate runs fn against the current panel and writes the result. fn returning
// an error writes nothing.
func (s *Store) Mutate(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for attempt := 0; ; attempt++ {
		d, version, err := s.readLocked()
		if err != nil {
			return err
		}
		d.expireShares(s.now())
		if err := fn(&d); err != nil {
			return err
		}
		ok, err := s.saveVersioned(&d, version)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// Someone else wrote between our read and our write. Their change is
		// now the truth, so run the whole decision again against it.
		if attempt >= maxCASRetries {
			return errors.New("the panel is being changed by too many people at once; try again")
		}
	}
}

func (s *Store) saveVersioned(d *Data, version int64) (bool, error) {
	if len(d.Activity) > maxActivity {
		d.Activity = d.Activity[len(d.Activity)-maxActivity:]
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return false, err
	}
	return s.backend.Save(raw, version)
}

// ---------------------------------------------------------------- lookups

func (d *Data) accountName(id string) string {
	for _, a := range d.Accounts {
		if a.ID == id {
			return a.Name
		}
	}
	return "an account that no longer exists"
}

func (d *Data) personName(id string) string {
	for _, p := range d.People {
		if p.ID == id {
			return p.Name
		}
	}
	return "someone who is no longer here"
}

// Account returns an account by id.
func (d *Data) Account(id string) (*Account, bool) {
	for i := range d.Accounts {
		if d.Accounts[i].ID == id {
			return &d.Accounts[i], true
		}
	}
	return nil, false
}

// Person returns a person by id.
func (d *Data) Person(id string) (*Person, bool) {
	for i := range d.People {
		if d.People[i].ID == id {
			return &d.People[i], true
		}
	}
	return nil, false
}

// Device returns a device by id.
func (d *Data) Device(id string) (*Device, bool) {
	for i := range d.Devices {
		if d.Devices[i].ID == id {
			return &d.Devices[i], true
		}
	}
	return nil, false
}

// personByName finds a person by name, case-insensitively. Names are how a
// pushed login attributes the machine that added it, and one small team's
// names are unique enough to key on.
func (d *Data) personByName(name string) (*Person, bool) {
	for i := range d.People {
		if strings.EqualFold(d.People[i].Name, name) {
			return &d.People[i], true
		}
	}
	return nil, false
}

// ensurePerson returns the person with this name, creating one if there is
// none. It is how the machine that pushes a login gets a member record without
// the admin having to add them by hand first.
func (d *Data) ensurePerson(name, email string, now time.Time) *Person {
	if p, ok := d.personByName(name); ok {
		return p
	}
	d.People = append(d.People, Person{ID: newID(), Name: name, Email: email, CreatedAt: now})
	return &d.People[len(d.People)-1]
}

// shareFor returns a person's existing share on an account, if any.
func (d *Data) shareFor(accountID, personID string) (Share, bool) {
	for _, sh := range d.Shares {
		if sh.AccountID == accountID && sh.PersonID == personID {
			return sh, true
		}
	}
	return Share{}, false
}

// putShare records a share, replacing any existing key for the same
// account+person pair — re-issuing is how a lost key is rotated.
func (d *Data) putShare(sh Share) {
	out := d.Shares[:0]
	for _, existing := range d.Shares {
		if !(existing.AccountID == sh.AccountID && existing.PersonID == sh.PersonID) {
			out = append(out, existing)
		}
	}
	d.Shares = append(out, sh)
}

// ShareByKeyHash returns the share a gateway key belongs to, for the gateway to
// resolve a member key to an account. A key with no live share is unknown, which
// the gateway turns into a 401 — instant revocation.
func (d *Data) ShareByKeyHash(keyHash string) (Share, bool) {
	return d.ShareByKeyHashAt(keyHash, time.Now())
}

// ShareByKeyHashAt is ShareByKeyHash against a given clock: a share whose
// deadline has passed is not found, so its key stops working at the deadline
// itself, before any sweep.
func (d *Data) ShareByKeyHashAt(keyHash string, now time.Time) (Share, bool) {
	for _, sh := range d.Shares {
		if sh.KeyHash == keyHash && sh.Live(now) {
			return sh, true
		}
	}
	return Share{}, false
}

// expireShares removes every share whose time has run out and writes a line
// for each, so the history says access ended and why. Mutate runs it before
// every change, and the panel's reads go through Mutate, so a deadline is
// settled within seconds of passing (and the gateway stops honouring the
// share at the deadline itself — see Share.Live).
func (d *Data) expireShares(now time.Time) {
	kept := d.Shares[:0]
	for _, sh := range d.Shares {
		if sh.Live(now) {
			kept = append(kept, sh)
			continue
		}
		by := "the time it was given for ran out"
		if sh.GrantedBy != "" {
			by = "the time " + sh.GrantedBy + " gave it for ran out"
		}
		d.log(now, "clawdh", fmt.Sprintf("ended %s's access to %s — %s", d.personName(sh.PersonID), d.accountName(sh.AccountID), by))
	}
	d.Shares = kept
}

// log appends one line of activity.
func (d *Data) log(at time.Time, who, what string) {
	d.Activity = append(d.Activity, Event{At: at, Who: who, What: what})
}

// Log appends one line of activity. Callers outside this package use it so the
// log is written in the same transaction as the change it describes.
func (d *Data) Log(at time.Time, who, what string) { d.log(at, who, what) }

// ---------------------------------------------------------------- decisions

// IssueShare makes an account available to a person through the gateway and
// returns the gateway key to hand out (stored only as a hash). Issuing it again
// for the same pair replaces the key, so a lost key is rotated by re-issuing.
// IssueShare makes an account available to a person through the gateway and
// returns the gateway key. sealKey seals it for later delivery to the person's
// device (the caller supplies it because only it holds the panel key).
func (s *Store) IssueShare(accountID, personID, who string, sealKey func(string) []byte) (key string, err error) {
	return s.IssueShareFor(accountID, personID, who, 0, sealKey)
}

// IssueShareFor is IssueShare with a deadline: after d the access ends on its
// own and the history says so. A zero d is access until revoked.
func (s *Store) IssueShareFor(accountID, personID, who string, d time.Duration, sealKey func(string) []byte) (key string, err error) {
	key, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	sealed := sealKey(key)
	err = s.Mutate(func(data *Data) error {
		if _, ok := data.Account(accountID); !ok {
			return fmt.Errorf("no account with id %q", accountID)
		}
		if _, ok := data.Person(personID); !ok {
			return fmt.Errorf("no person with id %q", personID)
		}
		now := s.now()
		sh := Share{ID: newID(), AccountID: accountID, PersonID: personID, KeyHash: hash, SealedKey: sealed, CreatedAt: now, GrantedBy: who}
		what := fmt.Sprintf("gave %s access to %s", data.personName(personID), data.accountName(accountID))
		if d > 0 {
			sh.ExpiresAt = now.Add(d)
			what += " for " + humanDuration(d)
		}
		data.putShare(sh)
		data.Log(now, who, what)
		return nil
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

// RevokeShare withdraws a person's gateway access to an account; their key stops
// working at the gateway on the next request.
func (s *Store) RevokeShare(shareID, who string) error {
	return s.Mutate(func(d *Data) error {
		out := d.Shares[:0]
		var removed *Share
		for i := range d.Shares {
			if d.Shares[i].ID == shareID {
				removed = &d.Shares[i]
				continue
			}
			out = append(out, d.Shares[i])
		}
		if removed == nil {
			return nil
		}
		d.Shares = out
		d.Log(s.now(), who, fmt.Sprintf("took %s's access to %s away", d.personName(removed.PersonID), d.accountName(removed.AccountID)))
		return nil
	})
}

// newID is a short random identifier. These are never guessed at by anyone —
// they are handed out by the panel — so eight bytes is plenty.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a time-based fallback
		// would be a silently weaker id. Fail loudly instead.
		panic("panel: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// jsonMarshal is exposed to tests that need to build a raw backend blob.
func jsonMarshal(d Data) ([]byte, error) { return json.MarshalIndent(d, "", "  ") }

// humanDuration is a duration in the words a person would use: "8 hours",
// "a day", "3 days", "a week".
func humanDuration(d time.Duration) string {
	switch h := int(d.Round(time.Hour).Hours()); {
	case h < 1:
		return fmt.Sprintf("%d minutes", int(d.Round(time.Minute).Minutes()))
	case h == 1:
		return "an hour"
	case h < 24:
		return fmt.Sprintf("%d hours", h)
	case h == 24:
		return "a day"
	case h < 24*7:
		return fmt.Sprintf("%d days", h/24)
	case h == 24*7:
		return "a week"
	default:
		return fmt.Sprintf("%d weeks", h/(24*7))
	}
}
