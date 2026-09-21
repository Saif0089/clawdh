package statusline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func loadJSON(t *testing.T, path string) map[string]any {
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

func statusLineOf(t *testing.T, settings string) map[string]any {
	t.Helper()
	sl, _ := loadJSON(t, settings)["statusLine"].(map[string]any)
	return sl
}

func mtime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixNano()
}

// The real shape: the person has a status line of their own, with padding,
// among other settings. clawdh wraps it — theirs is kept whole, its padding
// stays on the wrapper, nothing else in the file moves.
func TestEnsureWrapsTheirStatusLineAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "clawdh", "statusline.json")
	os.WriteFile(settings, []byte(`{
      "model": "sonnet",
      "statusLine": {"type":"command","command":"python3 \"/Users/me/.claude/usage-aware.py\" statusline 2>/dev/null || true","padding":0}
    }`), 0o600)

	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	cfg := loadJSON(t, settings)
	if cfg["model"] != "sonnet" {
		t.Error("unrelated settings must be preserved")
	}
	sl := statusLineOf(t, settings)
	if sl["command"] != `"/usr/local/bin/clawdh" statusline` || sl["type"] != "command" {
		t.Errorf("statusLine = %v, want clawdh's command", sl)
	}
	if sl["padding"] != float64(0) {
		t.Errorf("padding = %v, want theirs (0) kept", sl["padding"])
	}
	theirs, ok := Saved(save)
	if !ok || !strings.Contains(theirs["command"].(string), "usage-aware.py") || theirs["padding"] != float64(0) {
		t.Errorf("saved status line = %v, %v; want theirs whole", theirs, ok)
	}

	// Steady state: nothing is written.
	before := mtime(t, settings)
	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	if mtime(t, settings) != before {
		t.Error("a second Ensure with nothing to do must not rewrite settings.json")
	}
	if theirs, _ := Saved(save); !strings.Contains(theirs["command"].(string), "usage-aware.py") {
		t.Error("a second Ensure must not overwrite the saved status line with clawdh's own")
	}
}

// Someone with no status line gets clawdh's alone, and Restore takes it away
// again rather than leaving a wrapper around nothing.
func TestEnsureWithNoStatusLineAndRestore(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "statusline.json")
	os.WriteFile(settings, []byte(`{"model":"opus"}`), 0o600)

	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	if sl := statusLineOf(t, settings); sl["command"] != `"/usr/local/bin/clawdh" statusline` {
		t.Errorf("statusLine = %v", sl)
	}
	if _, hasPadding := statusLineOf(t, settings)["padding"]; hasPadding {
		t.Error("no padding of theirs to keep, so none should be invented")
	}
	if theirs, ok := Saved(save); !ok || theirs != nil {
		t.Errorf("saved = %v, %v; want a record that they had none", theirs, ok)
	}

	if err := Restore(settings, save); err != nil {
		t.Fatal(err)
	}
	cfg := loadJSON(t, settings)
	if _, still := cfg["statusLine"]; still {
		t.Errorf("statusLine still set after restore: %v", cfg["statusLine"])
	}
	if cfg["model"] != "opus" {
		t.Error("restore must keep the rest of the file")
	}
	if _, err := os.Stat(save); !os.IsNotExist(err) {
		t.Error("the saved file should be gone after restore")
	}
}

// Restore puts theirs back exactly.
func TestRestoreReturnsTheirStatusLine(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "statusline.json")
	os.WriteFile(settings, []byte(`{"statusLine":{"type":"command","command":"my-line.sh","padding":2}}`), 0o600)
	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	if err := Restore(settings, save); err != nil {
		t.Fatal(err)
	}
	sl := statusLineOf(t, settings)
	if sl["command"] != "my-line.sh" || sl["padding"] != float64(2) {
		t.Errorf("restored statusLine = %v, want theirs back", sl)
	}
}

// A status line the person changed by hand after clawdh wrapped theirs is
// theirs: the next launch wraps the new one, and an uninstall leaves it alone.
func TestAStatusLineChangedByHandIsTheirs(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "statusline.json")
	os.WriteFile(settings, []byte(`{"statusLine":{"type":"command","command":"old.sh"}}`), 0o600)
	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(settings, []byte(`{"statusLine":{"type":"command","command":"new.sh"}}`), 0o600)

	if err := Restore(settings, save); err != nil {
		t.Fatal(err)
	}
	if sl := statusLineOf(t, settings); sl["command"] != "new.sh" {
		t.Errorf("restore replaced a status line that was not clawdh's: %v", sl)
	}

	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	if theirs, _ := Saved(save); theirs["command"] != "new.sh" {
		t.Errorf("saved = %v, want the status line they changed to", theirs)
	}
}

