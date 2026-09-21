package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/switching"
)

// seedRunEnv points HOME at a temp dir with a two-account ~/.clawdh and a shared
// ~/.claude, so cmdRun's real config/store/hook-install paths all operate on
// throwaway files.
func seedRunEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	mustMkdir(t, filepath.Join(home, ".clawdh", "accounts"))
	mustMkdir(t, filepath.Join(home, ".claude"))
	ehtiDir := filepath.Join(home, ".clawdh", "accounts", "ehti")
	workDir := filepath.Join(home, ".clawdh", "accounts", "work")
	mustMkdir(t, ehtiDir)
	mustMkdir(t, workDir)

	// Marshal rather than hand-build the JSON: on Windows the configDir paths
	// contain backslashes, which are invalid unescaped in a JSON string.
	accountsData, err := json.Marshal(map[string]any{"accounts": []map[string]any{
		{"id": "ehti", "slug": "ehti", "kind": "managed", "configDir": ehtiDir, "alias": "claude-ehti", "isolation": "credentials-only"},
		{"id": "work", "slug": "work", "kind": "managed", "configDir": workDir, "alias": "claude-work", "isolation": "credentials-only"},
		{"id": "default", "slug": "default", "kind": "default", "configDir": "", "alias": "claude"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".clawdh", "accounts.json"), string(accountsData))
	mustWrite(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"orig"}}`)
	// Give "work" an identity stub so the switch also exercises applyIdentity.
	mustWrite(t, filepath.Join(workDir, ".claude.json"), `{"oauthAccount":{"accountUuid":"work-uuid"}}`)

	// A clawdh-supervised shell would export a handoff path; clear it so these
	// tests exercise the supervisor rather than the switch-staging path.
	t.Setenv(switching.HandoffEnvVar, "")

	// `go test` runs with stdin on a pipe. Claim the terminal so cmdRun's
	// interactive guard does not turn every supervisor test into a refusal.
	origTTY := stdinIsTTY
	stdinIsTTY = func() bool { return true }
	t.Cleanup(func() { stdinIsTTY = origTTY })
	return home
}

// seedTranscript writes the file Claude Code would have written for a session,
// in the shared ~/.claude the accounts now pool into.
func seedTranscript(t *testing.T, home, sessionID string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-some-project")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, sessionID+".jsonl"), "{}\n")
}

func mustMkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
}
func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunSupervisorRelaunchesOnSwitch drives the whole supervisor loop with a
// stubbed launcher: first "launch" stages a switch to work, second exits. The
// loop must relaunch as work with the forked session resumed, and set work's
// identity active.
func TestRunSupervisorRelaunchesOnSwitch(t *testing.T) {
	home := seedRunEnv(t)
	// The session being switched has a conversation on disk, so the relaunch
	// forks it. (Without one there is nothing to resume — see the test below.)
	seedTranscript(t, home, "sess-123")

	var calls [][]string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		captured := append([]string{}, args...)
		calls = append(calls, captured)
		if len(calls) == 1 {
			// Simulate the in-session hook staging a switch to "work".
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-123"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 launches (ehti, then work), got %d: %v", len(calls), calls)
	}
	if rest := withoutMintedID(calls[0]); len(rest) != 0 {
		t.Errorf("first launch should carry no resume args, got %v", rest)
	}
	want := []string{"--resume", "sess-123"}
	if !reflect.DeepEqual(calls[1], want) {
		t.Errorf("second launch args = %v, want %v", calls[1], want)
	}
	// The switch made work the active identity in the shared config.
	got := readJSONFile(t, filepath.Join(home, ".claude.json"))
	if oa, _ := got["oauthAccount"].(map[string]any); oa["accountUuid"] != "work-uuid" {
		t.Errorf("active identity = %v, want work-uuid", got["oauthAccount"])
	}
}

// The flags a session was started with have to survive the switch: dropping
// --dangerously-skip-permissions mid-conversation lands the user in a session
// that behaves differently from the one they were in.
func TestRunKeepsLaunchFlagsAcrossASwitch(t *testing.T) {
	home := seedRunEnv(t)
	seedTranscript(t, home, "sess-9")

	var calls [][]string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		calls = append(calls, append([]string{}, args...))
		if len(calls) == 1 {
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-9"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti", "--dangerously-skip-permissions"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	want := []string{"--dangerously-skip-permissions", "--resume", "sess-9"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
		t.Errorf("relaunch args = %v, want %v", calls[len(calls)-1], want)
	}
}

// A session switched before it wrote anything has no conversation to carry.
// Resuming it anyway is what made Claude Code exit with "No conversation found
// with session ID" and drop the user back to the shell, so the relaunch must
// start clean instead — under the same id, which the usage ledger (and an
// editor that launched the session by id) already knows it by.
func TestRunSwitchOfAnUnrecordedSessionStartsClean(t *testing.T) {
	seedRunEnv(t)

	var calls [][]string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		calls = append(calls, append([]string{}, args...))
		if len(calls) == 1 {
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "never-written"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 launches, got %d: %v", len(calls), calls)
	}
	if want := []string{"--session-id", "never-written"}; !reflect.DeepEqual(calls[1], want) {
		t.Errorf("relaunch args = %v, want %v (nothing to resume, same id)", calls[1], want)
	}
}

// An editor launches every session as `--session-id=<id>` (and a resumed one
// as `--resume=<id>`). A relaunch that kept those beside its own --resume gave
// Claude Code two session flags, so the launch's session args must go and only
// the relaunch's remain.
func TestRunRelaunchDropsTheLaunchSessionArgs(t *testing.T) {
	seedRunEnv(t)
	home := os.Getenv("HOME")
	// Record a transcript for the session so the relaunch resumes it.
	proj := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(proj, "sess-editor.jsonl"), "{}\n")

	var calls [][]string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		calls = append(calls, append([]string{}, args...))
		if len(calls) == 1 {
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-editor"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti", "--output-format", "stream-json", "--session-id=sess-editor"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 launches, got %d: %v", len(calls), calls)
	}
	want := []string{"--output-format", "stream-json", "--resume", "sess-editor"}
	if !reflect.DeepEqual(calls[1], want) {
		t.Errorf("relaunch args = %v, want %v", calls[1], want)
	}
}

func TestRunUnknownAccount(t *testing.T) {
	seedRunEnv(t)
	if code := cmdRun([]string{"nope"}); code == 0 {
		t.Error("running an unknown account should fail")
	}
}

func TestRunClaudeOnceTerminatesOnHandoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude is POSIX")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	mustWrite(t, fake, "#!/bin/sh\nsleep 30\n")
	os.Chmod(fake, 0o755)
	handoff := filepath.Join(dir, "handoff.json")

	// Stage a switch shortly after launch; the supervisor should notice and
	// terminate the sleeping child.
	go func() {
		time.Sleep(300 * time.Millisecond)
		switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "s"})
	}()

	done := make(chan bool, 1)
	go func() {
		_, switched := runClaudeOnce(fake, nil, os.Environ(), handoff, "acct", nil)
		done <- switched
	}()
	select {
	case switched := <-done:
		if !switched {
			t.Error("runClaudeOnce should report switched=true when a handoff appears")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runClaudeOnce did not terminate the child on a handoff")
	}
}

func TestRunClaudeOnceReturnsChildExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude is POSIX")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	mustWrite(t, fake, "#!/bin/sh\nexit 7\n")
	os.Chmod(fake, 0o755)

	code, switched := runClaudeOnce(fake, nil, os.Environ(), filepath.Join(dir, "no-handoff.json"), "acct", nil)
	if switched {
		t.Error("a clean exit is not a switch")
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (the child's)", code)
	}
}

// TestRunStagesSwitchInsideSupervisedSession is `!clawdh work` typed in a session
// clawdh is supervising: a shell command, so no tty and no hook payload, but the
// handoff path and session id are inherited from the session. It must stage the
// switch for the supervisor instead of starting a second session.
func TestRunStagesSwitchInsideSupervisedSession(t *testing.T) {
	home := seedRunEnv(t)
	handoff := filepath.Join(home, "handoff.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SessionIDEnvVar, "sess-abc")
	// The supervisor has to still be running for a staged switch to mean
	// anything; this test process stands in for it.
	t.Setenv(switching.SupervisorEnvVar, strconv.Itoa(os.Getpid()))
	stdinIsTTY = func() bool { return false }

	launched := false
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		launched = true
		return 0, false
	}

	if code := cmdRun([]string{"work"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if launched {
		t.Error("staging a switch must not launch a nested Claude Code")
	}
	h, ok := switching.ReadHandoff(handoff)
	if !ok {
		t.Fatal("no handoff staged")
	}
	if h.Account != "work" || h.SessionID != "sess-abc" {
		t.Errorf("handoff = %+v, want account work / session sess-abc", h)
	}
}

// CLAWDH_HANDOFF is inherited by anything a session spawned, including processes
// that outlive it. Staging a handoff against a supervisor that has exited
// printed "Switching…" and did nothing at all.
func TestRunRefusesToStageForADeadSupervisor(t *testing.T) {
	home := seedRunEnv(t)
	handoff := filepath.Join(home, "handoff.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SessionIDEnvVar, "sess-abc")
	t.Setenv(switching.SupervisorEnvVar, "999999") // no such process
	stdinIsTTY = func() bool { return false }

	if code := cmdRun([]string{"work"}); code == 0 {
		t.Error("staging against a dead supervisor should fail, got exit 0")
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("nothing should have been staged")
	}
}

// `clawdh ehti -p "..."` inside a supervised session is a deliberate one-shot on
// another account, not a request to switch the session and throw the arguments
// away.
func TestRunWithArgsInsideASessionDoesNotStageASwitch(t *testing.T) {
	home := seedRunEnv(t)
	handoff := filepath.Join(home, "handoff.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SupervisorEnvVar, strconv.Itoa(os.Getpid()))
	stdinIsTTY = func() bool { return false }

	var got []string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		got = append([]string{}, args...)
		return 0, false
	}

	if code := cmdRun([]string{"work", "-p", "hello"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("a command with arguments must not stage a switch")
	}
	if want := []string{"-p", "hello"}; !reflect.DeepEqual(withoutMintedID(got), want) {
		t.Errorf("launch args = %v, want %v", got, want)
	}
}

// `clawdh run --auto` is how the shell wrapper starts a plain `claude`: no
// account was named, so it supervises the one that shell was already pointed
// at — here, an account directory exported by clawdh's own alias.
func TestRunAutoFollowsTheAccountTheShellPointsAt(t *testing.T) {
	home := seedRunEnv(t)
	workDir := filepath.Join(home, ".clawdh", "accounts", "work")
	t.Setenv(accounts.SecureStorageEnvVar, workDir)

	var env []string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, e []string, handoff, accountID string, _ onSwitch) (int, bool) {
		env = e
		return 0, false
	}

	if code := cmdRun([]string{"--auto"}); code != 0 {
		t.Fatalf("cmdRun --auto exit = %d, want 0", code)
	}
	want := accounts.SecureStorageEnvVar + "=" + workDir
	if !slices.Contains(env, want) {
		t.Errorf("child env does not scope the credential store to work (%q)", want)
	}
}

// With nothing exported, a plain `claude` means the default login — so that is
// what the wrapper must supervise, not some managed account.
func TestRunAutoFallsBackToTheDefaultAccount(t *testing.T) {
	seedRunEnv(t)
	t.Setenv(accounts.SecureStorageEnvVar, "")

	var env []string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, e []string, handoff, accountID string, _ onSwitch) (int, bool) {
		env = e
		return 0, false
	}

	if code := cmdRun([]string{"--auto"}); code != 0 {
		t.Fatalf("cmdRun --auto exit = %d, want 0", code)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, accounts.SecureStorageEnvVar+"=") {
			t.Errorf("default account must not scope the credential store, got %q", kv)
		}
	}
}

// Typing `claude` inside a supervised session starts a nested session; it is
// not a request to re-account the session you are in.
func TestRunAutoNeverStagesASwitch(t *testing.T) {
	home := seedRunEnv(t)
	handoff := filepath.Join(home, "handoff.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SupervisorEnvVar, strconv.Itoa(os.Getpid()))

	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, e []string, h, accountID string, _ onSwitch) (int, bool) { return 0, false }

	if code := cmdRun([]string{"--auto"}); code != 0 {
		t.Fatalf("cmdRun --auto exit = %d, want 0", code)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("--auto must never stage a switch")
	}
}

// TestRunRefusesWithoutATerminal covers `!clawdh ehti` from inside a Claude Code
// session: no tty, no passthrough args, so clawdh must explain itself rather than
// launch a Claude Code that dies on "Input must be provided...".
func TestRunRefusesWithoutATerminal(t *testing.T) {
	seedRunEnv(t)
	stdinIsTTY = func() bool { return false }

	launched := false
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		launched = true
		return 0, false
	}

	if code := cmdRun([]string{"ehti"}); code == 0 {
		t.Error("cmdRun without a terminal should fail, got exit 0")
	}
	if launched {
		t.Error("cmdRun should not launch Claude Code when there is no terminal")
	}
}

// A caller who passes Claude Code arguments is driving it deliberately
// (`clawdh ehti -p "..."`), so the guard stays out of the way.
func TestRunWithArgsSkipsTheTerminalGuard(t *testing.T) {
	seedRunEnv(t)
	stdinIsTTY = func() bool { return false }

	var got []string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, _ onSwitch) (int, bool) {
		got = append([]string{}, args...)
		return 0, false
	}

	if code := cmdRun([]string{"ehti", "-p", "hello"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	want := []string{"-p", "hello"}
	if !reflect.DeepEqual(withoutMintedID(got), want) {
		t.Errorf("launch args = %v, want %v", got, want)
	}
}

// withoutMintedID drops the --session-id clawdh mints for every fresh launch, so
// a test can assert on the arguments it is actually about.
func withoutMintedID(args []string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session-id" {
			return append(append([]string{}, args[:i]...), args[i+2:]...)
		}
	}
	return args
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Switching a live session relaunches it. clawdh does not write credential stores
// any more, so the login a running Claude Code reads cannot be changed
// underneath it: the conversation is carried across, and anything running
// inside the old process ends with it.
func TestSwitchRelaunchesOntoTheOtherAccount(t *testing.T) {
	home := seedRunEnv(t)
	seedTranscript(t, home, "sess-1")
	ehti := filepath.Join(home, ".clawdh", "accounts", "ehti")
	ledger := filepath.Join(home, switching.LedgerFile)
	mustWrite(t, ledger, "#cutover\t1.000000\n")

	var launches [][]string
	var handled bool

	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, applyInPlace onSwitch) (int, bool) {
		launches = append(launches, append([]string{}, args...))
		if len(launches) == 1 {
			// The session runs on the account's own directory: there is no
			// per-session copy of the login any more.
			if !slices.Contains(env, accounts.SecureStorageEnvVar+"="+ehti) {
				t.Errorf("session was not scoped to the account's own directory; env had %v", env)
			}
			handled = applyInPlace(switching.Handoff{Account: "work", SessionID: "sess-1"})
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-1"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if handled {
		t.Error("a switch to a real account must ask for a relaunch, not report itself done")
	}
	if len(launches) != 2 {
		t.Fatalf("want one relaunch, got %d launch(es)", len(launches))
	}
	if !slices.Contains(launches[0], "--session-id") {
		t.Error("clawdh should mint the session id so usage is attributed from the first token")
	}
	// The minted id must not survive into the relaunch: --session-id names a
	// new conversation and --resume reopens an existing one.
	want := []string{"--resume", "sess-1"}
	if !reflect.DeepEqual(launches[1], want) {
		t.Errorf("relaunch args = %v, want %v", launches[1], want)
	}

	// The conversation keeps its id, so clawdh can record who owns it from the
	// switch onward. The monitor reads ownership by interval, so the work done
	// before the switch stays with the account that did it.
	work := filepath.Join(home, ".clawdh", "accounts", "work")
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sess-1\t"+realpath(t, work)) {
		t.Errorf("no ownership line for the account switched to:\n%s", raw)
	}
}

// The conversation is what has to survive a switch, so the relaunch resumes it
// rather than starting clean.
func TestSwitchRelaunchResumesTheConversation(t *testing.T) {
	home := seedRunEnv(t)
	seedTranscript(t, home, "sess-2")

	var launches [][]string
	origRunner := claudeRunner
	t.Cleanup(func() { claudeRunner = origRunner })
	claudeRunner = func(bin string, args, env []string, handoff, accountID string, applyInPlace onSwitch) (int, bool) {
		launches = append(launches, append([]string{}, args...))
		if len(launches) == 1 {
			if applyInPlace != nil && applyInPlace(switching.Handoff{Account: "work", SessionID: "sess-2"}) {
				t.Fatal("a switch to a real account claimed it needed no relaunch")
			}
			if err := switching.WriteHandoff(handoff, switching.Handoff{Account: "work", SessionID: "sess-2"}); err != nil {
				t.Fatal(err)
			}
			return 0, true
		}
		return 0, false
	}

	if code := cmdRun([]string{"ehti"}); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if len(launches) != 2 {
		t.Fatalf("want a relaunch as the fallback, got %d launch(es)", len(launches))
	}
	want := []string{"--resume", "sess-2"}
	if !reflect.DeepEqual(launches[1], want) {
		t.Errorf("relaunch args = %v, want %v", launches[1], want)
	}
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// A revoked account must stop its live session: the supervisor notices the
// panel's revocation marker on the same poll it watches for switches, and
// terminates the child WITHOUT reporting a switch, so cmdRun exits (with the
// revocation message) rather than relaunching.
func TestRunClaudeOnceStopsOnRevocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude is POSIX")
	}
	t.Setenv("HOME", t.TempDir()) // config.HomeDir -> a temp ~/.clawdh
	t.Setenv("USERPROFILE", t.TempDir())

	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	mustWrite(t, fake, "#!/bin/sh\nsleep 30\n")
	os.Chmod(fake, 0o755)
	handoff := filepath.Join(dir, "handoff.json")

	// The account is taken back shortly after launch.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := config.MarkRevoked("work"); err != nil {
			t.Errorf("MarkRevoked: %v", err)
		}
	}()

	done := make(chan bool, 1)
	go func() {
		_, switched := runClaudeOnce(fake, nil, os.Environ(), handoff, "work", nil)
		done <- switched
	}()
	select {
	case switched := <-done:
		if switched {
			t.Error("a revoked session must not report a switch (which would relaunch it)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop the session when its account was revoked")
	}
}
