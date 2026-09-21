package shellrc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSyncerWritesAliasesToAllRcFiles(t *testing.T) {
	home := t.TempDir()
	s := NewSyncer(home)

	entries := []AliasEntry{
		{Alias: "claude-work", ConfigDir: filepath.Join(home, ".clawdh/accounts/work")},
		{Alias: "claude-personal", ConfigDir: filepath.Join(home, ".clawdh/accounts/personal")},
	}
	if err := s.Sync(entries); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	for shell, path := range RcPaths(home) {
		has, err := HasBlock(path)
		if err != nil {
			t.Fatalf("HasBlock(%s): %v", path, err)
		}
		if !has {
			t.Errorf("%s: expected managed block in %s", shell, path)
		}
		data, _ := os.ReadFile(path)
		if !contains(string(data), "claude-work") || !contains(string(data), "claude-personal") {
			t.Errorf("%s: expected both aliases in %s, got %q", shell, path, data)
		}
	}
}

// A sync with no entries drops the aliases but keeps the block, because the
// block is also where the `claude` wrapper lives — and every caller now syncs
// with no entries. Losing the wrapper meant a plain `claude` was no longer
// supervised, so `clawdh <name>` typed into it could not switch it.
func TestSyncerWithNoAccountsKeepsTheClaudeWrapper(t *testing.T) {
	home := t.TempDir()
	s := NewSyncer(home)

	entries := []AliasEntry{{Alias: "claude-work", ConfigDir: home}}
	if err := s.Sync(entries); err != nil {
		t.Fatalf("Sync (populate): %v", err)
	}
	if err := s.Sync(nil); err != nil {
		t.Fatalf("Sync (empty): %v", err)
	}

	for shell, path := range RcPaths(home) {
		has, err := HasBlock(path)
		if err != nil {
			t.Fatalf("HasBlock(%s): %v", path, err)
		}
		if !has {
			t.Fatalf("%s: the managed block must survive an empty sync — it carries the claude wrapper", shell)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "claude-work") {
			t.Errorf("%s: alias still present after an empty sync", shell)
		}
		if !strings.Contains(string(data), "run --auto") {
			t.Errorf("%s: claude wrapper missing after an empty sync:\n%s", shell, data)
		}
	}
}

