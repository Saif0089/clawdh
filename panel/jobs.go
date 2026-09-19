package panel

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Remote jobs: the consented "ask a machine for X" channel. The admin queues a
// job for one enrolled machine; the machine picks it up at its next check-in —
// but only if its owner turned remote help on — runs it, and posts the answer
// back. Every kind is read-only and reports what it did, so nothing here can
// change a machine, only look at it, and only when that machine agreed to be
// looked at.
type Job struct {
	ID          string     `json:"id"`
	DeviceID    string     `json:"deviceId"`
	PersonID    string     `json:"personId,omitempty"`
	Kind        string     `json:"kind"`             // diagnose | sessions | transcript
	Params      string     `json:"params,omitempty"` // e.g. the session id a transcript wants
	Status      string     `json:"status"`           // pending | done | error
	Result      string     `json:"result,omitempty"`
	RequestedBy string     `json:"requestedBy,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ResolvedAt  *time.Time `json:"resolvedAt,omitempty"`
}

// Jobs is the store the remote-jobs channel runs on. It is wired only on the
// server panel (Postgres); a local, file-backed panel leaves it nil and the
// channel is simply absent.
type Jobs interface {
	EnqueueJob(ctx context.Context, j Job) (Job, error)
	PendingJobs(ctx context.Context, deviceID string) ([]Job, error)
	CompleteJob(ctx context.Context, deviceID, jobID, status, result string) error
	RecentJobs(ctx context.Context, deviceID string, limit int) ([]Job, error)
}

// jobKinds are the only things a machine can be asked to do — all read-only,
// and all scoped to what is the panel's business. diagnose: is clawdh healthy
// there and can it reach the gateway. sessions: the sessions that ran through a
// *shared* account (ids, sizes, which account — not content). transcript: one
// such session's transcript. The machine decides scope from its own
// shared-session ledger, so a person's own sessions — their personal login, or a
// local account they manage themselves — are never listed or sent, whatever the
// panel asks for. There is deliberately no general file access.
var jobKinds = map[string]string{
	"diagnose":   "check its own health",
	"sessions":   "list its shared-account sessions",
	"transcript": "send a shared-account session transcript",
}

// handleRequestJob queues a job for a machine. Admin-only. The request is logged
// to the activity feed the moment it is made, so asking a machine for something
// is never invisible to the team.
func (s *Server) handleRequestJob(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	var in struct {
		Kind   string `json:"kind"`
		Params string `json:"params"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	verb, ok := jobKinds[in.Kind]
	if !ok {
		fail(w, http.StatusBadRequest, "That is not something a machine can be asked to do.")
		return
	}

	d, _ := s.store.Load()
	dev, ok := deviceByID(d, deviceID)
	if !ok {
		fail(w, http.StatusNotFound, "There is no such machine.")
		return
	}

	job, err := s.jobs.EnqueueJob(r.Context(), Job{
		DeviceID: deviceID, PersonID: dev.PersonID, Kind: in.Kind,
		Params: strings.TrimSpace(in.Params), RequestedBy: "admin",
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "The request could not be queued: "+err.Error())
		return
	}
	_ = s.store.Mutate(func(d *Data) error {
		d.Log(s.now(), "the panel", "asked "+dev.Name+" to "+verb)
		return nil
	})
	writeJSON(w, http.StatusOK, job)
}

// handleDeviceJobs lists a machine's recent jobs and their answers. Admin-only.
func (s *Server) handleDeviceJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.jobs.RecentJobs(r.Context(), r.PathValue("id"), 20)
	if err != nil {
		fail(w, http.StatusInternalServerError, "Those results could not be read: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleJobResult is where a machine posts the answer to one of its own jobs.
// Device-authenticated (the enrolled machine's token), and scoped to that
// machine, so a machine can only ever resolve jobs addressed to it.
func (s *Server) handleJobResult(w http.ResponseWriter, r *http.Request, dev Device) {
	var in struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	if err := s.jobs.CompleteJob(r.Context(), dev.ID, r.PathValue("id"), in.Status, in.Result); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deviceByID finds an enrolled device by id.
func deviceByID(d Data, id string) (Device, bool) {
	for _, dev := range d.Devices {
		if dev.ID == id {
			return dev, true
		}
	}
	return Device{}, false
}
