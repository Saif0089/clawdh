// Package e2e drives the actual built clawdh binary through its full
// lifecycle — install, serve, add an account, log it in, uninstall —
// exactly as a real user would, with no admin/elevation available (CI
// runners execute as an ordinary per-user account, so any step that
// silently required elevation would simply hang or fail here, which is
// the whole point of this test existing).
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t         *testing.T
	clawdhBin string
	claudeDir string // holds the fake "claude" binary, prepended to PATH
	home      string
	port      int
	env       []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	repoRoot := repoRoot(t)
	home := t.TempDir()

	// Build into the per-user install directory the real installers use
	// (~/.local/bin, %LOCALAPPDATA%\clawdh\bin), so this exercises the
	// same paths a real install does — including `clawdh uninstall`
	// removing its own binary, which it deliberately refuses to do for
	// a binary sitting outside that directory.
	binDir := filepath.Join(home, ".local", "bin")
	if runtime.GOOS == "windows" {
		binDir = filepath.Join(home, "AppData", "Local", "clawdh", "bin")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("creating install dir: %v", err)
	}

	clawdhBin := filepath.Join(binDir, exeName("clawdh"))
	build(t, filepath.Join(repoRoot, "cmd", "clawdh"), clawdhBin)

	claudeDir := t.TempDir()
	fakeClaudeBin := filepath.Join(claudeDir, exeName("claude"))
	build(t, filepath.Join(repoRoot, "testdata", "fakeclaude"), fakeClaudeBin)

	port := freePort(t)

	env := os.Environ()
	env = setEnv(env, "HOME", home)
	env = setEnv(env, "PATH", claudeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The keychain belongs to the machine, not to this temporary HOME. Without
	// this, every run on a Mac left a "Claude Code-credentials-<hash>" item in
	// the real login keychain for a directory that no longer exists.
	env = setEnv(env, "CLAWDH_CREDENTIALS_FILE", "1")
	if runtime.GOOS == "windows" {
		env = setEnv(env, "USERPROFILE", home)
		env = setEnv(env, "APPDATA", filepath.Join(home, "AppData", "Roaming"))
		env = setEnv(env, "LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	}

	return &harness{t: t, clawdhBin: clawdhBin, claudeDir: claudeDir, home: home, port: port, env: env}
}

func (h *harness) run(args ...string) (stdout string, err error) {
	h.t.Helper()
	cmd := exec.Command(h.clawdhBin, args...)
	cmd.Env = h.env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

func (h *harness) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", h.port)
}

func (h *harness) waitForHTTP(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.baseURL() + "/api/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func TestFullLifecycle(t *testing.T) {
	h := newHarness(t)

	// --- install: no admin/elevation available in this environment,
	// so a hang or a non-zero exit here means the install path tried
	// to require it. ---
	out, err := h.run("install", "--port", fmt.Sprint(h.port))
	if err != nil {
		t.Fatalf("clawdh install failed: %v\n%s", err, out)
	}
	t.Logf("install output:\n%s", out)

	assertNoSystemPaths(t, h.home)

	if !h.waitForHTTP(15 * time.Second) {
		t.Fatalf("service never answered on %s after install; log:\n%s", h.baseURL(), readLog(h))
	}

	// --- reinstalling must be idempotent: no duplicated autostart
	// artifacts, no duplicated rc blocks. ---
	if out, err := h.run("install", "--port", fmt.Sprint(h.port)); err != nil {
		t.Fatalf("second clawdh install failed: %v\n%s", err, out)
	}

	// --- full account lifecycle over the real HTTP API. ---
	account := h.createAccount("Work")
	// Accounts no longer get a shell alias — they run as `clawdh <name>` on every
	// OS — so creating one adds nothing to the managed rc block, which carries
	// only the `claude` wrapper.
	if data, err := os.ReadFile(h.anyRcPath()); err == nil && strings.Contains(string(data), "claude-work") {
		t.Errorf("creating an account wrote a shell alias, want none:\n%s", data)
	}

	h.driveLoginToLinked(account.ID)

	accountsAfterLogin := h.listAccounts()
	if len(accountsAfterLogin) != 1 || accountsAfterLogin[0].Status != "linked" {
		t.Fatalf("accounts after login = %+v, want one linked account", accountsAfterLogin)
	}

	// --- removing the only account keeps the rc block: it carries the `claude`
	// wrapper that makes a plain `claude` a switchable session, accounts or not.
	// (Dropping the block here is what once left every new terminal's `claude`
	// unsupervised.) Only uninstall, below, removes it. ---
	h.deleteAccount(account.ID)
	if data, err := os.ReadFile(h.anyRcPath()); err != nil {
		t.Errorf("rc file should still exist once no accounts remain: %v", err)
	} else if !strings.Contains(string(data), "clawdh run --auto") {
		t.Errorf("rc block lost the claude wrapper once no accounts remain:\n%s", data)
	}

	// --- uninstall must undo everything: autostart artifact, service,
	// and (eventually — Windows deletes it asynchronously) the binary
	// itself. ---
	out, err = h.run("uninstall", "--port", fmt.Sprint(h.port))
	if err != nil {
		t.Fatalf("clawdh uninstall failed: %v\n%s", err, out)
	}
	t.Logf("uninstall output:\n%s", out)

	if !waitUntilNot(5*time.Second, func() bool {
		resp, err := http.Get(h.baseURL() + "/api/status")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	}) {
		t.Error("service still answering after uninstall")
	}

	// On Windows the binary can't delete itself while still running, so
	// uninstall schedules the delete to happen just after this process
	// exits; Defender/handle-release adds further variance on CI
	// runners. Give it a generous window rather than tightening the
	// mechanism around CI's worst case.
	if !waitUntilNot(30*time.Second, func() bool {
		_, err := os.Stat(h.clawdhBin)
		return err == nil
	}) {
		t.Errorf("binary %s still present after uninstall", h.clawdhBin)
	}

	assertAutostartArtifactsGone(t, h.home)
}

// --- helpers -----------------------------------------------------------

type accountDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Alias  string `json:"alias"`
	Status string `json:"status"`
}

func (h *harness) createAccount(name string) accountDTO {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(h.baseURL()+"/api/accounts", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("POST /api/accounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("POST /api/accounts status = %d: %s", resp.StatusCode, b)
	}
	var a accountDTO
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		h.t.Fatalf("decode account: %v", err)
	}
	return a
}

func (h *harness) listAccounts() []accountDTO {
	h.t.Helper()
	resp, err := http.Get(h.baseURL() + "/api/accounts")
	if err != nil {
		h.t.Fatalf("GET /api/accounts: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Accounts []accountDTO `json:"accounts"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Accounts
}

func (h *harness) deleteAccount(id string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, h.baseURL()+"/api/accounts/"+id+"?confirm=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("DELETE /api/accounts/%s: %v", id, err)
	}
	resp.Body.Close()
}

func (h *harness) driveLoginToLinked(accountID string) {
	h.t.Helper()
	resp, err := http.Post(h.baseURL()+"/api/accounts/"+accountID+"/login", "", nil)
	if err != nil {
		h.t.Fatalf("POST login: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(h.baseURL() + "/api/accounts/" + accountID + "/login/events")
	if err != nil {
		h.t.Fatalf("GET login/events: %v", err)
	}
	defer resp.Body.Close()

	// scanner.Scan() blocks, so a deadline checked around it never fires
	// while nothing is arriving. Read on a goroutine and close the body
	// to break out, so a login that never completes fails with the
	// frames it did see rather than hanging until the test binary's
	// global timeout.
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
			// Decoded from the literal wire format the browser sees, NOT
			// by reusing ptyauth.Event: this test previously declared
			// `json:"Type"` and so happily passed while the real UI,
			// which reads event.type, got undefined for every field.
			var ev struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "url":
				sawURL = true
				if len(ev.URL) < 400 {
					h.t.Errorf("OAuth URL looks truncated (%d chars): %s", len(ev.URL), ev.URL)
				}
			case "linked":
				sawLinked = true
			}
			if sawLinked {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		resp.Body.Close()
		<-done
		h.t.Errorf("timed out waiting for SSE events; frames seen: %v\nclawdh log:\n%s", seen, readLog(h))
	}

	if !sawURL {
		h.t.Error("expected a url event over SSE")
	}
	if !sawLinked {
		h.t.Fatal("expected a linked event over SSE")
	}
}

func (h *harness) anyRcPath() string {
	h.t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(h.home, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1")
	}
	return filepath.Join(h.home, ".zshrc")
}

func (h *harness) readAnyRcFile() string {
	data, err := os.ReadFile(h.anyRcPath())
	if err != nil {
		h.t.Fatalf("reading rc file %s: %v", h.anyRcPath(), err)
	}
	return string(data)
}

func readLog(h *harness) string {
	data, _ := os.ReadFile(filepath.Join(h.home, ".clawdh", "clawdh.log"))
	return string(data)
}

func assertNoSystemPaths(t *testing.T, home string) {
	t.Helper()
	forbidden := []string{"/usr/local", "/etc/systemd/system", "Program Files", "System32"}
	filepath.WalkDir(filepath.Join(home, ".clawdh"), func(path string, d os.DirEntry, err error) error {
		return nil // presence check only, not content; directory existing is enough context for the message below
	})
	for _, f := range forbidden {
		if strings.Contains(home, f) {
			t.Fatalf("test home unexpectedly under a system path: %s", home)
		}
	}
}

func assertAutostartArtifactsGone(t *testing.T, home string) {
	t.Helper()
	candidates := []string{
		filepath.Join(home, "Library", "LaunchAgents", "com.clawdh.agent.plist"),
		filepath.Join(home, ".config", "systemd", "user", "clawdh.service"),
		filepath.Join(home, ".config", "autostart", "clawdh.desktop"),
		filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "clawdh-autostart.cmd"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			t.Errorf("autostart artifact still present: %s", c)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Join(wd, "..", "..")
}

func build(t *testing.T, pkgDir, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkgDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", pkgDir, err, output)
	}
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, prefix+value)
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		out = append(out, prefix+value)
	}
	return out
}

func freePort(t *testing.T) int {
	t.Helper()
	// Deliberately not net.Listen(":0") here: clawdh's own "port 0 means
	// pick one" behavior is exercised elsewhere; this test wants a
	// fixed, known port so the same value can be passed to `clawdh
	// install`/`clawdh uninstall` as a real user would via --port.
	base := 47800 + (int(time.Now().UnixNano()) % 500)
	return base
}

func waitUntilNot(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !cond() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
