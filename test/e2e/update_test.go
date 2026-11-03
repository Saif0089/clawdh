package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clawdh/internal/updater"
)

// TestAutoUpdateInstallsAndRestartsIntoIt drives the whole automatic
// update through the real binary: a real release server, a real
// download, a real checksum, the real replace-and-restart.
//
// The claim being tested is the one nobody can verify by reading the
// code — that after all of it, the clawdh answering on the port is the
// *new* build. That is why the published binary is stamped with a
// version of its own: /api/status reporting it can only happen if the
// swap and the restart both worked.
func TestAutoUpdateInstallsAndRestartsIntoIt(t *testing.T) {
	h := newHarness(t)

	const publishedVersion = "v9.9.9-published"
	published := buildStamped(t, publishedVersion)
	release := startReleaseServer(t, published, time.Now())

	// The installed binary is deliberately older than the release, so
	// the "never install something older" rule lets this one through.
	older := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(h.clawdhBin, older, older); err != nil {
		t.Fatalf("ageing the installed binary: %v", err)
	}
	beforeSum := sha256File(t, h.clawdhBin)

	h.env = setEnv(h.env, "CLAWDH_UPDATE_API", release.URL)
	// Check straight away rather than after the minute a real machine
	// waits, and never pop a desktop notification from a test run.
	h.env = setEnv(h.env, "CLAWDH_UPDATE_DELAY", "0s")
	h.env = setEnv(h.env, "CLAWDH_NOTIFY", "0")

	out, err := h.run("install", "--port", fmt.Sprint(h.port))
	if err != nil {
		t.Fatalf("clawdh install failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { stopAndRelease(t, h) })

	if !h.waitForHTTP(15 * time.Second) {
		t.Fatalf("service never answered; log:\n%s", readLog(h))
	}
	pidBefore := statusPID(h.baseURL())

	// The update replaces the binary, then the server restarts into it.
	// Both halves have to land, and only the second is observable from
	// outside — so wait for the version the published build reports.
	deadline := time.Now().Add(60 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = statusVersion(h.baseURL()); got == publishedVersion {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got != publishedVersion {
		t.Fatalf("version answering on the port = %q, want the published %q\nlog:\n%s",
			got, publishedVersion, readLog(h))
	}

	if after := sha256File(t, h.clawdhBin); after == beforeSum {
		t.Error("the installed binary was never replaced")
	}

	// How the handover happened matters, not just that it did. On Unix
	// the process execs into the new binary, keeping its pid — and with
	// it its cgroup, which is what stops a systemd --user unit from
	// killing the successor the moment the old process exits. A changed
	// pid there means it went back to spawn-and-exit.
	if runtime.GOOS != "windows" && statusPID(h.baseURL()) != pidBefore {
		t.Errorf("pid changed from %d to %d: the restart did not exec into the new binary",
			pidBefore, statusPID(h.baseURL()))
	}
	// Windows reports 0666 for every regular file — the execute bit is
	// not a thing there — so this is only a question worth asking where
	// the answer means something.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(h.clawdhBin); err != nil {
			t.Errorf("stat of the updated binary: %v", err)
		} else if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("updated binary is not executable: %v", info.Mode().Perm())
		}
	}

	// Nothing half-downloaded may be left in the install directory.
	entries, err := os.ReadDir(filepath.Dir(h.clawdhBin))
	if err != nil {
		t.Fatalf("reading the install dir: %v", err)
	}
	for _, entry := range entries {
		// The image the update displaced may still be there: it is the running
		// server's own file until that process exits, and the next start deletes
		// it. Each update gives it a name of its own (clawdh.exe.old-<time>), so
		// a displaced image an earlier session still holds cannot block this one.
		name := entry.Name()
		displaced := strings.HasPrefix(name, exeName("clawdh")+".old")
		if name != exeName("clawdh") && !displaced {
			t.Errorf("left %q behind in the install directory", name)
		}
	}
}

// A release older than the running binary must be left alone: that is
// what keeps a build made by hand from being replaced by the published
// one behind its owner's back.
func TestAutoUpdateLeavesANewerLocalBuildAlone(t *testing.T) {
	h := newHarness(t)

	published := buildStamped(t, "v0.0.1-published")
	release := startReleaseServer(t, published, time.Now().Add(-48*time.Hour))
	beforeSum := sha256File(t, h.clawdhBin)

	h.env = setEnv(h.env, "CLAWDH_UPDATE_API", release.URL)
	h.env = setEnv(h.env, "CLAWDH_UPDATE_DELAY", "0s")
	h.env = setEnv(h.env, "CLAWDH_NOTIFY", "0")

	if out, err := h.run("install", "--port", fmt.Sprint(h.port)); err != nil {
		t.Fatalf("clawdh install failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { stopAndRelease(t, h) })
	if !h.waitForHTTP(15 * time.Second) {
		t.Fatalf("service never answered; log:\n%s", readLog(h))
	}

	// Long enough for a check that was going to happen to have
	// happened: the delay is zero and the release server is local.
	time.Sleep(3 * time.Second)

	if after := sha256File(t, h.clawdhBin); after != beforeSum {
		t.Error("an older release was installed over a newer local build")
	}
	if v := statusVersion(h.baseURL()); v != "dev" {
		t.Errorf("version = %q, want the local build's own %q", v, "dev")
	}
}

// stopAndRelease uninstalls, then waits for the log file to actually be
// free.
//
// It is an assertion, not politeness: after an update the server that is
// running is one this test never started — the successor the outgoing
// process handed over to — and the only evidence from outside that
// uninstall really stopped *that* process is its grip on the log file
// letting go. Windows says so plainly by refusing the delete; on Unix
// the remove succeeds first time.
func stopAndRelease(t *testing.T, h *harness) {
	t.Helper()
	if out, err := h.run("uninstall", "--port", fmt.Sprint(h.port)); err != nil {
		t.Logf("uninstall after the test: %v\n%s", err, out)
	}

	logPath := filepath.Join(h.home, ".clawdh", "clawdh.log")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := os.Remove(logPath); err == nil || os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s is still held open 20s after uninstall: a clawdh process outlived it", logPath)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// buildStamped builds clawdh with a version of its own, so the binary
// that gets published is distinguishable from the one that is running.
func buildStamped(t *testing.T, version string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), exeName("clawdh-published"))
	cmd := exec.Command("go", "build",
		"-ldflags", "-X clawdh/internal/buildinfo.Version="+version,
		"-o", out, filepath.Join(repoRoot(t), "cmd", "clawdh"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the published binary: %v\n%s", err, output)
	}
	return out
}

// startReleaseServer serves what GitHub serves: the release JSON, the
// asset, and the checksums.txt the updater verifies it against.
func startReleaseServer(t *testing.T, binaryPath string, publishedAt time.Time) *httptest.Server {
	t.Helper()

	body, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("reading the published binary: %v", err)
	}
	sum := sha256.Sum256(body)
	asset := updater.AssetName()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/download/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":         "latest (main@e2e)",
			"tag_name":     "latest",
			"published_at": publishedAt.UTC().Format(time.RFC3339),
			"assets": []map[string]string{
				{"name": asset, "browser_download_url": srv.URL + "/download/" + asset},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/download/checksums.txt"},
			},
		})
	})
	return srv
}

func statusVersion(baseURL string) string {
	resp, err := http.Get(baseURL + "/api/status")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var status struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return ""
	}
	return status.Version
}

func statusPID(baseURL string) int {
	resp, err := http.Get(baseURL + "/api/status")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var status struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return 0
	}
	return status.PID
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
