package panel

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "panel.json"))
	s.now = func() time.Time { return clock }
	return s, &clock
}

// seed puts one account and two people in, and returns their ids.
func seed(t *testing.T, s *Store) (account, alice, bob string) {
	t.Helper()
	account, alice, bob = newID(), newID(), newID()
	err := s.Mutate(func(d *Data) error {
		d.Accounts = append(d.Accounts, Account{ID: account, Name: "Work"})
		d.People = append(d.People,
			Person{ID: alice, Name: "Alice"},
			Person{ID: bob, Name: "Bob"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// ensurePerson is how a pushed login attributes the machine that added it. It
// must reuse an existing member by name (case-insensitively) rather than pile up
// a new person on every push.
func TestEnsurePersonDedupesByNameCaseInsensitively(t *testing.T) {
	d := &Data{}
	now := time.Now()
	first := d.ensurePerson("Hassan", "", now).ID
	again := d.ensurePerson("hassan", "", now).ID // same name, different case
	if first != again {
		t.Errorf("ensurePerson made two members for one name: %s vs %s", first, again)
	}
	if len(d.People) != 1 {
		t.Errorf("People has %d entries, want 1", len(d.People))
	}
	if other := d.ensurePerson("Ibrahim", "", now).ID; other == first {
		t.Error("a different name should be a different member")
	}
	if len(d.People) != 2 {
		t.Errorf("People has %d entries, want 2", len(d.People))
	}
}

func TestAMissingFileIsAnEmptyPanel(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nothing-here.json"))
	d, err := s.Load()
	if err != nil {
		t.Fatalf("loading a panel that was never set up = %v, want no error", err)
	}
	if len(d.Accounts) != 0 || d.Admin != nil {
		t.Error("a machine nobody has set up should read as an empty panel")
	}
}

func loggedContaining(d Data, want string) bool {
	for _, e := range d.Activity {
		if strings.Contains(e.What, want) {
			return true
		}
	}
	return false
}

// The log is what answers "who could use this, and when". Every decision writes
// to it in the same transaction as the change itself, so a change that happened
// is a change that is recorded.
func TestEveryDecisionIsLogged(t *testing.T) {
	s, _ := newTestStore(t)
	account, alice, _ := seed(t, s)
	seal := func(k string) []byte { return []byte(k) }

	if _, err := s.IssueShare(account, alice, "tester", seal); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Load()
	var shareID string
	for _, sh := range d.Shares {
		shareID = sh.ID
	}
	if err := s.RevokeShare(shareID, "tester"); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Load()
	if !loggedContaining(d, "gave Alice access to Work") {
		t.Error("no log line for access being granted")
	}
	if !loggedContaining(d, "took Alice's access to Work away") {
		t.Error("no log line for access being taken away")
	}
}

// A share makes one account usable by a person through the gateway, and many
// shares can exist for one account at once — the gateway model. The gateway
// resolves a presented key by its hash.
func TestSharesAreManyPerAccountAndResolveByKey(t *testing.T) {
	s, _ := newTestStore(t)
	account, alice, bob := seed(t, s)
	if err := s.Mutate(func(d *Data) error {
		d.Accounts[0].Credential = []byte("sealed") // pretend it has a login
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	aliceKey, err := s.IssueShare(account, alice, "tester", func(k string) []byte { return []byte(k) })
	if err != nil {
		t.Fatal(err)
	}
	bobKey, err := s.IssueShare(account, bob, "tester", func(k string) []byte { return []byte(k) })
	if err != nil {
		t.Fatal(err)
	}
	if aliceKey == bobKey {
		t.Fatal("two people got the same gateway key")
	}

	d, _ := s.Load()
	// Both shares live at once — no one-holder limit.
	if len(d.Shares) != 2 {
		t.Fatalf("%d shares, want 2 (one account, two people, together)", len(d.Shares))
	}
	// The gateway resolves a key by its hash to the right account.
	sh, ok := d.ShareByKeyHash(HashToken(aliceKey))
	if !ok || sh.AccountID != account || sh.PersonID != alice {
		t.Error("Alice's key did not resolve to her share")
	}
	if _, ok := d.ShareByKeyHash(HashToken("not-a-key")); ok {
		t.Error("a bogus key resolved")
	}

	// Revoking Alice's share stops her key; Bob's still works.
	var aliceShareID string
	for _, x := range d.Shares {
		if x.PersonID == alice {
			aliceShareID = x.ID
		}
	}
	if err := s.RevokeShare(aliceShareID, "tester"); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Load()
	if _, ok := d.ShareByKeyHash(HashToken(aliceKey)); ok {
		t.Error("Alice's key still resolves after revoke")
	}
	if _, ok := d.ShareByKeyHash(HashToken(bobKey)); !ok {
		t.Error("Bob's key stopped working when Alice's was revoked")
	}
}

// Re-issuing a share to the same pair replaces the key rather than piling up a
// second one, so a lost key is rotated by simply sharing again.
func TestReissuingAShareRotatesTheKey(t *testing.T) {
	s, _ := newTestStore(t)
	account, alice, _ := seed(t, s)
	seal := func(k string) []byte { return []byte(k) }

	first, err := s.IssueShare(account, alice, "tester", seal)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.IssueShare(account, alice, "tester", seal)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("re-issuing gave back the same key")
	}
	d, _ := s.Load()
	if len(d.Shares) != 1 {
		t.Fatalf("%d shares for one pair, want 1 (the new key replaces the old)", len(d.Shares))
	}
	if _, ok := d.ShareByKeyHash(HashToken(first)); ok {
		t.Error("the old key still works after re-issuing")
	}
	if _, ok := d.ShareByKeyHash(HashToken(second)); !ok {
		t.Error("the new key does not work after re-issuing")
	}
}
