package panel

import (
	"testing"
	"time"
)

// The state rests on what was observed, and on nothing else: a recent metered
// request is working whatever the machines say, a recent check-in without one is
// idle, and neither is offline — still saying when they were last seen.
func TestPresenceIsReadFromCheckInsAndMeteredRequests(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name          string
		seen, request time.Time
		want          Presence
	}{
		{"never connected", time.Time{}, time.Time{}, Presence{State: PresenceOffline}},
		{"checked in a minute ago", now.Add(-time.Minute), time.Time{}, Presence{State: PresenceIdle, At: now.Add(-time.Minute)}},
		{"three check-ins missed", now.Add(-91 * time.Second), time.Time{}, Presence{State: PresenceOffline, At: now.Add(-91 * time.Second)}},
		{"a request 40s ago", now.Add(-10 * time.Second), now.Add(-40 * time.Second), Presence{State: PresenceWorking, At: now.Add(-40 * time.Second)}},
		{"a request, machine silent", time.Time{}, now.Add(-2 * time.Minute), Presence{State: PresenceWorking, At: now.Add(-2 * time.Minute)}},
		{"a request too long ago", now.Add(-30 * time.Second), now.Add(-4 * time.Minute), Presence{State: PresenceIdle, At: now.Add(-30 * time.Second)}},
	} {
		if got := presenceOf(now, c.seen, c.request); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The People tab and the usage board carry each person's presence; a person
// with two machines is online if either is, and the machine's own check-in is
// what keeps them there.
func TestThePanelShowsWhoIsWorkingOnlineOrOffline(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, _ := h.join("Alice", "alice-mbp")
	bobID, _ := h.join("Bob", "bob-pc")
	carolID, _ := h.join("Carol", "carol-laptop")
	start := h.clock

	h.clock = start.Add(10 * time.Minute)
	// Carol's second machine checked in 30s ago; her first has been silent since
	// she enrolled it.
	if err := h.store.Mutate(func(d *Data) error {
		d.Devices = append(d.Devices, Device{ID: "carol-desk", PersonID: carolID, Name: "carol-desk", LastSeen: h.clock.Add(-30 * time.Second)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage.lastRequests = map[string]time.Time{bobID: h.clock.Add(-40 * time.Second)}

	_, body := h.do("GET", "/api/panel", nil, "")
	got := map[string]map[string]any{}
	for _, p := range body["people"].([]any) {
		m := p.(map[string]any)
		got[m["id"].(string)] = m["presence"].(map[string]any)
	}
	if got[bobID]["state"] != PresenceWorking || got[bobID]["at"] != h.clock.Add(-40*time.Second).Format(time.RFC3339) {
		t.Errorf("Bob = %v, want working since his request 40s ago", got[bobID])
	}
	if got[aliceID]["state"] != PresenceOffline || got[aliceID]["at"] != start.Format(time.RFC3339) {
		t.Errorf("Alice = %v, want offline, last seen when she enrolled", got[aliceID])
	}
	if got[carolID]["state"] != PresenceIdle {
		t.Errorf("Carol = %v, want online — her second machine checked in 30s ago", got[carolID])
	}

}

// Wherever a person is listed with their usage — an account's card on the board,
// the people board, and the shared-account cards a machine is sent — their
// presence is beside them. A machine is told only the state and its time.
func TestPresenceRidesWithEveryListOfPeople(t *testing.T) {
	usage := &fakeUsage{}
	h := newHarnessUsage(t, usage)
	h.setUp()
	aliceID, aliceTok := h.join("Alice", "alice-mbp")
	bobID, bobTok := h.join("Bob", "bob-pc")
	acct := h.contribute(bobTok, "Team", "team@example.com")
	if code, body := h.do("POST", "/api/accounts/"+acct+"/share", map[string]string{"personId": aliceID}, ""); code != 200 {
		t.Fatalf("sharing = %d %v", code, body)
	}
	usage.windows = []AccountWindow{{AccountID: acct, SevenD: 0.80, SevenDReset: h.clock.Add(50 * time.Hour), UpdatedAt: h.clock}}
	usage.ranBy = map[string][]SubjectUsage{acct: {{SubjectID: bobID, Weighted: 100e6}}}
	usage.people = []SubjectUsage{{SubjectID: bobID, Weighted: 100e6}}
	usage.lastRequests = map[string]time.Time{bobID: h.clock.Add(-20 * time.Second)}

	card := boardAccount(t, h, acct)
	bob := card["people"].([]any)[0].(map[string]any)
	if pr, _ := bob["presence"].(map[string]any); pr["state"] != PresenceWorking {
		t.Errorf("Bob on the card = %v, want working", bob["presence"])
	}

	_, board := h.do("GET", "/api/usage/people?window=day", nil, "")
	if subs, _ := board["subjects"].([]any); len(subs) != 1 || subs[0].(map[string]any)["presence"].(map[string]any)["state"] != PresenceWorking {
		t.Errorf("people board = %v, want Bob working", board["subjects"])
	}

	_, body := h.do("POST", "/api/v1/checkin", nil, aliceTok)
	w := body["windows"].([]any)[0].(map[string]any)
	pr := w["people"].([]any)[0].(map[string]any)["presence"].(map[string]any)
	if pr["state"] != PresenceWorking || len(pr) != 2 {
		t.Errorf("Bob as Alice's machine sees him = %v, want working and its time, nothing more", pr)
	}
}
