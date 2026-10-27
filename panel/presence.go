package panel

import (
	"context"
	"time"
)

// Whether a person is around, from the two things the panel actually observes:
// their machines checking in, and the gateway metering their requests. Nothing
// is inferred beyond that.
//
//   - Working: the gateway metered a request of theirs in the last few minutes.
//     A request is metered when its answer finishes, and Claude Code sends one
//     every few seconds while it works, so a long answer is the longest gap.
//   - Online, idle: one of their machines checked in recently, and nothing of
//     theirs went through the gateway.
//   - Offline: neither. A machine checks in every 30 seconds while clawdh runs,
//     so three missed check-ins is a machine that is off, asleep or cut off.
const (
	PresenceWorking = "working"
	PresenceIdle    = "idle"
	PresenceOffline = "offline"

	// workingWithin is how recent a metered request must be to call someone
	// working.
	workingWithin = 3 * time.Minute
	// onlineWithin is how recent a check-in must be to call a machine online:
	// three of the machine's 30-second check-ins.
	onlineWithin = 90 * time.Second
)

// Presence is a person's state and the moment it rests on: their last metered
// request when working, their machines' last check-in otherwise (zero: no
// machine of theirs has ever checked in).
type Presence struct {
	State string    `json:"state"`
	At    time.Time `json:"at,omitzero"`
}

// presenceOf decides a person's state from the newest check-in of any of their
// machines and their newest metered request.
func presenceOf(now, lastSeen, lastRequest time.Time) Presence {
	if !lastRequest.IsZero() && now.Sub(lastRequest) <= workingWithin {
		return Presence{State: PresenceWorking, At: lastRequest}
	}
	if !lastSeen.IsZero() && now.Sub(lastSeen) <= onlineWithin {
		return Presence{State: PresenceIdle, At: lastSeen}
	}
	return Presence{State: PresenceOffline, At: lastSeen}
}

// presence is every current person's state. Several machines: the newest
// check-in of any counts. Without metering (a file-backed panel) nobody can be
// seen working, only online or not.
func (s *Server) presence(ctx context.Context, d Data) map[string]Presence {
	now := s.now()
	seen := map[string]time.Time{}
	for _, dev := range d.Devices {
		if dev.LastSeen.After(seen[dev.PersonID]) {
			seen[dev.PersonID] = dev.LastSeen
		}
	}
	var requested map[string]time.Time
	if s.usage != nil {
		requested, _ = s.usage.LastRequests(ctx, now.Add(-workingWithin)) // a failed read just shows nobody working
	}
	out := make(map[string]Presence, len(d.People))
	for _, p := range d.People {
		out[p.ID] = presenceOf(now, seen[p.ID], requested[p.ID])
	}
	return out
}
