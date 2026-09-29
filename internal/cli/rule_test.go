package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawdh/internal/config"
	"clawdh/internal/sessions"
	"clawdh/internal/switching"
	"clawdh/internal/usage"
	"clawdh/panel"
)

// A move for later has to happen exactly when it was asked to — after an
// answer, once enough is used, in the session it was set in — and never on a
// guess. These drive applyRule, the whole of the Stop hook's decision.

func ruleSession() sessions.Session {
	return sessions.Session{PID: os.Getpid(), Nonce: "n1", Account: "HassanDH", AccountID: "a1", Slug: "hassandh"}
}

func withRule(t *testing.T, r switching.Rule) string {
	t.Helper()
	handoff := filepath.Join(t.TempDir(), ".handoff-1.json")
	if err := switching.WriteRule(handoff, r); err != nil {
		t.Fatal(err)
	}
	return handoff
}

func reads(p float64) func(sessions.Session, string) (float64, bool) {
	return func(sessions.Session, string) (float64, bool) { return p, true }
}

func TestARuleWaitsUntilEnoughIsUsed(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "saif", Shared: true, Window: switching.WindowWeek, AtPercent: 5, For: "n1"})
	if msg := applyRule(handoff, "s1", ruleSession(), reads(4.9)); msg != "" {
		t.Errorf("fired at 4.9%% of a 5%% rule: %q", msg)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("a switch was staged below the threshold")
	}
	if _, ok := switching.ReadRule(handoff); !ok {
		t.Error("the rule was dropped before it fired")
	}
}

func TestARuleMovesTheSessionOnceEnoughIsUsed(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "saif", Shared: true, Window: switching.WindowWeek, AtPercent: 5, For: "n1"})
	msg := applyRule(handoff, "s1", ruleSession(), reads(5))
	if !strings.Contains(msg, "HassanDH reached 5% of its week") || !strings.Contains(msg, "saif") {
		t.Errorf("message = %q", msg)
	}
	h, ok := switching.ReadHandoff(handoff)
	if !ok || h.Account != "saif" || !h.Shared || h.SessionID != "s1" {
		t.Fatalf("staged %+v, %v; want the ordinary switch to saif, keeping conversation s1", h, ok)
	}
	if _, ok := switching.ReadRule(handoff); ok {
		t.Error("the rule outlived the move it made")
	}
}

// With no reading to go on — the service is down, a shared account's numbers
// predate an outage — nothing moves.
func TestARuleNeverFiresOnAGuess(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "saif", Shared: true, Window: switching.WindowWeek, AtPercent: 5, For: "n1"})
	none := func(sessions.Session, string) (float64, bool) { return 0, false }
	if msg := applyRule(handoff, "s1", ruleSession(), none); msg != "" {
		t.Errorf("fired without a reading: %q", msg)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("a switch was staged without a reading")
	}
}

// A rule left behind by a session that has ended must not fire in another
// that happens to have inherited its pid.
func TestARuleFromAnotherSessionIsDropped(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "saif", Shared: true, Window: switching.WindowWeek, AtPercent: 5, For: "someone-else"})
	if msg := applyRule(handoff, "s1", ruleSession(), reads(99)); msg != "" {
		t.Errorf("a stranger's rule fired: %q", msg)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("a stranger's rule staged a switch")
	}
	if _, ok := switching.ReadRule(handoff); ok {
		t.Error("a stranger's rule was left in place")
	}
}

// `at 0%` means "as soon as this answer finishes": no reading needed.
func TestAtZeroMovesAfterTheCurrentAnswer(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "ehti", Window: switching.WindowWeek, AtPercent: 0, For: "n1"})
	never := func(sessions.Session, string) (float64, bool) {
		t.Error("usage was read for an at-0% rule")
		return 0, false
	}
	if msg := applyRule(handoff, "s1", ruleSession(), never); !strings.Contains(msg, "moving this session to ehti") {
		t.Errorf("message = %q", msg)
	}
	if h, ok := switching.ReadHandoff(handoff); !ok || h.Account != "ehti" || h.Shared {
		t.Errorf("staged %+v, %v", h, ok)
	}
}

// chdirUnreadable moves the test into a folder it cannot list, as macOS privacy
// protection does to a terminal in Documents.
func chdirUnreadable(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not stop a folder being listed on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	dir := filepath.Join(t.TempDir(), "locked-project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PWD", dir)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir
}

