package sessions

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// liveProcess starts something harmless that outlives the assertions, so a
// register entry naming it is genuinely live.
func liveProcess(t *testing.T) int {
	t.Helper()
	name, args := "sleep", []string{"30"}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", "ping", "-n", "30", "127.0.0.1"}
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process here: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

func TestPublishedSessionIsListed(t *testing.T) {
	dir := t.TempDir()
	pid := liveProcess(t)
	want := Session{
		PID: pid, Nonce: "n1", SessionID: "sess-1", Account: "Ehtisham",
		AccountID: "ehti", Slug: "ehti", Host: HostEditor, Editor: "VS Code",
		Dir: "/work/app", Handoff: "/tmp/h.json", StartedAt: time.Now(),
	}
	if err := Publish(dir, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := List(dir)
	if len(got) != 1 {
		t.Fatalf("List returned %d sessions, want 1", len(got))
	}
	if got[0].Account != "Ehtisham" || got[0].Nonce != "n1" || got[0].Host != HostEditor {
		t.Errorf("listed %+v, want the published session back", got[0])
	}
}

// A switch rewrites the entry, so the register always names the account the
// session is on now rather than the one it started as.
func TestPublishReplacesTheEntryForAPID(t *testing.T) {
	dir := t.TempDir()
	pid := liveProcess(t)
	_ = Publish(dir, Session{PID: pid, Account: "Ehtisham", AccountID: "ehti"})
	_ = Publish(dir, Session{PID: pid, Account: "Personal", AccountID: "personal"})

	got := List(dir)
	if len(got) != 1 {
		t.Fatalf("List returned %d sessions, want 1", len(got))
	}
	if got[0].Account != "Personal" {
		t.Errorf("account = %q, want the account it was switched to", got[0].Account)
	}
}

// A supervisor killed outright never withdrew itself. Counting the dead is
// worse than no register at all, so they are dropped — and the file with them,
// so the next read is not slowed by the same corpse.
func TestListDropsSessionsWhoseProcessIsGone(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process here: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	_ = Publish(dir, Session{PID: pid, Account: "Ehtisham"})
	if got := List(dir); len(got) != 0 {
		t.Fatalf("List returned %d sessions, want none for a dead process", len(got))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the dead session's file is still there: %d entries", len(entries))
	}
}

func TestWithdrawRemovesTheSession(t *testing.T) {
	dir := t.TempDir()
	pid := liveProcess(t)
	_ = Publish(dir, Session{PID: pid, Account: "Ehtisham"})
	Withdraw(dir, pid)
	if got := List(dir); len(got) != 0 {
		t.Errorf("List returned %d sessions after Withdraw, want none", len(got))
	}
}

func TestDefaultRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-sessions.json")
	if d := ReadDefault(path); d.Set() {
		t.Errorf("a missing file read as %+v, want nothing set", d)
	}
	if err := WriteDefault(path, Default{AccountID: "ehti", Name: "Ehtisham"}); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	got := ReadDefault(path)
	if got.AccountID != "ehti" || got.Name != "Ehtisham" || !got.Set() {
		t.Errorf("read %+v, want the account back", got)
	}
	if err := ClearDefault(path); err != nil {
		t.Fatalf("ClearDefault: %v", err)
	}
	if ReadDefault(path).Set() {
		t.Error("the default survived being cleared")
	}
}

// Upgrading must not silently move an editor back onto the machine's own
// login: the file `clawdh editor <name>` wrote is adopted on first read.
func TestDefaultAdoptsTheOldEditorOnlySetting(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new-sessions.json")
	legacy := filepath.Join(base, "editors", "default.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"accountId":"ehti","name":"Ehtisham"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadDefault(path)
	if got.AccountID != "ehti" {
		t.Fatalf("read %+v, want the editor-only setting adopted", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the adopted setting was not written to its new home: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the old file is still there; two settings can now disagree")
	}
}