// The binary moved (an update, a different install path): the command is
// refreshed and the saved status line is not touched.
func TestEnsureRefreshesAStaleBinaryPath(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "statusline.json")
	os.WriteFile(settings, []byte(`{"statusLine":{"type":"command","command":"mine.sh","padding":1}}`), 0o600)
	if err := Ensure(settings, save, "/old/clawdh"); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(settings, save, "/new/clawdh"); err != nil {
		t.Fatal(err)
	}
	sl := statusLineOf(t, settings)
	if sl["command"] != `"/new/clawdh" statusline` || sl["padding"] != float64(1) {
		t.Errorf("statusLine = %v, want the new path with their padding", sl)
	}
	if theirs, _ := Saved(save); theirs["command"] != "mine.sh" {
		t.Errorf("saved = %v, want theirs untouched", theirs)
	}
}

// A settings.json that does not parse is never clobbered, and a missing one is
// created.
func TestEnsureNeverClobbersAndCreates(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	save := filepath.Join(dir, "statusline.json")
	os.WriteFile(settings, []byte(`{not json`), 0o600)
	if err := Ensure(settings, save, "/usr/local/bin/clawdh"); err == nil {
		t.Error("want an error for an unparseable settings.json")
	}
	if data, _ := os.ReadFile(settings); string(data) != `{not json` {
		t.Error("an unparseable settings.json must be left as it was")
	}

	missing := filepath.Join(dir, "none", "settings.json")
	if err := Ensure(missing, save, "/usr/local/bin/clawdh"); err != nil {
		t.Fatal(err)
	}
	if sl := statusLineOf(t, missing); sl["command"] != `"/usr/local/bin/clawdh" statusline` {
		t.Errorf("statusLine in a created settings.json = %v", sl)
	}
}

// isOurs must not mistake a person's own script that takes a "statusline"
// argument for clawdh's command.
func TestIsOurs(t *testing.T) {
	for cmd, want := range map[string]bool{
		`"/usr/local/bin/clawdh" statusline`:                     true,
		`  "/Users/me/.local/bin/clawdh" statusline `:            true,
		`"C:\Users\me\clawdh.exe" statusline`:                    true,
		`python3 "/Users/me/.claude/usage-aware.py" statusline`:  false,
		`"/usr/local/bin/other" statusline`:                      false,
		`"/usr/local/bin/clawdh" statusline 2>/dev/null || true`: false,
		``: false,
	} {
		if _, got := isOurs(cmd); got != want {
			t.Errorf("isOurs(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestBadge(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name      string
		env       map[string]string
		installed string
		want      string
	}{
		{"no supervisor", map[string]string{}, "main@abc1234", ""},
		{"a session", map[string]string{VersionEnvVar: "main@abc1234", AccountEnvVar: "HassanDH"}, "main@abc1234", "clawdh main@abc1234 · HassanDH"},
		{"updated underneath", map[string]string{VersionEnvVar: "main@abc1234", AccountEnvVar: "HassanDH"}, "main@def5678", "clawdh main@abc1234 (update ready) · HassanDH"},
		{"a share", map[string]string{VersionEnvVar: "v1.2.0", AccountEnvVar: "hassanyasin"}, "v1.2.0", "clawdh v1.2.0 · hassanyasin"},
	} {
		if got := Badge(env(tc.env), tc.installed); got != tc.want {
			t.Errorf("%s: Badge = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Render runs their command with the stdin Claude Code passed and puts the
// badge in front of the first line only.
func TestRender(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	theirs := map[string]any{"type": "command", "command": `read line; echo "usage: $line"; echo "second row"`}
	got := Render(theirs, []byte("42%\n"), "clawdh main@abc1234 · Work")
	if got != "clawdh main@abc1234 · Work · usage: 42%\nsecond row" {
		t.Errorf("Render = %q", got)
	}

	// No badge (no supervisor): their line, untouched.
	if got := Render(theirs, []byte("7%\n"), ""); got != "usage: 7%\nsecond row" {
		t.Errorf("Render without a badge = %q", got)
	}
	// No command of theirs: the badge alone.
	if got := Render(nil, nil, "clawdh main@abc1234 · Work"); got != "clawdh main@abc1234 · Work" {
		t.Errorf("Render with nothing to wrap = %q", got)
	}
	// Their command failing is not the badge's problem.
	if got := Render(map[string]any{"command": "exit 3"}, nil, "clawdh dev · Work"); got != "clawdh dev · Work" {
		t.Errorf("Render over a failing command = %q", got)
	}
}
