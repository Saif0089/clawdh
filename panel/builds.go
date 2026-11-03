package panel

import (
	"strings"
	"time"
)

// Which machines have stopped updating without saying why.
//
// A machine reports the build it runs on every check-in, and why it cannot
// update when it knows. But a machine can stop updating without knowing: Windows
// PCs started at login by the old `start /min clawdh serve` switched automatic
// updates off and told nobody, and sat on builds weeks old that looked exactly
// like builds nobody had superseded. This is the net under every such cause,
// known or not: the panel notes when it first saw each build, and once a newer
// build has been running on two or more machines for an hour, a live machine
// still on an older one, with no reason given, is flagged on the panel. Healthy
// machines take a new build within minutes.
//
// It is kept in the panel process's memory, not in the panel's state: the
// gateway writes that state too, and a gateway build that predates a field
// drops it on every write. After a restart the builds already running are all
// "seen before tracking began" and rank equal, so nothing is ever flagged
// wrongly; detection resumes with the next build.

const (
	// buildsSettle is how long after tracking begins a build still counts as
	// already running before it: every live machine checks in within a minute,
	// so ten is ample.
	buildsSettle = 10 * time.Minute
	// behindAfter is how long a newer build must have been running before a
	// machine not on it counts as not updating.
	behindAfter = time.Hour
	// liveWithin is how recently a machine must have checked in to count.
	liveWithin = 15 * time.Minute
	// maxBuilds bounds the memory: a few releases a day, kept for weeks.
	maxBuilds = 200
)

// noteBuild records the first time the panel saw a build.
func (s *Server) noteBuild(version string, now time.Time) {
	version = strings.TrimSpace(version)
	if version == "" {
		return
	}
	s.builds.Lock()
	defer s.builds.Unlock()
	if s.builds.since.IsZero() {
		s.builds.since = now
	}
	if _, ok := s.builds.first[version]; ok {
		return
	}
	first := now
	if now.Sub(s.builds.since) < buildsSettle {
		first = time.Time{} // already running when tracking began: its age is unknown
	}
	if len(s.builds.first) >= maxBuilds {
		var oldest string
		for v, at := range s.builds.first {
			if oldest == "" || at.Before(s.builds.first[oldest]) {
				oldest = v
			}
		}
		delete(s.builds.first, oldest)
	}
	s.builds.first[version] = first
}

// notUpdating is the set of live machines, by device ID, that are on an older
// build than one two or more machines have been running for over an hour, and
// that report no reason of their own (a machine that does is already shown as
// unable to update, with the reason).
func (s *Server) notUpdating(d Data, now time.Time) map[string]bool {
	s.builds.Lock()
	first := make(map[string]time.Time, len(s.builds.first))
	for v, at := range s.builds.first {
		first[v] = at
	}
	s.builds.Unlock()

	running := map[string]int{}
	for _, dev := range d.Devices {
		if dev.Version != "" && now.Sub(dev.LastSeen) <= liveWithin {
			running[dev.Version]++
		}
	}
	// The newest build that is really out: two machines on it, so one machine's
	// own hand-made build never counts.
	var newest string
	var newestAt time.Time
	for v, n := range running {
		if at := first[v]; n >= 2 && at.After(newestAt) {
			newest, newestAt = v, at
		}
	}
	if newest == "" || now.Sub(newestAt) < behindAfter {
		return nil
	}
	out := map[string]bool{}
	for _, dev := range d.Devices {
		if dev.Version == "" || dev.Version == newest || dev.UpdateError != "" || now.Sub(dev.LastSeen) > liveWithin {
			continue
		}
		at, known := first[dev.Version]
		if !known || at.Before(newestAt) {
			out[dev.ID] = true
		}
	}
	return out
}
