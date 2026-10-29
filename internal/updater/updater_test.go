package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// release is the shape GitHub answers /releases/latest with, reduced to
// the fields the updater reads.
type release struct {
	Name        string  `json:"name"`
	TagName     string  `json:"tag_name"`
	PublishedAt string  `json:"published_at"`
	Assets      []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// stubRelease serves a release whose asset is the given bytes, with a
// checksums.txt that either matches it or, when tamper is set, does
// not — the case where a download must be refused.
func stubRelease(t *testing.T, body []byte, publishedAt time.Time, tamper bool) *httptest.Server {
	t.Helper()

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if tamper {
		digest = hex.EncodeToString(make([]byte, 32))
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/download/"+AssetName(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", digest, AssetName())
	})
	mux.HandleFunc("/repos/acme/clawdh/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{
			Name:        "latest (main@abc1234)",
			TagName:     "latest",
			PublishedAt: publishedAt.UTC().Format(time.RFC3339),
			Assets: []asset{
				{Name: AssetName(), URL: srv.URL + "/download/" + AssetName()},
				{Name: "checksums.txt", URL: srv.URL + "/download/checksums.txt"},
			},
		})
	})
	return srv
}

// installedBinary writes a stand-in for the running clawdh, with the
// modification time that decides whether a release counts as newer.
func installedBinary(t *testing.T, content string, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clawdh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing the stand-in binary: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("setting its time: %v", err)
	}
	return path
}

func newUpdater(t *testing.T, srv *httptest.Server, binaryPath string) *Updater {
	t.Helper()
	return &Updater{
		Repo:       "acme/clawdh",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
		BinaryPath: binaryPath,
		Now:        time.Now,
	}
}

func TestAppliesANewerRelease(t *testing.T) {
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, []byte("the new build"), published, false)
	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))

	got, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if got == nil {
		t.Fatal("want the installed release reported, got nil")
	}
	if got.Name != "latest (main@abc1234)" {
		t.Errorf("release name = %q", got.Name)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the updated binary: %v", err)
	}
	if string(content) != "the new build" {
		t.Errorf("binary = %q, want the downloaded build", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no execute bit — Go reports 0666 for every regular
	// file there — so this only means something on Unix.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %v, want it executable", info.Mode().Perm())
	}
	// Stamped with the release's own time, so the next check compares
	// against when this build was published rather than when it landed.
	if !info.ModTime().Truncate(time.Second).Equal(published.UTC().Truncate(time.Second)) {
		t.Errorf("mtime = %v, want the release's %v", info.ModTime().UTC(), published.UTC())
	}
}

// The rule that makes this safe to leave running unattended: a release
// older than the binary is not an update, whatever its checksum says.
// Without it, a build made by hand from a working tree that is ahead of
// the release would be silently replaced by the older published one.
func TestNeverInstallsSomethingOlderThanWhatIsRunning(t *testing.T) {
	published := time.Now().Add(-48 * time.Hour)
	srv := stubRelease(t, []byte("the published build"), published, false)
	path := installedBinary(t, "a build made ten minutes ago", time.Now().Add(-10*time.Minute))

	got, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if got != nil {
		t.Errorf("installed %q over a newer local build", got.Name)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "a build made ten minutes ago" {
		t.Errorf("binary = %q, want it untouched", content)
	}
}

func TestRefusesADownloadThatDoesNotMatchItsChecksum(t *testing.T) {
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, []byte("a download that was tampered with"), published, true)
	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))

	_, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err == nil {
		t.Fatal("want an error, got none")
	}
	content, _ := os.ReadFile(path)
	if string(content) != "the old build" {
		t.Errorf("binary = %q, want it left alone", content)
	}
	// Nothing half-written may be left beside the binary either.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if entry.Name() != "clawdh" {
			t.Errorf("left %q behind next to the binary", entry.Name())
		}
	}
}

// The same build, published again (a re-run of the pipeline), is not an
// update — and must not be downloaded a second time on the next check.
func TestSameBuildPublishedLaterIsNotAnUpdate(t *testing.T) {
	body := []byte("the same build")
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, body, published, false)
	path := installedBinary(t, string(body), published.Add(-24*time.Hour))

	up := newUpdater(t, srv, path)
	got, err := up.CheckAndApply(context.Background())
	if err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if got != nil {
		t.Errorf("reported an update for an identical build: %q", got.Name)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.ModTime().Before(published.Add(-time.Second)) {
		t.Errorf("mtime = %v, want it moved up to the release's %v so the next check is cheap", info.ModTime(), published)
	}
}

