package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawdh/internal/switching"
	"clawdh/panel"
)

// plantSession writes a fake transcript for a session id under the (overridden)
// Claude data dir, in the given project folder.
func plantSession(t *testing.T, root, project, id, body string) {
	t.Helper()
	dir := filepath.Join(root, "projects", project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Remote help may only ever see sessions that ran through a shared account —
// the ones in clawdh's shared-session ledger. A person's own sessions on the
// same machine (their personal login, a local account they manage themselves)
// are never listed and never sent, no matter what the panel asks for.
func TestRemoteJobsOnlyExposeSharedSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".claude")
	claudeRootOverride = root
	t.Cleanup(func() { claudeRootOverride = "" })
	ledger := filepath.Join(t.TempDir(), "shared-sessions.tsv")
	sharedLedgerOverride = ledger
	t.Cleanup(func() { sharedLedgerOverride = "" })

	// Two sessions on disk: one ran through a shared account, one is personal.
	plantSession(t, root, "proj-a", "shared-1", `{"type":"user","text":"work on the shared account"}`)
	plantSession(t, root, "proj-b", "personal-1", `{"type":"user","text":"ahmed's private session"}`)
	if err := switching.RecordSharedSession(ledger, "shared-1", "ehtisham"); err != nil {
		t.Fatal(err)
	}

	// sessions: only the shared one, tagged with the account it ran on.
	var out struct {
		Sessions []struct{ ID, Share, Project string }
		Note     string
	}
	if err := json.Unmarshal([]byte(jobSessions()), &out); err != nil {
		t.Fatalf("sessions answer is not JSON: %v", err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].ID != "shared-1" || out.Sessions[0].Share != "ehtisham" || out.Sessions[0].Project != "proj-a" {
		t.Errorf("sessions = %+v, want exactly the shared session on ehtisham", out.Sessions)
	}
	for _, s := range out.Sessions {
		if s.ID == "personal-1" {
			t.Fatal("a personal session was listed — privacy boundary broken")
		}
	}
	if !strings.Contains(out.Note, "never shown") {
		t.Errorf("the answer should say personal sessions are never shown, got note %q", out.Note)
	}

	// transcript: the shared one is served; the personal one is refused even
	// though it exists on disk.
	if got, err := jobTranscript("shared-1"); err != nil || !strings.Contains(got, "work on the shared account") {
		t.Errorf("shared transcript = %q, %v; want its content", got, err)
	}
	if got, err := jobTranscript("personal-1"); err == nil {
		t.Errorf("personal transcript was served (%q) — privacy boundary broken", got)
	} else if !strings.Contains(err.Error(), "shared account") {
		t.Errorf("refusal should explain the shared-account rule, got %v", err)
	}

	// A ledger entry for a session that no longer exists on disk is simply
	// absent from the list, not an error.
	if err := switching.RecordSharedSession(ledger, "gone-1", "ehtisham"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(jobSessions()), &out); err != nil || len(out.Sessions) != 1 {
		t.Errorf("a ledger entry with no transcript should not appear: %+v (%v)", out.Sessions, err)
	}
}

// Only the three read-only, shared-scoped jobs exist; there is no file access.
func TestExecuteJobRefusesUnknownKinds(t *testing.T) {
	for _, kind := range []string{"ls", "get", "cat", "exec"} {
		if out, status := executeJob(panel.RemoteJob{Kind: kind, Params: "~"}); status != "error" || !strings.Contains(out, "doesn't know how to") {
			t.Errorf("kind %q -> %q / %q, want a refusal", kind, out, status)
		}
	}
}
