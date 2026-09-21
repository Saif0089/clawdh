package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/switching"
)

// captureStderr runs fn with os.Stderr on a pipe and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	w.Close()
	return <-done
}

// dropLogin takes an account's login away, as a gateway's refresh or a sign-out
// does, and gives it the identity stub a once-connected account has.
func dropLogin(t *testing.T, home, slug, email string) string {
	t.Helper()
	dir := filepath.Join(home, ".clawdh", "accounts", slug)
	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		mustWrite(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"`+slug+`-uuid","emailAddress":"`+email+`"}}`)
	}
	return dir
}

// An account that was never connected has nothing to run as. Starting a session
// on it anyway put up Claude Code's login prompt with clawdh's account name on
// it; the page is where an account is connected, so say so.
func TestRunRefusesAnAccountThatWasNeverConnected(t *testing.T) {
	home := seedRunEnv(t)
	dir := dropLogin(t, home, "work", "")
	os.Remove(filepath.Join(dir, ".claude.json")) // never connected: no identity either

	launched := false
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(string, []string, []string, string, string, onSwitch) (int, bool) {
		launched = true
		return 0, false
	}

	var code int
	out := captureStderr(t, func() { code = cmdRun([]string{"work"}) })
	if code == 0 || launched {
		t.Fatalf("exit = %d, launched = %v; want a refusal and no session", code, launched)
	}
	if !strings.Contains(out, "isn't connected on this machine yet") || !strings.Contains(out, "clawdh page") {
		t.Errorf("stderr = %q, want the page named as where to connect it", out)
	}
}

// The ordinary way a login goes missing: it was handed to the gateway, whose
// refresh rotated the single-use refresh token, and Claude Code emptied the
// local store. The share that now runs the same login is the answer.
func TestRunNamesTheShareThatRunsALoginThatLeft(t *testing.T) {
	home := seedRunEnv(t)
	dropLogin(t, home, "work", "work@example.com")
	seedShares(t, home, `[{"account":"Work","email":"work@example.com","slug":"workshare","gateway":"https://gw.example","key":"k"}]`)

	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(string, []string, []string, string, string, onSwitch) (int, bool) {
		t.Error("a session must not start on an account with no login")
		return 0, false
	}

	var code int
	out := captureStderr(t, func() { code = cmdRun([]string{"work"}) })
	if code == 0 {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"has no login on this machine any more", "work@example.com", "`clawdh shared workshare`"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
}

// An older share cache has no email field; the web page's add flow names the
// panel account by its email, so that name still finds the share.
func TestShareForLoginFallsBackToTheAccountName(t *testing.T) {
	home := seedRunEnv(t)
	seedShares(t, home, `[{"account":"work@example.com","slug":"workshare","gateway":"https://gw.example","key":"k"}]`)
	sh, ok := shareForLogin("Work@Example.com")
	if !ok || sh.Slug != "workshare" {
		t.Errorf("shareForLogin = %+v, %v; want workshare", sh, ok)
	}
	if _, ok := shareForLogin("other@example.com"); ok {
		t.Error("an email no share carries must not match")
	}
}

// Inside a session, `!clawdh <name>` used to stage the switch and let the
// supervisor relaunch onto an account that could not sign in. It is refused
// before anything is staged, so the live session is never torn down for it.
func TestRunDoesNotStageASwitchToAnAccountWithoutALogin(t *testing.T) {
	home := seedRunEnv(t)
	dropLogin(t, home, "work", "work@example.com")
	handoff := filepath.Join(home, "handoff.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SessionIDEnvVar, "sess-abc")
	t.Setenv(switching.SupervisorEnvVar, strconv.Itoa(os.Getpid()))
	stdinIsTTY = func() bool { return false }

	var code int
	out := captureStderr(t, func() { code = cmdRun([]string{"work"}) })
	if code == 0 {
		t.Error("staging a switch to an account with no login should fail")
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("nothing should have been staged")
	}
	if !strings.Contains(out, "has no login on this machine any more") {
		t.Errorf("stderr = %q", out)
	}
}

// The hook is the other way a switch is staged; it refuses for the same reason,
// in the same words, and never lets the command through to the model.
func TestHookRefusesASwitchToAnAccountWithoutALogin(t *testing.T) {
	home := seedRunEnv(t)
	dropLogin(t, home, "work", "")

	label, problem := resolveSwitchTarget("work", false)
	if label != "" || !strings.Contains(problem, "isn't connected on this machine yet") {
		t.Errorf("resolveSwitchTarget = %q, %q; want a refusal", label, problem)
	}
	if label, problem := resolveSwitchTarget("ehti", false); label != "ehti" || problem != "" {
		t.Errorf("a signed-in account should resolve, got %q, %q", label, problem)
	}
}

// A switch that reaches the supervisor anyway (staged before the login died,
// or by an older clawdh) is settled in place: the hook gets the reason and the
// session is not relaunched.
func TestSupervisorSettlesASwitchToAnAccountWithoutALogin(t *testing.T) {
	home := seedRunEnv(t)
	dropLogin(t, home, "work", "work@example.com")

	var launches int
	var handled bool
	var outcome switching.Outcome
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, applyInPlace onSwitch) (int, bool) {
		launches++
		handled = applyInPlace(switching.Handoff{Account: "work", SessionID: "sess-1"})
		outcome, _ = switching.AwaitOutcome(handoff, 0, 0)
		return 0, false
	}
	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if !handled || launches != 1 {
		t.Errorf("handled = %v, launches = %d; want the switch settled without a relaunch", handled, launches)
	}
	if !strings.Contains(outcome.Message, "has no login on this machine any more") {
		t.Errorf("outcome = %q, want the missing-login reason", outcome.Message)
	}
}

// An editor set to an account whose login has gone falls back to the default
// login rather than failing the editor, and `clawdh editor` says so.
func TestEditorTargetFallsBackWhenTheRecordedAccountHasNoLogin(t *testing.T) {
	home := seedRunEnv(t)
	dropLogin(t, home, "work", "work@example.com")
	accountsDir := filepath.Join(home, ".clawdh", "accounts")
	if err := writeEditorDefault(accountsDir, editorDefault{AccountID: "work", Name: "work"}); err != nil {
		t.Fatal(err)
	}
	accountsFile, _ := config.AccountsFile()
	store := accounts.NewStore(accountsFile)

	var target sessionTarget
	var ok bool
	out := captureStderr(t, func() {
		target, ok = editorTarget(store, accountsDir, filepath.Join(home, ".claude.json"), filepath.Join(home, ".clawdh", "shares.json"))
	})
	if !ok || target.accountID != "default" {
		t.Fatalf("editorTarget = %+v, %v; want the default login", target, ok)
	}
	if !strings.Contains(out, "has no login on this machine any more") || !strings.Contains(out, "default login instead") {
		t.Errorf("stderr = %q, want the reason and the fallback", out)
	}

	list, _ := store.Load()
	label := editorDefaultLabel(readEditorDefault(accountsDir), list, nil)
	if !strings.Contains(label, "not signed in on this machine") {
		t.Errorf("editor label = %q, want it to say the account is not signed in", label)
	}
}

// Inside an editor every chat is its own supervisor, and the extension treats
// any identity it did not expect in ~/.claude.json as another account signing in
// outside the window — it then refreshes every webview. So a switch inside a
// chat leaves the shared identity alone; only the launch asserts it. A terminal
// keeps asserting on every launch, which the relaunch test above covers.
func TestEditorSwitchLeavesTheSharedIdentityAlone(t *testing.T) {
	home := seedRunEnv(t)
	seedTranscript(t, home, "sess-ed")
	mustWrite(t, filepath.Join(home, ".clawdh", "accounts", "ehti", ".claude.json"), `{"oauthAccount":{"accountUuid":"ehti-uuid"}}`)
	hostedByEditor = true
	t.Cleanup(func() { hostedByEditor = false })

	var launches int
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		launches++
		if launches == 1 {
			// The launch asserted the editor's own account.
			got := readJSONFile(t, filepath.Join(home, ".claude.json"))
			if oa, _ := got["oauthAccount"].(map[string]any); oa["accountUuid"] != "ehti-uuid" {
				t.Errorf("identity at launch = %v, want ehti-uuid", got["oauthAccount"])
			}
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-ed"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}
	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if launches != 2 {
		t.Fatalf("want a relaunch onto work, got %d launch(es)", launches)
	}
	got := readJSONFile(t, filepath.Join(home, ".claude.json"))
	if oa, _ := got["oauthAccount"].(map[string]any); oa["accountUuid"] != "ehti-uuid" {
		t.Errorf("identity after an in-editor switch = %v, want ehti-uuid left alone", got["oauthAccount"])
	}
}

// The extension sends Claude Code's own subcommands through the wrapper as well
// as conversations; only a conversation is supervised.
func TestIsSubcommandTellsProbesFromConversations(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"auth", "status", "--json"}, true},
		{[]string{"auth", "logout"}, true},
		{[]string{"design-login", "--json"}, true},
		{[]string{"--output-format", "stream-json", "--verbose"}, false},
		{[]string{"--claude-in-chrome-mcp"}, false},
		{nil, false},
	} {
		if got := isSubcommand(tc.args); got != tc.want {
			t.Errorf("isSubcommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