func TestSyncerRemoveAllLeavesUserContentIntact(t *testing.T) {
	home := t.TempDir()
	s := NewSyncer(home)

	// Seed one rc file with pre-existing user content, matching this
	// OS's actual rc path so RcPaths and this test agree.
	var seeded string
	for _, path := range RcPaths(home) {
		seeded = path
		break
	}
	original := "# my own aliases\nalias g='git'\n"
	if err := os.MkdirAll(filepath.Dir(seeded), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(seeded, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.Sync([]AliasEntry{{Alias: "claude-work", ConfigDir: home}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	got, err := os.ReadFile(seeded)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != original {
		t.Errorf("content = %q, want original %q", got, original)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRenderBodyEscapesForEachShell(t *testing.T) {
	entries := []AliasEntry{{Alias: "claude-work", ConfigDir: "/home/me/.clawdh/accounts/work", Account: "work"}}

	// Each entry point routes through clawdh so the session it starts can be
	// switched from inside, and keeps a direct-launch fallback with the
	// credential store scoped and the config dir unset.
	cases := map[Shell][]string{
		Bash:       {`command clawdh work "$@"`, `CLAUDE_SECURESTORAGE_CONFIG_DIR="/home/me/.clawdh/accounts/work" command claude "$@"`},
		Zsh:        {`command clawdh work "$@"`, `CLAUDE_SECURESTORAGE_CONFIG_DIR="/home/me/.clawdh/accounts/work" command claude "$@"`},
		Fish:       {`command clawdh work $argv`, `CLAUDE_SECURESTORAGE_CONFIG_DIR="/home/me/.clawdh/accounts/work" claude $argv`},
		PowerShell: {`clawdh 'work' @args`, `$env:CLAUDE_SECURESTORAGE_CONFIG_DIR = '/home/me/.clawdh/accounts/work'`},
	}
	for shell, wants := range cases {
		body := RenderBody(shell, entries)
		for _, want := range wants {
			if !contains(body, want) {
				t.Errorf("%s: body %q does not contain %q", shell, body, want)
			}
		}
	}
}

// An account with no name cannot be handed to clawdh, so the entry point falls
// back to the plain launch rather than rendering `clawdh` with no argument.
func TestRenderBodyWithoutAnAccountFallsBackToADirectLaunch(t *testing.T) {
	entries := []AliasEntry{{Alias: "claude-work", ConfigDir: "/home/me/.clawdh/accounts/work"}}
	for _, shell := range []Shell{Bash, Zsh, Fish, PowerShell} {
		body := RenderBody(shell, entries)
		if contains(body, "clawdh  ") || contains(body, "clawdh ''") || contains(body, `clawdh ""`) {
			t.Errorf("%s: rendered clawdh with an empty account: %q", shell, body)
		}
		if !contains(body, "/home/me/.clawdh/accounts/work") {
			t.Errorf("%s: the direct fallback lost the config dir: %q", shell, body)
		}
	}
}

// TestRenderBodyNeverEmitsTheConfigDirVariable is the regression guard for
// the swap: an alias that sets CLAUDE_CONFIG_DIR isolates the whole config
// directory, which is precisely what sharing ~/.claude is meant to stop.
// Only the unset (`env -u` / Remove-Item) mention of that name is allowed.
func TestRenderBodyNeverEmitsTheConfigDirVariable(t *testing.T) {
	entries := []AliasEntry{{Alias: "claude-work", ConfigDir: "/home/me/.clawdh/accounts/work"}}

	for _, shell := range []Shell{Bash, BashLogin, Zsh, Fish, PowerShell, PowerShellDesktop} {
		body := RenderBody(shell, entries)
		for _, assignment := range []string{
			`CLAUDE_CONFIG_DIR="`,
			`CLAUDE_CONFIG_DIR='`,
			`CLAUDE_CONFIG_DIR =`,
		} {
			if contains(body, assignment) {
				t.Errorf("%s: body assigns the config-dir variable (%q), which would keep the "+
					"session isolated from the shared ~/.claude: %q", shell, assignment, body)
			}
		}
		if !contains(body, "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
			t.Errorf("%s: body does not scope the credential store at all: %q", shell, body)
		}
	}
}

func TestRcPathsMatchesCurrentOS(t *testing.T) {
	paths := RcPaths(t.TempDir())
	if runtime.GOOS == "windows" {
		// Both PowerShells: 5.1 ships with Windows and is what
		// `powershell` opens, 7+ is what many developers install.
		for _, shell := range []Shell{PowerShell, PowerShellDesktop} {
			if _, ok := paths[shell]; !ok {
				t.Errorf("missing %s in RcPaths on windows: %v", shell, paths)
			}
		}
	} else {
		for _, shell := range []Shell{Bash, Zsh, Fish, PowerShell} {
			if _, ok := paths[shell]; !ok {
				t.Errorf("missing %s in RcPaths on %s", shell, runtime.GOOS)
			}
		}
	}
}

// TestSyncerWritesLoginShellFilesOnlyWhenPresent covers bash login
// shells — what macOS Terminal.app starts and what ssh gives you. They
// read ~/.bash_profile or ~/.profile and never look at ~/.bashrc, so an
// alias written only to .bashrc isn't there for those users. Creating
// those files where they don't exist is not an option: a new
// ~/.bash_profile stops bash reading ~/.profile entirely.
func TestSyncerWritesLoginShellFilesOnlyWhenPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login-shell startup files are a unix concern")
	}
	home := t.TempDir()
	profile := filepath.Join(home, ".bash_profile")
	if err := os.WriteFile(profile, []byte("# my login shell setup\n"), 0o644); err != nil {
		t.Fatalf("seeding .bash_profile: %v", err)
	}

	s := NewSyncer(home)
	if err := s.Sync([]AliasEntry{{Alias: "claude-work", ConfigDir: home}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("reading .bash_profile: %v", err)
	}
	if !contains(string(data), "claude-work") {
		t.Errorf("expected the alias in an existing .bash_profile, got:\n%s", data)
	}
	if !contains(string(data), "# my login shell setup") {
		t.Error("existing content was lost")
	}

	// ~/.profile did not exist, and must not have been created.
	if _, err := os.Stat(filepath.Join(home, ".profile")); !os.IsNotExist(err) {
		t.Errorf("~/.profile was created; stat err = %v", err)
	}

	if err := s.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	data, _ = os.ReadFile(profile)
	if string(data) != "# my login shell setup\n" {
		t.Errorf(".bash_profile not restored, got:\n%s", data)
	}
}
