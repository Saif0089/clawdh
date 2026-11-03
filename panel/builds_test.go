package panel

import (
	"testing"
	"time"
)

// A machine that has stopped updating without saying why is flagged once a newer
// build has been running on two or more machines for an hour — and nothing else
// is: not a machine giving its own reason, not one that is offline, not one on
// the newest build, and not one running its own hand-made build.
func TestAMachineThatStoppedUpdatingIsFlagged(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)

	// Tracking begins with these already running: their ages are unknown.
	s.noteBuild("main · 8959550 — old", t0)
	s.noteBuild("main · e67ae64 — current", t0.Add(30*time.Second))

	// A release comes out after tracking has settled.
	release := t0.Add(2 * time.Hour)
	s.noteBuild("main · 7976917 — new", release)
	// Someone's own build, seen later still, on one machine only.
	s.noteBuild("main · abcdef0 — hand-made", release.Add(10*time.Minute))

	at := func(after time.Duration) time.Time { return release.Add(after) }
	devices := func(now time.Time) Data {
		return Data{Devices: []Device{
			{ID: "new1", Version: "main · 7976917 — new", LastSeen: now},
			{ID: "new2", Version: "main · 7976917 — new", LastSeen: now},
			{ID: "stuck", Version: "main · 8959550 — old", LastSeen: now},
			{ID: "stuck2", Version: "main · e67ae64 — current", LastSeen: now},
			{ID: "says-why", Version: "main · 8959550 — old", LastSeen: now, UpdateError: "needs administrator rights"},
			{ID: "offline", Version: "main · 8959550 — old", LastSeen: now.Add(-3 * time.Hour)},
			{ID: "dev", Version: "main · abcdef0 — hand-made", LastSeen: now},
			{ID: "never-reported", LastSeen: now},
		}}
	}

	// Within the hour: healthy machines are still taking it.
	if got := s.notUpdating(devices(at(30*time.Minute)), at(30*time.Minute)); len(got) != 0 {
		t.Errorf("flagged before the new build had been out an hour: %v", got)
	}

	got := s.notUpdating(devices(at(90*time.Minute)), at(90*time.Minute))
	for _, id := range []string{"stuck", "stuck2"} {
		if !got[id] {
			t.Errorf("%s is on an older build with no reason and was not flagged", id)
		}
	}
	for _, id := range []string{"new1", "new2", "says-why", "offline", "dev", "never-reported"} {
		if got[id] {
			t.Errorf("%s was flagged", id)
		}
	}
}

// One machine on a build is not a release: a hand-made build never becomes the
// one everyone else is measured against.
func TestOneMachineOnABuildIsNotARelease(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	s.noteBuild("main · e67ae64 — current", t0)
	s.noteBuild("main · abcdef0 — hand-made", t0.Add(time.Hour))

	now := t0.Add(5 * time.Hour)
	d := Data{Devices: []Device{
		{ID: "a", Version: "main · e67ae64 — current", LastSeen: now},
		{ID: "b", Version: "main · e67ae64 — current", LastSeen: now},
		{ID: "dev", Version: "main · abcdef0 — hand-made", LastSeen: now},
	}}
	if got := s.notUpdating(d, now); len(got) != 0 {
		t.Errorf("machines were measured against one machine's own build: %v", got)
	}
}

// After a restart every build already running ranks the same, so nothing is
// flagged until a build the panel saw arrive has been out for an hour.
func TestAfterARestartNothingIsFlaggedWrongly(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	// Machines check in over the first minutes, in no particular order.
	s.noteBuild("main · 8959550 — old", t0.Add(40*time.Second))
	s.noteBuild("main · e67ae64 — current", t0)

	now := t0.Add(6 * time.Hour)
	d := Data{Devices: []Device{
		{ID: "a", Version: "main · e67ae64 — current", LastSeen: now},
		{ID: "b", Version: "main · e67ae64 — current", LastSeen: now},
		{ID: "c", Version: "main · 8959550 — old", LastSeen: now},
		{ID: "d", Version: "main · 8959550 — old", LastSeen: now},
	}}
	if got := s.notUpdating(d, now); len(got) != 0 {
		t.Errorf("builds of unknown age were ranked against each other: %v", got)
	}
}
