// Package guard holds repository-wide invariants that no single package can
// assert about itself.
package guard

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is this package's directory, two levels down from the module root.
const repoRoot = "../.."

// TestClawdhNeverTouchesTheCredentialStore is the guard on the bug that cost two
// real logins.
//
// clawdh used to keep a copy of each session's credentials so a switch could be a
// write rather than a restart. Making that work meant re-deriving Claude Code's
// own Keychain item name and writing it through `security -i`, which truncates
// at 4095 bytes: a real store is larger than that once MCP logins are in it, so
// the item was replaced with a fragment that still parsed, and the mirror then
// copied the fragment over the account's own store. Both managed accounts were
// unrecoverable.
//
// The fix was to delete the whole arrangement. clawdh reads an account's login
// only through Claude Code itself (`claude auth status`) or from the plain
// credentials file, and writes one never. This test is what keeps it deleted:
// the strings below cannot reappear in shipped code without failing the build.
//
// Comments are exempt on purpose — internal/accounts/env.go documents the
// derivation to explain why an empty CLAUDE_SECURESTORAGE_CONFIG_DIR is
// dangerous, and that explanation is worth keeping.
func TestClawdhNeverWritesTheCredentialStore(t *testing.T) {
	// The rule this enforces is narrower than "never touch the Keychain": it is
	// "never WRITE a credential store". Writing is what destroyed real logins —
	// `security -i` truncating a store at 4 KB, the mirror copying the fragment
	// back. Reading one to display usage, or to capture a login for the panel to
	// lend, is safe and necessary, and is allowed. Only the write and delete
	// primitives are banned.
	// Banned is the WRITE of credential DATA — the thing that truncated a store
	// and copied the fragment back over real logins. Reading a login (to show
	// usage, or to capture it for the panel) and deleting a specific one (to
	// revoke it) are both safe and necessary, and are allowed.
	banned := map[string]string{
		"add-generic-password": "writing Claude Code's Keychain item",
		"security -i":          "writing the Keychain from stdin (the 4 KB truncation bug)",
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		// Tests may name these strings; shipped code may not.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Mode 0 leaves comments out of the tree entirely.
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parsing %s: %v", path, perr)
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			for needle, why := range banned {
				if strings.Contains(value, needle) {
					rel, _ := filepath.Rel(repoRoot, path)
					t.Errorf("%s:%d: %q is back — %s.\n"+
						"clawdh must never WRITE Claude Code's credential store; this is the class of code that destroyed two real logins.",
						rel, fset.Position(lit.Pos()).Line, needle, why)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCredstorePackageStaysDeleted is the blunter half of the same guard: the
// package that owned every credential write must not come back under its old
// name and quietly reacquire callers.
func TestCredstorePackageStaysDeleted(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot, "internal", "credstore")); !os.IsNotExist(err) {
		t.Error("internal/credstore is back. It existed to write credential stores, which clawdh no longer does.")
	}
}

// spawnsOnPurpose are the files allowed to start a program with plain os/exec in
// code that runs on Windows, each for a reason a person could point at: the
// window is the point.
var spawnsOnPurpose = map[string]string{
	"internal/cli/cmd_exec.go":          "hands the person's own terminal to claude",
	"internal/cli/cmd_run.go":           "hands the person's own terminal to claude",
	"internal/termlauncher/launcher.go": "opens a terminal because the person pressed the button for one",
	"internal/winproc/winproc.go":       "is the wrapper every other spawn goes through",
}

// TestNoBackgroundProgramOpensAWindowOnWindows is the guard on the terminal and
// PowerShell windows that opened on people's screens around an update.
//
// On Windows every helper program clawdh starts in the background (PowerShell
// for the "updated" notification, schtasks, taskkill, claude's .cmd shim, the
// restarted server itself) is a console program, and Windows opens a console
// window for one unless it is told not to. Nothing about that shows on macOS or
// Linux, and clawdh's own Windows test runs cannot see a window, so the rule is
// held here at the source: in any file that is part of a Windows build, a
// program is started through winproc, which sets CREATE_NO_WINDOW, and a plain
// os/exec call has to be a file listed above with its reason.
func TestNoBackgroundProgramOpensAWindowOnWindows(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "windows", "amd64", false

	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "test", "web":
				if path != repoRoot {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Only files a Windows build compiles: build tags and _GOOS suffixes decide.
		if ok, merr := ctx.MatchFile(filepath.Dir(path), d.Name()); merr != nil || !ok {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parsing %s: %v", path, perr)
			return nil
		}
		// The local name os/exec is imported under, if it is imported at all.
		execName := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
				execName = "exec"
				if imp.Name != nil {
					execName = imp.Name.Name
				}
			}
		}
		if execName == "" {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		if _, ok := spawnsOnPurpose[rel]; ok {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != execName || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
				return true
			}
			t.Errorf("%s:%d: exec.%s starts a program with no window flags, so on Windows it can open a console window.\n"+
				"Use winproc.%s (internal/winproc), which starts it with CREATE_NO_WINDOW. If the window is the point "+
				"(a terminal the person asked for), list the file in spawnsOnPurpose with the reason.",
				rel, fset.Position(call.Pos()).Line, sel.Sel.Name, sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
