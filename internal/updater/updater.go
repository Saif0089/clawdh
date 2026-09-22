// Package updater keeps the installed clawdh current on its own.
//
// clawdh is a background service someone installs once and then forgets,
// which is exactly the shape of software that quietly rots: the fix is
// released, the page keeps showing the old bug, and nobody thinks to
// re-run the installer. So the running server checks the published
// release itself, verifies it against the checksums published beside
// it, replaces its own binary and restarts — the same steps install.sh
// takes, minus the person.
//
// Three rules keep that from being frightening:
//
//   - Nothing is replaced unless its SHA-256 matches the release's own
//     checksums.txt. A truncated or tampered download is discarded.
//   - Nothing is replaced by something older than what is running. The
//     release has to have been published after this binary was written,
//     which also means a build you just made by hand is left alone.
//   - CLAWDH_AUTO_UPDATE=0 turns the whole thing off.
package updater

import (
	"clawdh/internal/config"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DefaultRepo is where releases come from — the same repository
// install.sh downloads from.
const DefaultRepo = "Saif0089/clawdh"

// DefaultAPIBase is GitHub's API root. Overridable so tests can serve a
// release of their own without touching the network.
const DefaultAPIBase = "https://api.github.com"

const (
	// CheckInterval is how often the running service looks.
	//
	// Frequent polling is only affordable because a check that finds
	// nothing new costs nothing: the release is asked for with the ETag
	// of the last answer, and GitHub does not count a 304 against the
	// unauthenticated 60/hour limit. Without that, thirty checks an hour
	// from each machine would rate-limit a household — the limit is per
	// IP, so a few machines behind one router share it.
	CheckInterval = 2 * time.Minute
	// FirstCheckDelay is how long after startup the first check runs. It
	// is short on purpose: a machine that has just booted is the case
	// this whole package exists for, and the old minute meant someone
	// who started their computer, saw the page and went to work was on
	// yesterday's build for the first thing they did.
	FirstCheckDelay = 15 * time.Second
	// rateLimitBackoff is how long to wait after GitHub says no. Asking
	// again two minutes later cannot succeed and only deepens the hole.
	rateLimitBackoff = 20 * time.Minute
	// maxDownloadBytes bounds what a release asset is allowed to be, so
	// a wrong URL cannot fill the disk.
	maxDownloadBytes = 200 << 20
)

// Release is the published build clawdh might move to.
type Release struct {
	// Name is what the release is called: "v0.2.0", or
	// "latest (main@ab12cd3)" for the rolling one. It is what the
	// notification says, because it is the string that tells a person
	// what they just got.
	Name        string
	Tag         string
	PublishedAt time.Time
	Assets      map[string]string // asset name -> download URL
}

// Updater checks for and applies published releases.
type Updater struct {
	Repo       string
	APIBase    string
	HTTPClient *http.Client
	// BinaryPath is the executable to replace: clawdh's own.
	BinaryPath string
	Now        func() time.Time

	// FirstCheck and Interval override the schedule below. Zero means
	// the constants; the end-to-end test sets FirstCheck so it can
	// watch a real update happen without waiting out the delay meant
	// for a machine that has just logged in.
	FirstCheck time.Duration
	Interval   time.Duration

	// mu serialises checks. The background loop is not the only caller
	// any more — someone can press Update now on the page — and two
	// downloads racing to replace the same binary is not something to
	// leave to luck.
	mu sync.Mutex
	// etag and cached are the last answer GitHub gave, so the next ask
	// can be conditional (see CheckInterval).
	etag   string
	cached *Release
	// rateLimitedUntil is when it is worth asking again after a refusal.
	rateLimitedUntil time.Time
}

// ErrRateLimited is a refusal from GitHub rather than a failure: asking
// again shortly cannot succeed, and the caller should say so rather than
// report the update as broken.
var ErrRateLimited = errors.New("GitHub is rate-limiting update checks from this network; it will try again shortly")

// New returns an Updater for the running binary.
func New(binaryPath string) *Updater {
	return &Updater{
		Repo:       DefaultRepo,
		APIBase:    apiBase(),
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
		BinaryPath: binaryPath,
		Now:        time.Now,
		FirstCheck: firstCheckDelay(),
	}
}

// firstCheckDelay honours CLAWDH_UPDATE_DELAY, which exists for the same
// reason CLAWDH_UPDATE_API does: so a test can drive the real update path
// end to end instead of a mock of it.
func firstCheckDelay() time.Duration {
	if raw := config.Env("UPDATE_DELAY"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			// A zero here means "check now", but zero is also how Run
			// spells "no preference, use the default". One millisecond
			// is the same instant to everything except that test.
			if d == 0 {
				return time.Millisecond
			}
			return d
		}
	}
	return FirstCheckDelay
}

// apiBase honours CLAWDH_UPDATE_API so the end-to-end tests can exercise
// the whole path — check, download, verify, swap — against a stub.
func apiBase() string {
	if base := config.Env("UPDATE_API"); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return DefaultAPIBase
}

