package remotejobs

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type fakeHooks struct {
	ran       []string
	reports   []string // "id:status"
	runResult string
}

func (f *fakeHooks) wire(q *Queue) {
	q.Run = func(kind, params string) (string, string) {
		f.ran = append(f.ran, kind+":"+params)
		return f.runResult, "done"
	}
	q.Report = func(_ context.Context, id, status, _ string) error {
		f.reports = append(f.reports, id+":"+status)
		return nil
	}
}

func req(id, kind string) Request {
	return Request{ID: id, Kind: kind, Params: "p", RequestedBy: "Hassan", Describe: "do " + kind}
}

// A request is held until the owner decides; the panel is told it is awaiting.
// Allow runs it and reports the answer; Deny reports a decline. Both leave a
// history entry, and neither is repeatable.
func TestHoldAllowDeny(t *testing.T) {
	q := New("")
	f := &fakeHooks{runResult: "the answer"}
	f.wire(q)
	ctx := context.Background()

	if held := q.Receive(ctx, req("j1", "transcript")); !held {
		t.Fatal("a request outside an allow window should be held")
	}
	q.Receive(ctx, req("j2", "sessions"))
	if got := f.reports; len(got) != 2 || got[0] != "j1:awaiting" || got[1] != "j2:awaiting" {
		t.Fatalf("held requests should be reported awaiting, got %v", got)
	}
	if len(f.ran) != 0 {
		t.Fatal("nothing should run before a decision")
	}
	if st := q.Snapshot(); len(st.Pending) != 2 || st.TrustUntil != nil {
		t.Fatalf("snapshot = %+v, want 2 pending and no allow window", st)
	}

	if err := q.Allow(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	if err := q.Deny(ctx, "j2"); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 1 || f.ran[0] != "transcript:p" {
		t.Errorf("allow should run exactly the allowed request, ran %v", f.ran)
	}
	if last := f.reports[len(f.reports)-2:]; last[0] != "j1:done" || last[1] != "j2:denied" {
		t.Errorf("reports = %v, want j1 done then j2 denied", f.reports)
	}
	if err := q.Allow(ctx, "j1"); err == nil {
		t.Error("a settled request must not be allowable again")
	}
	st := q.Snapshot()
	if len(st.Pending) != 0 || len(st.History) != 2 || st.History[0].Outcome != "denied" || st.History[1].Outcome != "allowed" {
		t.Errorf("history = %+v", st.History)
	}
}

// Inside an allow window requests run on arrival (outcome "auto"); starting a
// window also runs everything already held; ending it makes requests ask again.
func TestAllowWindow(t *testing.T) {
	q := New("")
	f := &fakeHooks{}
	f.wire(q)
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	q.Now = func() time.Time { return now }

	q.Receive(ctx, req("held", "sessions"))
	q.Trust(ctx, 30*time.Minute)
	if len(f.ran) != 1 || f.ran[0] != "sessions:p" {
		t.Fatalf("starting a window should run what was held, ran %v", f.ran)
	}
	if st := q.Snapshot(); st.TrustUntil == nil || len(st.Pending) != 0 {
		t.Fatalf("snapshot = %+v, want an allow window and nothing pending", st)
	}
	if held := q.Receive(ctx, req("auto1", "transcript")); held {
		t.Error("a request inside the window must not be held")
	}
	if st := q.Snapshot(); st.History[0].Outcome != "auto" {
		t.Errorf("a request run inside the window records outcome auto, got %q", st.History[0].Outcome)
	}

	now = now.Add(31 * time.Minute) // the window lapsed
	if held := q.Receive(ctx, req("later", "transcript")); !held {
		t.Error("after the window lapses requests ask again")
	}
	q.Untrust()
	if q.Trusted() {
		t.Error("Untrust must end the window")
	}
}

// An unanswered request is declined on the owner's behalf after MaxPendingAge,
// and the queue survives a restart via its state file.
func TestExpiryAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-requests.json")
	q := New(path)
	f := &fakeHooks{}
	f.wire(q)
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	q.Now = func() time.Time { return now }

	q.Receive(ctx, req("old", "transcript"))
	now = now.Add(20 * time.Minute)
	q.Receive(ctx, req("fresh", "sessions"))

	// Restart: a new queue over the same file still holds both.
	q2 := New(path)
	f2 := &fakeHooks{}
	f2.wire(q2)
	q2.Now = func() time.Time { return now.Add(45 * time.Minute) } // old is >1h, fresh is 45m
	if st := q2.Snapshot(); len(st.Pending) != 2 {
		t.Fatalf("after restart pending = %d, want 2", len(st.Pending))
	}
	q2.Expire(ctx)
	st := q2.Snapshot()
	if len(st.Pending) != 1 || st.Pending[0].ID != "fresh" {
		t.Errorf("expire should drop only the hour-old request, pending = %+v", st.Pending)
	}
	if len(f2.reports) != 1 || f2.reports[0] != "old:denied" {
		t.Errorf("an expired request is reported denied, got %v", f2.reports)
	}
	if st.History[0].Outcome != "expired" {
		t.Errorf("history outcome = %q, want expired", st.History[0].Outcome)
	}
}