// A folder Claude Code cannot read gets a plain explanation, not Bun's
// "An unknown error occurred (Unexpected)".
func TestAnUnreadableFolderIsExplained(t *testing.T) {
	chdirUnreadable(t)
	problem := cwdProblem()
	if !strings.Contains(problem, "locked-project") {
		t.Fatalf("cwdProblem = %q; want it to name the folder", problem)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(problem, "Full Disk Access") {
		t.Errorf("cwdProblem = %q; want the macOS fix named", problem)
	}
}

func TestAReadableFolderIsNoProblem(t *testing.T) {
	t.Chdir(t.TempDir())
	if p := cwdProblem(); p != "" {
		t.Errorf("cwdProblem = %q for an ordinary empty folder", p)
	}
}

// ...and a rule due while the folder is unreadable waits instead of
// relaunching into it, which would end a session that is still running.
func TestARuleDoesNotRelaunchIntoAnUnreadableFolder(t *testing.T) {
	handoff := withRule(t, switching.Rule{Account: "saif", Shared: true, Window: switching.WindowWeek, AtPercent: 5, For: "n1"})
	chdirUnreadable(t)
	msg := applyRule(handoff, "s1", ruleSession(), reads(50))
	if !strings.Contains(msg, "did not move this session yet") {
		t.Errorf("message = %q", msg)
	}
	if _, ok := switching.ReadHandoff(handoff); ok {
		t.Error("a switch was staged into a folder Claude Code cannot read")
	}
	if _, ok := switching.ReadRule(handoff); !ok {
		t.Error("the rule was dropped instead of kept for later")
	}
}

// One of the machine's own accounts is judged on the service's reading — the
// numbers the page shows.
func TestLocalUsageComesFromTheService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/a1/usage" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(usage.Snapshot{Usage: &usage.Report{Limits: []usage.Limit{
			{Kind: "session", Percent: 47},
			{Kind: "weekly_all", Percent: 33},
			{Kind: "weekly_scoped", Percent: 81},
		}}})
	}))
	defer srv.Close()
	if p, ok := localUsagePercent(srv.URL, "a1", switching.WindowWeek); !ok || p != 33 {
		t.Errorf("week = %v, %v; want 33 — the all-models weekly limit", p, ok)
	}
	if p, ok := localUsagePercent(srv.URL, "a1", switching.Window5h); !ok || p != 47 {
		t.Errorf("5h = %v, %v; want 47", p, ok)
	}
	if _, ok := localUsagePercent(srv.URL, "nobody", switching.WindowWeek); ok {
		t.Error("an unknown account produced a reading")
	}
}

// A shared account is judged on the gateway's reading from the last check-in,
// and only while it is recent.
func TestSharedUsageMustBeRecent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	sess := sessions.Session{Shared: true, Slug: "saif", Account: "saif"}

	panel.SaveWindows([]panel.ShareWindow{{Slug: "saif", FiveH: 0.06, SevenD: 0.15, UpdatedAt: time.Now()}})
	if p, ok := usagePercent(sess, switching.WindowWeek); !ok || p < 14.99 || p > 15.01 {
		t.Errorf("week = %v, %v; want 15", p, ok)
	}

	panel.SaveWindows([]panel.ShareWindow{{Slug: "saif", FiveH: 0.06, SevenD: 0.15, UpdatedAt: time.Now().Add(-3 * 24 * time.Hour)}})
	if _, ok := usagePercent(sess, switching.WindowWeek); ok {
		t.Error("a reading three days old was acted on")
	}
}

// Setting a rule from inside a session stamps it with that session, and names
// what will happen in words.
func TestSettingARuleInsideASession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sharesPath, err := config.SharesFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sharesPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := panel.SaveShares(sharesPath, []panel.GatewayShare{{Account: "Saif", Slug: "saif", Gateway: "https://gw.example", Key: "k"}}); err != nil {
		t.Fatal(err)
	}
	dir, err := config.SessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Publish(dir, ruleSession()); err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(t.TempDir(), ".handoff-1.json")
	t.Setenv(switching.HandoffEnvVar, handoff)
	t.Setenv(switching.SupervisorEnvVar, strconv.Itoa(os.Getpid()))

	// A session launched by a clawdh from before rules has no Stop hook to
	// fire one in: refused, with the way out, rather than shown as pending.
	t.Setenv(switching.RulesEnvVar, "")
	if _, problem := setRule("saif", true, switching.WindowWeek, 5); !strings.Contains(problem, "clawdh hassandh --continue") {
		t.Errorf("an older session's problem = %q; want the restart command", problem)
	}
	if _, ok := switching.ReadRule(handoff); ok {
		t.Fatal("a rule was recorded in a session that cannot act on it")
	}
	t.Setenv(switching.RulesEnvVar, "1")

	msg, problem := setRule("saif", true, switching.WindowWeek, 5)
	if problem != "" {
		t.Fatal(problem)
	}
	if !strings.Contains(msg, "move this session to saif once HassanDH has used 5% of its week") {
		t.Errorf("message = %q", msg)
	}
	r, ok := switching.ReadRule(handoff)
	if !ok || r.Account != "saif" || !r.Shared || r.AtPercent != 5 || r.For != "n1" {
		t.Errorf("rule = %+v, %v; want it stamped with this session's nonce", r, ok)
	}

	// An account that is not shared with this machine is refused, not recorded.
	if _, problem := setRule("nobody", true, switching.WindowWeek, 5); problem == "" {
		t.Error("a rule for an unknown account was accepted")
	}
	// And `clawdh stay` takes the pending one back.
	if msg := stayMessage(handoff); !strings.Contains(msg, "no longer move to saif at 5% of the week") {
		t.Errorf("stay = %q", msg)
	}
	if _, ok := switching.ReadRule(handoff); ok {
		t.Error("the rule survived `clawdh stay`")
	}
}

// Outside a clawdh session there is nothing to move later, and it says how to
// start one rather than doing something surprising.
func TestSettingARuleOutsideASession(t *testing.T) {
	t.Setenv(switching.HandoffEnvVar, "")
	t.Setenv(switching.SupervisorEnvVar, "")
	if _, problem := setRule("saif", true, switching.WindowWeek, 5); !strings.Contains(problem, "Only a session started with clawdh") {
		t.Errorf("problem = %q", problem)
	}
}
