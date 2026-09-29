package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadSettings(t *testing.T, path string) map[string]any {
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

// upsCommands returns every UserPromptSubmit command string in a settings map.
func upsCommands(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	var out []string
	hooks, _ := cfg["hooks"].(map[string]any)
	ups, _ := hooks["UserPromptSubmit"].([]any)
	for _, e := range ups {
		if cmd, ok := entryCommand(e); ok {
			out = append(out, cmd)
		}
	}
	return out
}

func TestEnsureHookPreservesExistingHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// The user's real shape: a model setting and their own UserPromptSubmit hook.
	os.WriteFile(path, []byte(`{
      "model": "sonnet",
      "hooks": {
        "UserPromptSubmit": [
          {"hooks": [{"type":"command","command":"/Users/me/.claude/hooks/fable-nudge.sh","timeout":5}]}
        ]
      }
    }`), 0o600)

	if err := EnsureHooks(path, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	cfg := loadSettings(t, path)
	if cfg["model"] != "sonnet" {
		t.Error("unrelated settings must be preserved")
	}
	cmds := upsCommands(t, cfg)
	if len(cmds) != 2 {
		t.Fatalf("want 2 UserPromptSubmit hooks (theirs + clawdh), got %d: %v", len(cmds), cmds)
	}
	foundFable, foundClawdh := false, false
	for _, c := range cmds {
		if c == "/Users/me/.claude/hooks/fable-nudge.sh" {
			foundFable = true
		}
		if c == HookCommand("/usr/local/bin/clawdh") {
			foundClawdh = true
		}
	}
	if !foundFable || !foundClawdh {
		t.Errorf("both hooks must be present: fable=%v clawdh=%v", foundFable, foundClawdh)
	}
}

func TestEnsureHookIsIdempotentAndDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600)

	if err := EnsureHooks(path, "/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)

	// Second call must be a pure no-op — byte-identical, no reformat/churn.
	if err := EnsureHooks(path, "/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("second EnsureHook must not rewrite the file:\n%s\n---\n%s", first, second)
	}
}

func TestEnsureHookRefreshesStalePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{}`), 0o600)
	if err := EnsureHooks(path, "/old/path/clawdh"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHooks(path, "/new/path/clawdh"); err != nil {
		t.Fatal(err)
	}
	cmds := upsCommands(t, loadSettings(t, path))
	if len(cmds) != 1 || cmds[0] != HookCommand("/new/path/clawdh") {
		t.Errorf("stale clawdh hook should be refreshed to one entry with the new path, got %v", cmds)
	}
}

func TestRemoveHookLeavesOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{
      "hooks": {"UserPromptSubmit": [
        {"hooks":[{"type":"command","command":"/Users/me/.claude/hooks/fable-nudge.sh","timeout":5}]}
      ]}
    }`), 0o600)
	EnsureHooks(path, "/bin/clawdh")

	if err := RemoveHooks(path); err != nil {
		t.Fatal(err)
	}
	cmds := upsCommands(t, loadSettings(t, path))
	if len(cmds) != 1 || cmds[0] != "/Users/me/.claude/hooks/fable-nudge.sh" {
		t.Errorf("remove must delete only clawdh's hook, got %v", cmds)
	}
}

// Every clawdh hook is installed — the switch, the after-answer move, and the
// note that tells Claude how to set one — beside the user's own hooks on the
// same events, and a second run writes nothing.
func TestEnsureHooksInstallsAllThreeBesideTheUsersOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mine := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/my-hook stop"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"remember prompt"}]}]},"model":"opus"}`
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHooks(path, "/opt/clawdh/clawdh"); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	hooks := cfg["hooks"].(map[string]any)
	commands := func(event string) []string {
		var out []string
		for _, e := range hooks[event].([]any) {
			if c, ok := entryCommand(e); ok {
				out = append(out, c)
			}
		}
		return out
	}
	want := map[string][]string{
		"UserPromptSubmit": {"remember prompt", `"/opt/clawdh/clawdh" hook user-prompt-submit`},
		"Stop":             {"/usr/local/bin/my-hook stop", `"/opt/clawdh/clawdh" hook stop`},
		"SessionStart":     {`"/opt/clawdh/clawdh" hook session-start`},
	}
	for event, w := range want {
		if got := commands(event); strings.Join(got, "|") != strings.Join(w, "|") {
			t.Errorf("%s hooks = %q, want %q", event, got, w)
		}
	}
	if cfg["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}

	before, _ := os.Stat(path)
	if err := EnsureHooks(path, "/opt/clawdh/clawdh"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a second EnsureHooks rewrote settings.json although nothing changed")
	}

	// Uninstall takes clawdh's three and nothing of the user's.
	if err := RemoveHooks(path); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	cfg = nil
	_ = json.Unmarshal(data, &cfg)
	hooks = cfg["hooks"].(map[string]any)
	if got := commands("Stop"); len(got) != 1 || got[0] != "/usr/local/bin/my-hook stop" {
		t.Errorf("after RemoveHooks, Stop = %q; want only the user's own", got)
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("RemoveHooks left clawdh's SessionStart hook behind")
	}
}
