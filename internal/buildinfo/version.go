// Package buildinfo holds clawdh's version, set at build time via
// -ldflags "-X clawdh/internal/buildinfo.Version=...". "dev" is what a
// plain `go build`/`go run` (no ldflags) produces, so it's obvious an
// ad-hoc build is running rather than a tagged release.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

var Version = "dev"

// Commit is the revision this binary was built from, set the same way.
// Releases stamp it; a local `go build` leaves it empty and the commit
// is read from the VCS stamp Go embeds instead.
var Commit = ""

// Tag is the one string the page shows in its corner: which build is
// actually running. A release shows its version, an ad-hoc build shows
// the commit it came from, and a build with uncommitted changes says so
// — the whole point is being able to tell those three apart at a glance
// when a fix "should" already be live.
func Tag() string {
	version, commit := Version, shortCommit()
	switch {
	case version != "dev" && commit != "":
		return version + " · " + commit
	case version != "dev":
		return version
	case commit != "":
		return "dev · " + commit
	default:
		return "dev"
	}
}

// shortCommit is the abbreviated revision, from the ldflag if a release
// set one and otherwise from Go's own VCS stamp.
func shortCommit() string {
	commit, dirty := vcsRevision()
	if Commit != "" {
		commit, dirty = Commit, false
	}
	if commit == "" {
		return ""
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if dirty {
		return commit + "+"
	}
	return commit
}

// vcsRevision reads the revision Go stamps into every binary built
// inside a repository. It is absent from `go build`s of a source tree
// with no VCS (a container copy, a release tarball), which is why the
// ldflag exists as well.
var vcsRevision = sync.OnceValues(func() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
})

// Compact is Tag in one word, for places that separate fields with " · "
// themselves (the status line): "main@7b506ea", "v1.2.0", "dev@7b506ea+", "dev".
func Compact() string {
	version, commit := Version, shortCommit()
	if commit == "" {
		return version
	}
	return version + "@" + commit
}
