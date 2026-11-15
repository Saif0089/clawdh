package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"clawdh/internal/accounts"
)

func TestParseTrigger(t *testing.T) {
	for _, tc := range []struct {
		prompt     string
		wantName   string
		wantShared bool
		wantOK     bool
	}{
		{"clawdh ehti", "ehti", false, true},
		{"  clawdh   ehti  ", "ehti", false, true},
		{"clawdh switch ehti", "ehti", false, true},
		{"clawdh shared ehtisham", "ehtisham", true, true}, // shared switch
		{"clawdh claude-ehti", "claude-ehti", false, true},
		{"/clawdh ehti", "", false, false},           // slash never reaches the hook as this shape
		{"clawdh", "", false, false},                 // no name
		{"clawdh ehti now", "", false, false},        // extra words → a real prompt
		{"clawdh shared ehti now", "", false, false}, // extra words → a real prompt
		{"please run clawdh ehti", "", false, false}, // sentence
		{"what does clawdh do", "", false, false},
		{"", "", false, false},
	} {
		name, shared, ok := ParseTrigger(tc.prompt)
		if ok != tc.wantOK || name != tc.wantName || shared != tc.wantShared {
			t.Errorf("ParseTrigger(%q) = (%q,shared=%v,%v), want (%q,shared=%v,%v)", tc.prompt, name, shared, ok, tc.wantName, tc.wantShared, tc.wantOK)
		}
	}
}

func TestResolveAccount(t *testing.T) {
	list := []accounts.Account{
		{ID: "ehti", Slug: "ehti", Alias: "claude-ehti"},
		{ID: "default", Slug: "default", Alias: "claude", Name: "saif@devhouse.co"},
	}
	for _, tc := range []struct{ name, wantID string }{
		{"ehti", "ehti"},
		{"EHTI", "ehti"},        // case-insensitive
		{"claude-ehti", "ehti"}, // full alias
		{"default", "default"},  // slug
		{"claude", "default"},   // alias
	} {
		got, ok := ResolveAccount(list, tc.name)
		if !ok || got.ID != tc.wantID {
			t.Errorf("ResolveAccount(%q) = (%q,%v), want %q", tc.name, got.ID, ok, tc.wantID)
		}
	}
	if _, ok := ResolveAccount(list, "nope"); ok {
		t.Error("unknown name should not resolve")
	}
}

func TestHandoffRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.json")

	if _, ok := ReadHandoff(path); ok {
		t.Error("no handoff should exist yet")
	}
	if err := WriteHandoff(path, Handoff{Account: "ehti", SessionID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	h, ok := ReadHandoff(path)
	if !ok || h.Account != "ehti" || h.SessionID != "s-1" {
		t.Fatalf("round trip failed: %+v ok=%v", h, ok)
	}
	ClearHandoff(path)
	if _, ok := ReadHandoff(path); ok {
		t.Error("handoff should be gone after ClearHandoff")
	}
	ClearHandoff(path) // clearing a missing file is not an error
}

func TestBlockDecisionJSON(t *testing.T) {
	data, err := BlockDecisionJSON("Switching to ehti…")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["decision"] != "block" {
		t.Errorf("decision = %v, want block", m["decision"])
	}
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if hso["hookEventName"] != "UserPromptSubmit" || hso["suppressOriginalPrompt"] != true {
		t.Errorf("hookSpecificOutput wrong: %v", hso)
	}
}

func TestResumeArgs(t *testing.T) {
	got := ResumeArgs("sess-1", true)
	want := []string{"--resume", "sess-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs(recorded) = %v, want %v", got, want)
	}
	// Switched before its first message: nothing to resume, and asking would
	// take the relaunched session down with "No conversation found" — so a
	// clean start, but keeping the id the ledger and the editor already know.
	if got, want := ResumeArgs("sess-1", false), []string{"--session-id", "sess-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs(unrecorded) = %v, want %v", got, want)
	}
	if got := ResumeArgs("  ", false); !reflect.DeepEqual(got, []string{"--continue"}) {
		t.Errorf("ResumeArgs(no id) = %v, want [--continue]", got)
	}
}

func TestWithoutSessionArgs(t *testing.T) {
	// What the VS Code extension launches with, plus every other spelling.
	in := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--session-id=abc", "--resume=def", "--session-id", "ghi", "--resume", "jkl", "-r", "mno",
		"--continue", "-c", "--fork-session", "--permission-mode", "default",
	}
	want := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--permission-mode", "default"}
	if got := WithoutSessionArgs(in); !reflect.DeepEqual(got, want) {
		t.Errorf("WithoutSessionArgs = %v, want %v", got, want)
	}
	if got := WithoutSessionArgs(nil); len(got) != 0 {
		t.Errorf("WithoutSessionArgs(nil) = %v, want empty", got)
	}
}

func TestHasTranscript(t *testing.T) {
	claudeDir := t.TempDir()
	projects := filepath.Join(claudeDir, "projects", "-Users-someone-code")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasTranscript(claudeDir, "sess-1") {
		t.Error("a recorded session should be found whatever directory it was in")
	}
	if HasTranscript(claudeDir, "sess-2") {
		t.Error("a session with no transcript must not be reported as recorded")
	}
	if HasTranscript(claudeDir, "") {
		t.Error("an empty session id is not a recorded session")
	}
}

// A session first started with its own prompt (`clawdh work "fix x"`) already
// sent it. A relaunch that kept it would replay it, and beside the relaunch's own
// first prompt it would be two positionals: Claude Code reads the first and
// silently ignores the rest, so the continue prompt would be the one lost.
func TestWithoutPromptArgsDropsOnlyThePositionalPrompt(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"no prompt", []string{"--dangerously-skip-permissions"}, []string{"--dangerously-skip-permissions"}},
		{"bare prompt", []string{"fix x"}, []string{}},
		{"prompt after boolean flags", []string{"--dangerously-skip-permissions", "-c", "fix x"}, []string{"--dangerously-skip-permissions", "-c"}},
		{"values are not prompts", []string{"--model", "opus", "--effort", "high", "fix x"}, []string{"--model", "opus", "--effort", "high"}},
		{"prompt before flags", []string{"fix x", "--model", "opus"}, []string{"--model", "opus"}},
		{"flag=value form", []string{"--model=opus", "fix x"}, []string{"--model=opus"}},
		{"variadic flag keeps all its values", []string{"--add-dir", "a", "b", "--model", "opus"}, []string{"--add-dir", "a", "b", "--model", "opus"}},
		{"optional value is taken as the value", []string{"--debug", "api", "--verbose", "fix x"}, []string{"--debug", "api", "--verbose"}},
		{"everything after -- is positional", []string{"--verbose", "--", "fix x", "and y"}, []string{"--verbose"}},
		{"an unknown flag keeps what may be its value", []string{"--new-flag", "v", "fix x"}, []string{"--new-flag", "v"}},
	} {
		got := WithoutPromptArgs(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: WithoutPromptArgs(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
