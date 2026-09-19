package switching

import (
	"path/filepath"
	"testing"
)

// The shared-session ledger is created on first write, read back as id -> slug,
// keeps the latest entry for an id (a session switched between shared accounts
// reports the one it is on now), and treats a missing file as no sessions.
func TestSharedSessionLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "shared-sessions.tsv")

	if got := SharedSessions(path); len(got) != 0 {
		t.Errorf("missing ledger should read as empty, got %v", got)
	}
	for _, e := range [][2]string{{"s1", "ehtisham"}, {"s2", "work"}, {"s1", "work"}} {
		if err := RecordSharedSession(path, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	got := SharedSessions(path)
	if got["s1"] != "work" || got["s2"] != "work" || len(got) != 2 {
		t.Errorf("ledger = %v, want s1 (latest: work) and s2 (work)", got)
	}

	// Blank ids/slugs are ignored; tabs and newlines are refused so a line can't
	// be forged into two.
	if err := RecordSharedSession(path, "", "x"); err != nil {
		t.Errorf("blank id should be a no-op, got %v", err)
	}
	if err := RecordSharedSession(path, "bad\tid", "x"); err == nil {
		t.Error("a tab in the id should be refused")
	}
	if len(SharedSessions(path)) != 2 {
		t.Error("refused/ignored writes must not add entries")
	}
}
