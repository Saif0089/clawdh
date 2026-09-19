package panelpg

import (
	"context"
	"os"
	"sync"
	"testing"

	"clawdh/panel"
)

// dsn is the test database, or the test is skipped. CI without a database, and
// a developer without one, both skip cleanly; the integration is proven wherever
// CLAWDH_TEST_POSTGRES points at a real Postgres.
func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("CLAWDH_TEST_POSTGRES")
	if d == "" {
		t.Skip("set CLAWDH_TEST_POSTGRES to a Postgres DSN to run the database-backed tests")
	}
	return d
}

func freshBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := Open(context.Background(), dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	// Start every test from an empty panel.
	if _, err := b.db.Exec(`UPDATE panel_state SET version = 0, data = '{}'::jsonb WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// seal is the identity sealer the store-level tests use; a real panel seals
// with its key, but the store does not care what the bytes are.
func seal(k string) []byte { return []byte(k) }

// The panel's own logic, unchanged, running over a real Postgres row: set up,
// add an account and a person, share it, and read it back.
func TestPanelRoundTripsThroughPostgres(t *testing.T) {
	b := freshBackend(t)
	store := panel.NewStoreWithBackend(b)

	account, alice := "", ""
	if err := store.Mutate(func(d *panel.Data) error {
		account = "acct-1"
		alice = "person-1"
		d.Accounts = append(d.Accounts, panel.Account{ID: account, Name: "Work"})
		d.People = append(d.People, panel.Person{ID: alice, Name: "Alice"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueShare(account, alice, "tester", seal); err != nil {
		t.Fatal(err)
	}

	// A second Store over the same database — a different serverless instance —
	// sees the share.
	other := panel.NewStoreWithBackend(mustReopen(t))
	d, err := other.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Shares) != 1 || d.Shares[0].PersonID != alice {
		t.Fatal("a second instance does not see the share the first made")
	}
}

// The real thing this had to buy: two instances sharing the same account at the
// same moment, and both landing without loss. Postgres settles the
// compare-and-swap, and shares are additive, so nothing is dropped.
func TestPostgresLandsConcurrentSharesWithoutLoss(t *testing.T) {
	b := freshBackend(t)
	setup := panel.NewStoreWithBackend(b)
	if err := setup.Mutate(func(d *panel.Data) error {
		d.Accounts = append(d.Accounts, panel.Account{ID: "acct", Name: "Work"})
		d.People = append(d.People,
			panel.Person{ID: "alice", Name: "Alice"},
			panel.Person{ID: "bob", Name: "Bob"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Two independent Stores, as two instances would be, racing to share the one
	// account with two different people at once.
	s1 := panel.NewStoreWithBackend(mustReopen(t))
	s2 := panel.NewStoreWithBackend(mustReopen(t))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	people := []string{"alice", "bob"}
	stores := []*panel.Store{s1, s2}
	wg.Add(2)
	for i := range stores {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = stores[i].IssueShare("acct", people[i], "tester", seal)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("share %d failed under concurrency: %v", i, e)
		}
	}

	d, _ := panel.NewStoreWithBackend(mustReopen(t)).Load()
	shared := map[string]bool{}
	for _, sh := range d.Shares {
		if sh.AccountID == "acct" {
			shared[sh.PersonID] = true
		}
	}
	if !shared["alice"] || !shared["bob"] {
		t.Fatalf("after the race both people should share the account, got %v", shared)
	}
}

var reopenDSN string

func mustReopen(t *testing.T) *Backend {
	t.Helper()
	if reopenDSN == "" {
		reopenDSN = dsn(t)
	}
	b, err := Open(context.Background(), reopenDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}