// Enabled reports whether automatic updates are turned on. Only an
// explicit "0"/"false"/"off" turns them off: an unset variable means the
// person never had an opinion, and the point of this package is that
// they should not need one.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(config.Env("AUTO_UPDATE"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// AssetName is the release asset for the platform this binary runs on,
// named exactly as the release pipeline names it.
func AssetName() string {
	name := "clawdh_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Run checks now and again for the life of ctx, applying whatever it
// finds. onUpdated is called after the new binary is in place, with the
// release that was installed; it is where the caller notifies the
// person and restarts.
//
// Run never returns an error: an update that could not happen is a log
// line, not a reason to take the service down.
func (u *Updater) Run(ctx context.Context, onUpdated func(Release)) {
	if !Enabled() {
		log.Print("automatic updates are off (CLAWDH_AUTO_UPDATE=0)")
		return
	}

	first, interval := u.FirstCheck, u.Interval
	if first <= 0 {
		first = FirstCheckDelay
	}
	if interval <= 0 {
		interval = CheckInterval
	}

	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		release, err := u.CheckAndApply(ctx)
		if err != nil {
			log.Printf("update check: %v", err)
		} else if release != nil {
			log.Printf("updated to %s", release.Name)
			if onUpdated != nil {
				onUpdated(*release)
			}
			// Whatever onUpdated does — normally restart into the new
			// binary — this loop has nothing left to do.
			return
		}
		timer.Reset(interval)
	}
}

// CheckAndApply installs the published release if it is newer than the
// running binary and its checksum matches. It returns the release that
// was installed, or nil when there was nothing to do.
func (u *Updater) CheckAndApply(ctx context.Context) (*Release, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	release, err := u.Latest(ctx)
	if err != nil {
		return nil, err
	}

	// "Newer than what is running" is decided by when this binary was
	// written, not by comparing version strings: the rolling release is
	// always tagged "latest", so its name says nothing about age. This
	// is also what stops an update from overwriting a build made by
	// hand from a working tree that is ahead of the release.
	info, err := os.Stat(u.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("stat of the running binary %s: %w", u.BinaryPath, err)
	}
	if release.PublishedAt.IsZero() {
		// Without a publish time there is no way to tell an update from
		// a downgrade, so nothing is installed — but silence here would
		// look identical to "already current" forever.
		return nil, fmt.Errorf("release %s has no publish time, so it cannot be compared with what is installed", release.Name)
	}
	// A binary dated in the future — a machine whose clock was wrong
	// when clawdh was installed — carries a date that cannot order
	// anything. Left as a gate it would refuse every release from then
	// on, silently and permanently, so in that one case the age check
	// steps aside and the checksum below decides on its own. Stamping
	// the installed time afterwards puts the machine back on a sane
	// footing.
	builtAt := info.ModTime()
	if builtAt.After(u.now()) {
		log.Printf("update check: the installed binary is dated %v, which is in the future; comparing builds instead of dates", builtAt)
	} else if !release.PublishedAt.After(builtAt) {
		return nil, nil
	}

	expected, err := u.expectedChecksum(ctx, release)
	if err != nil {
		return nil, err
	}
	current, err := fileSHA256(u.BinaryPath)
	if err != nil {
		return nil, err
	}
	if current == expected {
		// Same build, published later — nothing to install, and saying
		// so keeps the next check from downloading it again.
		if err := touch(u.BinaryPath, release.PublishedAt); err != nil {
			log.Printf("update check: could not mark the binary as current: %v", err)
		}
		return nil, nil
	}

	if err := u.apply(ctx, release, expected); err != nil {
		return nil, err
	}
	return release, nil
}

// Latest reports the release GitHub marks as latest, which for this
// repository is either a vX.Y.Z tag or the rolling build from main.
//
// The ask carries the ETag of the last answer. Nothing published since
// comes back as a 304 with no body — which GitHub does not charge
// against the rate limit, and which is what makes checking every couple
// of minutes affordable.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	if !u.rateLimitedUntil.IsZero() && u.now().Before(u.rateLimitedUntil) {
		return nil, ErrRateLimited
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase(), u.repo())

	var payload struct {
		Name        string    `json:"name"`
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	resp, err := u.fetch(ctx, url, u.etag)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && u.cached != nil {
		return u.cached, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("reading the published release: %w", err)
	}

	release := &Release{
		Name:        payload.Name,
		Tag:         payload.TagName,
		PublishedAt: payload.PublishedAt,
		Assets:      map[string]string{},
	}
	if release.Name == "" {
		release.Name = release.Tag
	}
	for _, asset := range payload.Assets {
		release.Assets[asset.Name] = asset.URL
	}
	u.etag, u.cached = resp.Header.Get("ETag"), release
	return release, nil
}

