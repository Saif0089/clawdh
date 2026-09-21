package panel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// memJobs is an in-memory Jobs store for the round-trip test.
type memJobs struct {
	mu   sync.Mutex
	list []Job
}

func (m *memJobs) EnqueueJob(_ context.Context, j Job) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j.ID = fmt.Sprintf("job%d", len(m.list)+1)
	j.Status, j.CreatedAt = "pending", time.Now()
	m.list = append(m.list, j)
	return j, nil
}

func (m *memJobs) PendingJobs(_ context.Context, deviceID string) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.list {
		if j.DeviceID == deviceID && j.Status == "pending" {
			out = append(out, Job{ID: j.ID, DeviceID: j.DeviceID, Kind: j.Kind, Params: j.Params, Status: "pending"})
		}
	}
	return out, nil
}

func (m *memJobs) CompleteJob(_ context.Context, deviceID, jobID, status, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == jobID && m.list[i].DeviceID == deviceID && m.list[i].Status == "pending" {
			m.list[i].Status, m.list[i].Result = status, result
		}
	}
	return nil
}

func (m *memJobs) RecentJobs(_ context.Context, deviceID string, _ int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for i := len(m.list) - 1; i >= 0; i-- {
		if m.list[i].DeviceID == deviceID {
			out = append(out, m.list[i])
		}
	}
	return out, nil
}

// The whole remote-jobs channel, end to end and consent-gated: the admin asks a
// machine to diagnose itself, the machine only hears about it when it reports
// remote help on, it posts the answer back, and the admin reads it.
func TestRemoteJobRoundTripIsConsentGated(t *testing.T) {
	h := newHarnessWith(t, &memJobs{})

	if code, _ := h.do("POST", "/api/setup", map[string]string{"password": "a-long-enough-one", "name": "Tester"}, ""); code != 200 {
		t.Fatal("setup failed")
	}
	if code, _ := h.do("POST", "/api/people", map[string]string{"name": "Alice"}, ""); code != 201 {
		t.Fatalf("adding a person failed")
	}
	_, pb := h.do("GET", "/api/panel", nil, "")
	personID := pb["people"].([]any)[0].(map[string]any)["id"].(string)

	// Alice enrols a machine.
	_, codeBody := h.do("POST", "/api/people/"+personID+"/invite", nil, "")
	joinCode, _ := codeBody["code"].(string)
	_, enrolled := h.do("POST", "/api/v1/enroll", map[string]string{"code": joinCode, "machine": "alice-mbp"}, "")
	token, _ := enrolled["token"].(string)
	deviceID, _ := enrolled["deviceId"].(string)

	// The admin queues a diagnose for the machine.
	if code, _ := h.do("POST", "/api/devices/"+deviceID+"/jobs", map[string]string{"kind": "diagnose"}, ""); code != 200 {
		t.Fatalf("queuing a job = %d", code)
	}

	// A check-in that does NOT report remote help gets no jobs — consent gates it.
	_, body := h.do("POST", "/api/v1/checkin", map[string]bool{"remote": false}, token)
	if jobs, _ := body["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("a machine with remote help off was handed %d job(s); it must get none", len(jobs))
	}

	// With remote help on, the machine is handed the pending job.
	_, body = h.do("POST", "/api/v1/checkin", map[string]bool{"remote": true}, token)
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("a consenting machine was handed %d job(s), want 1", len(jobs))
	}
	job := jobs[0].(map[string]any)
	if job["kind"] != "diagnose" {
		t.Errorf("job kind = %v, want diagnose", job["kind"])
	}
	jobID, _ := job["id"].(string)

	// The machine posts its answer back.
	if code, _ := h.do("POST", "/api/v1/jobs/"+jobID+"/result",
		map[string]string{"status": "done", "result": "all good here"}, token); code != 200 {
		t.Fatalf("posting a result = %d", code)
	}

	// The admin reads it, and it is no longer pending.
	_, listed := h.do("GET", "/api/devices/"+deviceID+"/jobs", nil, "")
	got, _ := listed["jobs"].([]any)
	if len(got) != 1 {
		t.Fatalf("the admin sees %d job(s), want 1", len(got))
	}
	done := got[0].(map[string]any)
	if done["status"] != "done" || done["result"] != "all good here" {
		t.Errorf("resolved job = %v, want done with the machine's answer", done)
	}

	// And a fresh check-in no longer offers it (it is resolved).
	_, body = h.do("POST", "/api/v1/checkin", map[string]bool{"remote": true}, token)
	if jobs, _ := body["jobs"].([]any); len(jobs) != 0 {
		t.Errorf("a resolved job was handed out again: %v", jobs)
	}
}

// A machine may only resolve jobs addressed to it: posting a result for another
// machine's job does nothing.
func TestRemoteJobResultIsScopedToTheDevice(t *testing.T) {
	store := &memJobs{}
	j, _ := store.EnqueueJob(context.Background(), Job{DeviceID: "device-A", Kind: "diagnose"})
	// device-B tries to resolve device-A's job.
	if err := store.CompleteJob(context.Background(), "device-B", j.ID, "done", "sneaky"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.RecentJobs(context.Background(), "device-A", 10)
	if got[0].Status != "pending" {
		t.Errorf("another device resolved this job (status %q); it must stay pending", got[0].Status)
	}
}
