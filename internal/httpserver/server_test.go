package httpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/ptyauth"
	"clawdh/internal/shellrc"
)

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	src := filepath.Join(wd, "..", "..", "testdata", "fakeclaude")
	// The .exe suffix is required, not cosmetic: Windows' exec.Command
	// resolves a path through PATHEXT, so an extensionless binary can't
	// be run by the completion probe at all — while ConPTY's
	// CreateProcess happily runs it, which produced the confusing
	// result of a login that showed its URL but never linked.
	name := "fakeclaude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", out, src)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fakeclaude: %v\n%s", err, output)
	}
	return out
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	// Not just for the paths built below: New() constructs a usage
	// Service, which keeps its last good numbers under ~/.clawdh. Without
	// this the test writes into the home of whoever is running it.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows' UserHomeDir
	store := accounts.NewStore(filepath.Join(home, ".clawdh", "accounts.json"))
	manager := accounts.NewManager(store, filepath.Join(home, ".clawdh", "accounts"))
	syncer := shellrc.NewSyncer(home)
	fake := buildFakeClaude(t)
	return New(manager, syncer, fake), home
}

func TestAccountsCRUDLifecycle(t *testing.T) {
	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create.
	createBody, _ := json.Marshal(map[string]string{"name": "Work"})
	resp, err := http.Post(ts.URL+"/api/accounts", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/accounts: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created accounts.Account
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if created.Alias != "claude-work" {
		t.Errorf("alias = %q, want claude-work", created.Alias)
	}

	// Accounts no longer get a shell alias — they run as `clawdh <name>` — so
	// creating one adds nothing to the managed rc block: it carries the `claude`
	// wrapper (which every sync writes, so a plain `claude` is switchable) and
	// no alias for the account.
	rcPath := anyRcPath(home)
	assertWrapperOnly(t, rcPath, "creating an account", "claude-work")

	// List.
	resp, err = http.Get(ts.URL + "/api/accounts")
	if err != nil {
		t.Fatalf("GET /api/accounts: %v", err)
	}
	var list accountsResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(list.Accounts))
	}

	// Rename.
	renameBody, _ := json.Marshal(map[string]string{"name": "Side Project"})
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/accounts/"+created.ID, bytes.NewReader(renameBody))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	assertWrapperOnly(t, rcPath, "renaming an account", "claude-side-project")

	// Delete.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/accounts/"+created.ID+"?confirm=true", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// The block stays when the last account goes: the wrapper is not an alias.
	// (Removing it here is what once left every new terminal's `claude`
	// unsupervised; only uninstall removes it.)
	assertWrapperOnly(t, rcPath, "deleting the last account", "claude-work")
}

// assertWrapperOnly checks that the managed rc block is present with the
// `claude` wrapper and carries no alias for the account named.
func assertWrapperOnly(t *testing.T, rcPath, after, alias string) {
	t.Helper()
	data, err := os.ReadFile(rcPath)
	if err != nil {
		t.Errorf("%s: rc file should hold the managed block: %v", after, err)
		return
	}
	if !strings.Contains(string(data), "clawdh run --auto") {
		t.Errorf("%s: rc block lost the claude wrapper: %q", after, data)
	}
	if strings.Contains(string(data), alias) {
		t.Errorf("%s: wrote a shell alias, want none: %q", after, data)
	}
}

func anyRcPath(home string) string {
	for _, p := range shellrc.RcPaths(home) {
		return p
	}
	return ""
}

func TestLoginFlowEndToEnd(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]string{"name": "Work"})
	resp, _ := http.Post(ts.URL+"/api/accounts", "application/json", bytes.NewReader(createBody))
	var account accounts.Account
	json.NewDecoder(resp.Body).Decode(&account)
	resp.Body.Close()

	resp, err := http.Post(ts.URL+"/api/accounts/"+account.ID+"/login", "", nil)
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/accounts/" + account.ID + "/login/events")
	if err != nil {
		t.Fatalf("GET login/events: %v", err)
	}
	defer resp.Body.Close()

	// scanner.Scan() blocks, so a deadline checked in the loop condition
	// never fires while nothing is arriving — that turned a login that
	// simply never completed into a ten-minute hang with no diagnosis.
	// Read on a goroutine and close the body to break out instead.
	var sawURL, sawLinked bool
	var seen []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			seen = append(seen, line)
			var ev ptyauth.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case ptyauth.EventURL:
				sawURL = true
			case ptyauth.EventLinked:
				sawLinked = true
			}
			if sawLinked {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(45 * time.Second):
		resp.Body.Close()
		<-done
		t.Errorf("timed out waiting for SSE events; frames seen: %v", seen)
	}

	if !sawURL {
		t.Error("expected a url event over SSE")
	}
	if !sawLinked {
		t.Error("expected a linked event over SSE")
	}

	// The account's stored status should now be "linked" too.
	resp, _ = http.Get(ts.URL + "/api/accounts")
	var list accountsResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Accounts) != 1 || list.Accounts[0].Status != accounts.StatusLinked {
		t.Errorf("accounts = %+v, want one linked account", list.Accounts)
	}

	// A successful login is followed by marking Claude Code's own
	// onboarding complete, so the first `claude` run under this account
	// doesn't walk the first-run wizard. That happens on a goroutine
	// after the event, so wait for it — both to assert it happens and
	// so it isn't still writing into the temp dir during cleanup.
	claudeConfig := filepath.Join(account.ConfigDir, ".claude.json")
	if !waitForFile(claudeConfig, 20*time.Second) {
		t.Errorf("expected %s to be written after a successful login", claudeConfig)
	}
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestLaunchTerminalUsesInjectedLauncher(t *testing.T) {
	srv, _ := newTestServer(t)
	var gotConfigDir string
	srv.launchTerminal = func(configDir, label string) error {
		gotConfigDir = configDir
		return nil
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]string{"name": "Work"})
	resp, _ := http.Post(ts.URL+"/api/accounts", "application/json", bytes.NewReader(createBody))
	var account accounts.Account
	json.NewDecoder(resp.Body).Decode(&account)
	resp.Body.Close()

	resp, err := http.Post(ts.URL+"/api/accounts/"+account.ID+"/launch-terminal", "", nil)
	if err != nil {
		t.Fatalf("POST launch-terminal: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	if gotConfigDir != account.ConfigDir {
		t.Errorf("launchTerminal called with %q, want %q", gotConfigDir, account.ConfigDir)
	}
}

func TestWebUIServedAtRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := new(bytes.Buffer)
	body.ReadFrom(resp.Body)
	if !strings.Contains(body.String(), "clawdh") {
		t.Errorf("expected the embedded UI to mention clawdh, got %d bytes", body.Len())
	}
}
