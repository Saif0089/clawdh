package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/sessions"
	"clawdh/internal/switching"
)

// A register entry has to name a process that is really there, or List drops
// it — which is the behaviour these tests depend on being true.
func liveHelper(t *testing.T) int {
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

// seedAccount registers an account with a login on this machine, so it is one
// a session may actually be moved to.
func seedAccount(t *testing.T, srv *Server, name string) accounts.Account {
	t.Helper()
	acct, err := srv.manager.Add(name)
	if err != nil {
		t.Fatalf("adding %s: %v", name, err)
	}
	if err := os.MkdirAll(acct.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x","refreshToken":"sk-ant-ort01-x","expiresAt":9999999999999,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(acct.ConfigDir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	return acct
}

// publishSession puts one live session in the register and returns the file a
// switch for it would be staged in.
func publishSession(t *testing.T, home string, s sessions.Session) string {
	t.Helper()
	dir := filepath.Join(home, ".clawdh", "sessions")
	if s.Handoff == "" {
		s.Handoff = filepath.Join(t.TempDir(), "handoff.json")
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}
	if err := sessions.Publish(dir, s); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return s.Handoff
}

func getSessions(t *testing.T, ts *httptest.Server) sessionsResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatalf("GET /api/sessions: %v", err)
	}
	defer resp.Body.Close()
	var out sessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func post(t *testing.T, ts *httptest.Server, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// The page's whole answer to "what am I running, and as whom" comes from here,
// including which of the two ways of running Claude Code each session is.
func TestSessionsListsWhatIsRunning(t *testing.T) {
	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ehti := seedAccount(t, srv, "Ehtisham")

	publishSession(t, home, sessions.Session{
		PID: liveHelper(t), Nonce: "n1", SessionID: "sess-1", Account: "Ehtisham",
		AccountID: ehti.ID, Slug: ehti.Slug, Host: sessions.HostEditor,
		Editor: "VS Code", Dir: filepath.Join(home, "dev", "web"),
	})

	got := getSessions(t, ts)
	if !got.Supported {
		t.Fatal("the register reported itself unreadable")
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.Host != "editor" || s.Editor != "VS Code" || s.Account != "Ehtisham" {
		t.Errorf("listed %+v, want the VS Code chat on Ehtisham", s)
	}
	if s.Where != "~/dev/web" {
		t.Errorf("where = %q, want the folder written as ~/dev/web", s.Where)
	}
	if got.Default.Set {
		t.Errorf("default = %+v, want nothing set yet", got.Default)
	}
}

// Moving one session stages exactly what typing `clawdh <name>` into it would
// have — the same conversation, the same file — with the stamp that keeps it
// from being read by a supervisor that inherited the pid.
func TestSwitchOneSessionStagesTheHandoff(t *testing.T) {
	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ehti := seedAccount(t, srv, "Ehtisham")
	work := seedAccount(t, srv, "Work")

	pid := liveHelper(t)
	handoff := publishSession(t, home, sessions.Session{
		PID: pid, Nonce: "n1", SessionID: "sess-1", Account: "Ehtisham",
		AccountID: ehti.ID, Slug: ehti.Slug, Host: sessions.HostEditor,
	})

	resp, body := post(t, ts, "/api/sessions/switch", map[string]any{"pid": pid, "accountId": work.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["moved"] != float64(1) {
		t.Errorf("moved = %v, want 1", body["moved"])
	}

	h, ok := switching.ReadHandoff(handoff)
	if !ok {
		t.Fatal("no switch was staged for that session")
	}
	if h.Account != work.Slug || h.SessionID != "sess-1" || h.Shared {
		t.Errorf("staged %+v, want this conversation moved to %s", h, work.Slug)
	}
	if h.For != "n1" {
		t.Errorf("for = %q, want the session's own stamp", h.For)
	}
}

// "Use for everything" must not restart the sessions that are already there:
// a needless relaunch costs whatever was running inside them.
func TestSwitchAllSkipsSessionsAlreadyOnTheAccount(t *testing.T) {
	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ehti := seedAccount(t, srv, "Ehtisham")
	work := seedAccount(t, srv, "Work")

	onWork := publishSession(t, home, sessions.Session{
		PID: liveHelper(t), Nonce: "n1", SessionID: "sess-1", Account: "Work",
		AccountID: work.ID, Slug: work.Slug, Host: sessions.HostTerminal,
	})
	onEhti := publishSession(t, home, sessions.Session{
		PID: liveHelper(t), Nonce: "n2", SessionID: "sess-2", Account: "Ehtisham",
		AccountID: ehti.ID, Slug: ehti.Slug, Host: sessions.HostEditor,
	})

	resp, body := post(t, ts, "/api/sessions/switch", map[string]any{"all": true, "accountId": work.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["moved"] != float64(1) || body["already"] != float64(1) {
		t.Errorf("moved = %v, already = %v; want one moved and one left alone", body["moved"], body["already"])
	}
	if _, staged := switching.ReadHandoff(onWork); staged {
		t.Error("a session already on the account was restarted anyway")
	}
	if h, staged := switching.ReadHandoff(onEhti); !staged || h.Account != work.Slug {
		t.Errorf("the other session was not moved: %+v", h)
	}
}

// Moving a session onto an account with no login here would relaunch it as a
// sign-in prompt. Refuse before anything is staged, and say which account.
func TestSwitchRefusesAnAccountWithNoLogin(t *testing.T) {
	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ehti := seedAccount(t, srv, "Ehtisham")
	gone, err := srv.manager.Add("Gone") // added, never signed in
	if err != nil {
		t.Fatal(err)
	}

	pid := liveHelper(t)
	handoff := publishSession(t, home, sessions.Session{
		PID: pid, Nonce: "n1", SessionID: "sess-1", Account: "Ehtisham",
		AccountID: ehti.ID, Slug: ehti.Slug, Host: sessions.HostTerminal,
	})

	resp, body := post(t, ts, "/api/sessions/switch", map[string]any{"pid": pid, "accountId": gone.ID})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg, _ := body["error"].(string); msg == "" || !bytes.Contains([]byte(msg), []byte("Gone")) {
		t.Errorf("error = %q, want it to name the account", msg)
	}
	if _, staged := switching.ReadHandoff(handoff); staged {
		t.Error("a switch was staged onto an account that cannot sign in")
	}
}

// The account new sessions start as is one setting covering editors and
// terminals, and the page reads back what it just set.
func TestSetSessionDefaultIsRecordedAndReported(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ehti := seedAccount(t, srv, "Ehtisham")

	resp, _ := post(t, ts, "/api/sessions/default", map[string]any{"accountId": ehti.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := getSessions(t, ts)
	if !got.Default.Set || got.Default.AccountID != ehti.ID || got.Default.Label != "Ehtisham" {
		t.Fatalf("default = %+v, want Ehtisham", got.Default)
	}
	if got.Default.Problem != "" {
		t.Errorf("problem = %q, want none for an account that is signed in", got.Default.Problem)
	}

	// Removing the account leaves the setting naming something that is gone.
	// "Set" is not the same as "working", and the page has to be able to say so.
	if _, err := srv.manager.Remove(ehti.ID); err != nil {
		t.Fatal(err)
	}
	got = getSessions(t, ts)
	if got.Default.Problem == "" {
		t.Error("the default still reads as fine although the account is gone")
	}

	resp, _ = post(t, ts, "/api/sessions/default", map[string]any{"clear": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing: status = %d, want 200", resp.StatusCode)
	}
	if getSessions(t, ts).Default.Set {
		t.Error("the default survived being cleared")
	}
}