func TestRefusesAReleaseWithNoChecksums(t *testing.T) {
	published := time.Now().Add(-time.Hour)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/acme/clawdh/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{
			Name:        "unverifiable",
			TagName:     "latest",
			PublishedAt: published.UTC().Format(time.RFC3339),
			Assets:      []asset{{Name: AssetName(), URL: srv.URL + "/download"}},
		})
	})
	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))

	_, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err == nil {
		t.Fatal("want an error for a release nothing can verify")
	}
	content, _ := os.ReadFile(path)
	if string(content) != "the old build" {
		t.Error("want the binary left alone")
	}
}

func TestEnabledRespectsTheOptOut(t *testing.T) {
	for _, value := range []string{"0", "false", "off", "no", "OFF"} {
		t.Setenv("CLAWDH_AUTO_UPDATE", value)
		if Enabled() {
			t.Errorf("CLAWDH_AUTO_UPDATE=%q left automatic updates on", value)
		}
	}
	for _, value := range []string{"", "1", "true", "yes", "anything else"} {
		t.Setenv("CLAWDH_AUTO_UPDATE", value)
		if !Enabled() {
			t.Errorf("CLAWDH_AUTO_UPDATE=%q turned automatic updates off", value)
		}
	}
}

func TestValidateRefusesAnEmptyBinaryPath(t *testing.T) {
	if err := (&Updater{}).Validate(); err == nil {
		t.Fatal("want an error rather than an updater pointed at nothing")
	}
}

// A release nothing can be compared against must say so rather than
// look identical to "already up to date" forever.
func TestReportsAReleaseWithNoPublishTime(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/acme/clawdh/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{Name: "undated", TagName: "latest"})
	})
	path := installedBinary(t, "the old build", time.Now().Add(-24*time.Hour))

	_, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err == nil {
		t.Fatal("want an error naming the release with no publish time")
	}
}

// A binary dated in the future — an install made while the machine's
// clock was wrong — would otherwise refuse every update from then on.
func TestAClockSkewedBinaryStillUpdates(t *testing.T) {
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, []byte("the new build"), published, false)
	path := installedBinary(t, "the old build", time.Now().Add(72*time.Hour))

	got, err := newUpdater(t, srv, path).CheckAndApply(context.Background())
	if err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if got == nil {
		t.Fatal("want the update installed despite the future date on the binary")
	}
}

// The mode of the installed binary is the mode its replacement keeps: an
// install tightened by hand must not be widened by an update.
func TestKeepsTheInstalledBinarysMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows permissions carry only the read-only attribute, so
		// there is no 0700 to preserve and Chmod cannot set one.
		t.Skip("file modes are a Unix concern")
	}
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, []byte("the new build"), published, false)
	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("tightening the binary: %v", err)
	}

	if _, err := newUpdater(t, srv, path).CheckAndApply(context.Background()); err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %v, want the 0700 it was installed with", got)
	}
}

// Wreckage from an interrupted download is cleaned up by the next one,
// rather than accumulating in the install directory forever.
func TestSweepsAbandonedDownloads(t *testing.T) {
	published := time.Now().Add(-time.Hour)
	srv := stubRelease(t, []byte("the new build"), published, false)
	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))

	stale := filepath.Join(filepath.Dir(path), tempPrefix+"abandoned")
	if err := os.WriteFile(stale, []byte("half a download"), 0o600); err != nil {
		t.Fatalf("planting the leftover: %v", err)
	}
	old := time.Now().Add(-2 * staleDownloadAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("ageing the leftover: %v", err)
	}

	if _, err := newUpdater(t, srv, path).CheckAndApply(context.Background()); err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the abandoned download is still there: %v", err)
	}
}

// The failure that kept machines on old builds: several of them share one
// address, the API's 60 an hour per address is spent, and every check was
// refused. With the API refusing, the release is still found and installed
// through the plain download links.
func TestUpdatesThroughDownloadLinksWhenTheAPIIsRateLimited(t *testing.T) {
	published := time.Now().Add(-time.Hour).Truncate(time.Second)
	body := []byte("the new build")
	sum := sha256.Sum256(body)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/repos/acme/clawdh/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/acme/clawdh/releases/latest/download/"+AssetName(), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/acme/clawdh/releases/latest/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", published.UTC().Format(http.TimeFormat))
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), AssetName())
	})

	path := installedBinary(t, "the old build", published.Add(-24*time.Hour))
	u := newUpdater(t, srv, path)
	u.WebBase = srv.URL

	got, err := u.CheckAndApply(context.Background())
	if err != nil {
		t.Fatalf("CheckAndApply: %v", err)
	}
	if got == nil {
		t.Fatal("want the release installed through the download links, got nil")
	}
	if content, _ := os.ReadFile(path); string(content) != string(body) {
		t.Errorf("binary = %q, want the downloaded build", content)
	}

	// Already current: the next check, still refused by the API, finds nothing to do.
	again, err := u.CheckAndApply(context.Background())
	if err != nil || again != nil {
		t.Errorf("second check = %v, %v; want nothing to do", again, err)
	}
}
