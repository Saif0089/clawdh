// Package buildinfo holds clawdh's version, set at build time via
// -ldflags "-X clawdh/internal/buildinfo.Version=...". "dev" is what a
// plain `go build`/`go run` (no ldflags) produces, so it's obvious an
// ad-hoc build is running rather than a tagged release.
package buildinfo

import (
	"runtime/debug"
	"strings"
	"sync"
)

var Version = "dev"

// Commit is the revision this binary was built from, set the same way.
// Releases stamp it; a local `go build` leaves it empty and the commit
// is read from the VCS stamp Go embeds instead.
var Commit = ""

// Summary is the subject line of the commit this build was cut from —
// the one sentence that says what the release actually covers. A tag
// and a short sha identify a build; they don't tell anyone what changed
// in it, which is the thing a person wants to know when a notification
// says they were just updated. Releases stamp it; a local build leaves
// it empty and every caller falls back to the bare tag.
var Summary = ""

// summaryLimit keeps a stamped subject to something that still fits a
// status line and a badge. Commit subjects in this repo are sentences,
// so the cut is generous enough to read as one.
const summaryLimit = 72

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

// Describe is Tag plus what the build covers, for the two places with
// room for a sentence: the update notification and the version badges.
// It is Tag() exactly whenever nothing was stamped, so an ad-hoc build
// reads the same as it always did.
func Describe() string {
	summary := Headline()
	if summary == "" {
		return Tag()
	}
	return Tag() + " — " + summary
}

// Headline is the stamped subject on its own, trimmed to a length a
// badge can show, with an ellipsis when it had to be cut so nobody
// reads a truncated sentence as the whole of it.
func Headline() string {
	summary := strings.TrimSpace(Summary)
	if summary == "" {
		return ""
	}
	// Count runes, not bytes: an em dash in a subject must not cause a
	// cut through the middle of a character.
	r := []rune(summary)
	if len(r) <= summaryLimit {
		return summary
	}
	return strings.TrimRight(string(r[:summaryLimit]), " ") + "…"
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
