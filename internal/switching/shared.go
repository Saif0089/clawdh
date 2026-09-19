package switching

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The shared-session ledger: which conversations on this machine ran through a
// clawdh-shared account, and which one. The supervisor appends to it whenever a
// session launches on, or switches to, a shared account.
//
// It is the privacy boundary for remote help. The panel may only ever list or be
// sent sessions that are in this ledger — sessions that ran on an account the
// panel itself lent out. Everything else on the machine (a person's own login,
// a local account they manage themselves, and every session of either) is not
// the panel's concern and is never visible to it.
//
// Append-only TSV — session id, share slug, epoch seconds — so a crash mid-write
// can at worst lose the last line, never corrupt earlier ones.

// RecordSharedSession notes that sessionID is running on the shared account
// slug. Unlike the usage monitor's ledger, this is clawdh's own file, so it is
// created if missing.
func RecordSharedSession(path, sessionID, slug string) error {
	if path == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(slug) == "" {
		return nil
	}
	if strings.ContainsAny(sessionID+slug, "\t\n") {
		return fmt.Errorf("refusing to write a session id or slug containing a tab or newline")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\t%d\n", sessionID, slug, time.Now().Unix())
	return err
}

// SharedSessions reads the ledger into session id -> share slug (the latest
// entry for an id wins, so a session switched between shared accounts reports
// the one it is on now). A missing ledger is simply no shared sessions.
func SharedSessions(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}