// expectedChecksum is the SHA-256 the release publishes for this
// platform's asset. An asset with no published checksum is not
// installed: unverified is the one case worth refusing outright.
func (u *Updater) expectedChecksum(ctx context.Context, release *Release) (string, error) {
	url, ok := release.Assets["checksums.txt"]
	if !ok {
		return "", fmt.Errorf("release %s publishes no checksums.txt, so nothing can be verified", release.Name)
	}
	body, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading checksums.txt: %w", err)
	}

	want := AssetName()
	for _, line := range strings.Split(string(raw), "\n") {
		// sha256sum's format: the digest, two spaces, the file name.
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == want {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("release %s publishes no checksum for %s", release.Name, want)
}

// apply downloads the asset, verifies it, and puts it in place.
func (u *Updater) apply(ctx context.Context, release *Release, expected string) error {
	url, ok := release.Assets[AssetName()]
	if !ok {
		return fmt.Errorf("release %s has no %s to install", release.Name, AssetName())
	}

	// Downloaded next to the binary it will replace: a rename is only
	// atomic within one filesystem, and the system temp directory is
	// routinely on another.
	dir := filepath.Dir(u.BinaryPath)
	// A download killed halfway (sleep, power loss, `clawdh stop`) leaves
	// its temporary file behind, and nothing else would ever remove it.
	sweepStaleDownloads(dir, u.now())

	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating a temporary file next to %s: %w", u.BinaryPath, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has taken it away

	body, err := u.get(ctx, url)
	if err != nil {
		tmp.Close()
		return err
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, digest), io.LimitReader(body, maxDownloadBytes))
	body.Close()
	// Flushed to the disk before the rename: a crash in the seconds
	// after an update would otherwise leave the *name* pointing at a
	// file whose contents never made it, and clawdh would not start at
	// all.
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err != nil {
		return fmt.Errorf("downloading %s: %w", AssetName(), err)
	}
	if written >= maxDownloadBytes {
		return fmt.Errorf("%s is larger than the %d bytes this will download, so it was not installed", AssetName(), maxDownloadBytes)
	}
	if syncErr != nil {
		return fmt.Errorf("flushing the downloaded update to disk: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("writing the downloaded update: %w", closeErr)
	}

	if got := hex.EncodeToString(digest.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s — not installing it", AssetName(), expected, got)
	}
	// Whatever mode the installed binary has is the mode its
	// replacement gets: an install tightened to 0700 on a shared
	// machine must not be widened by an update nobody asked to run.
	mode := os.FileMode(0o755)
	if info, err := os.Stat(u.BinaryPath); err == nil {
		mode = info.Mode().Perm() | 0o100
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("making the update executable: %w", err)
	}
	if err := replaceBinary(tmpName, u.BinaryPath); err != nil {
		return err
	}
	// The mtime decides what "newer" means on the next check, and a
	// fresh download's is now — which would make a release published an
	// hour ago look old. Stamp it with the release's own time instead.
	if err := touch(u.BinaryPath, release.PublishedAt); err != nil {
		log.Printf("update: could not stamp the new binary's time: %v", err)
	}
	return nil
}

func (u *Updater) get(ctx context.Context, url string) (io.ReadCloser, error) {
	resp, err := u.fetch(ctx, url, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("fetching %s: HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// fetch performs one request, optionally conditional on an ETag, and hands
// back the response for the caller to interpret. A rate-limit refusal is
// turned into ErrRateLimited and remembered, so the next few checks do not
// walk into the same wall.
func (u *Updater) fetch(ctx context.Context, url, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub answers unauthenticated API calls, and asks for this.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clawdh-updater")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	client := u.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	// 403 with the remaining count at zero, or 429, is GitHub saying the
	// limit is per IP and this network has spent it. It is not a broken
	// update, and asking again in two minutes cannot help.
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		resp.Body.Close()
		u.rateLimitedUntil = u.now().Add(rateLimitBackoff)
		return nil, ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		resp.Body.Close()
		return nil, fmt.Errorf("fetching %s: HTTP %d", url, resp.StatusCode)
	}
	u.rateLimitedUntil = time.Time{}
	return resp, nil
}

func (u *Updater) apiBase() string {
	if u.APIBase != "" {
		return strings.TrimSuffix(u.APIBase, "/")
	}
	return DefaultAPIBase
}

func (u *Updater) repo() string {
	if u.Repo != "" {
		return u.Repo
	}
	return DefaultRepo
}

// tempPrefix names an in-progress download, so a leftover can be told
// from anything else living beside the binary.
const tempPrefix = ".clawdh-update-"

// staleDownloadAge is how long a temporary file has to have sat there
// before it is assumed to be the wreckage of an interrupted download
// rather than one in flight.
const staleDownloadAge = time.Hour

func sweepStaleDownloads(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < staleDownloadAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func touch(path string, at time.Time) error {
	if at.IsZero() {
		return nil
	}
	return os.Chtimes(path, at, at)
}

// errNoBinaryPath guards the one mistake that would make this package
// destructive: pointing it at nothing.
var errNoBinaryPath = errors.New("no binary path to update")

// Validate reports whether this Updater is safe to run.
func (u *Updater) Validate() error {
	if strings.TrimSpace(u.BinaryPath) == "" {
		return errNoBinaryPath
	}
	return nil
}
