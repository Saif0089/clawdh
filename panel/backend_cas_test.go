package panel

import (
	"sync"
	"testing"
	"time"
)

// racingBackend is a fake shared database: an in-memory blob with a version,
// and a hook that fires once, the first time a save is attempted, to simulate
// another instance winning the race in between this instance's read and write.
type racingBackend struct {
	mu      sync.Mutex
	raw     []byte
	version int64
	onFirst func()
	fired   bool
}

func (b *racingBackend) Load() ([]byte, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.raw, b.version, nil
}

func (b *racingBackend) Save(raw []byte, expected int64) (bool, error) {
	if b.onFirst != nil && !b.fired {
		b.fired = true
		b.onFirst() // another writer lands here, moving the version
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.version != expected {
		return false, nil // lost the race; caller must re-read and retry
	}
	b.raw = append([]byte(nil), raw...)
	b.version++
	return true, nil
}

func (b *racingBackend) commit(raw []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.raw = append([]byte(nil), raw...)
	b.version++
}

// A serverless panel is several processes over one database. Two admins who
// share the same account at the same moment must both land — the second write
// is refused by the version check, its decision re-runs against the first, and
// both shares survive. Shares are additive (that is the gateway: many people,
// one login), so nothing is lost. This drives the retry path deterministically.
func TestConcurrentShareIssueDuringAWriteStillLands(t *testing.T) {
	back := &racingBackend{}
	s := NewStoreWithBackend(back)
	clock := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	account, alice, bob := seed(t, s)
	seal := func(k string) []byte { return []byte(k) }

	// Mid-save, a concurrent instance shares the account with Bob and commits
	// first. Alice's IssueShare must lose the race, re-run against Bob's state,
	// and land — leaving both shares.
	back.onFirst = func() {
		d, _, _ := s.readLocked()
		_, hash, _ := NewToken()
		d.Shares = append(d.Shares, Share{ID: newID(), AccountID: account, PersonID: bob, KeyHash: hash, CreatedAt: clock})
		back.commit(mustMarshal(t, d))
	}

	if _, err := s.IssueShare(account, alice, "tester", seal); err != nil {
		t.Fatalf("Alice's share was lost to a concurrent write: %v", err)
	}

	d, _ := s.Load()
	people := map[string]bool{}
	for _, sh := range d.Shares {
		if sh.AccountID == account {
			people[sh.PersonID] = true
		}
	}
	if !people[alice] || !people[bob] {
		t.Fatalf("after the race both Alice and Bob should share the account, got %v", people)
	}
}

// A lost race that does NOT hit the invariant just retries and succeeds: adding
// two different people at once must not lose one.
func TestConcurrentUnrelatedWritesBothLand(t *testing.T) {
	back := &racingBackend{}
	s := NewStoreWithBackend(back)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	// First add lands normally.
	if err := s.Mutate(func(d *Data) error {
		d.People = append(d.People, Person{ID: newID(), Name: "Alice"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Second add races once against a concurrent "Carol was added", then retries.
	back.onFirst = func() {
		d, _, _ := s.readLocked()
		d.People = append(d.People, Person{ID: newID(), Name: "Carol"})
		back.commit(mustMarshal(t, d))
	}
	if err := s.Mutate(func(d *Data) error {
		d.People = append(d.People, Person{ID: newID(), Name: "Bob"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	d, _ := s.Load()
	names := map[string]bool{}
	for _, p := range d.People {
		names[p.Name] = true
	}
	for _, want := range []string{"Alice", "Bob", "Carol"} {
		if !names[want] {
			t.Errorf("%s was lost to a race; have %v", want, names)
		}
	}
}

func mustMarshal(t *testing.T, d Data) []byte {
	t.Helper()
	raw, err := jsonMarshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
