package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"clawdh/internal/config"
	"clawdh/internal/remotejobs"
	"clawdh/panel"
)

// Remote help, from the owner's side. The panel can ask this machine for a
// health check (answered at once — it reveals nothing personal) or for its
// shared-account sessions and their transcripts, which wait here for the
// owner's decision. This is the API the "Remote help" section of the page
// drives: whether remote help is on, what is waiting, the allow window, and
// what was decided recently. Nothing here reaches beyond this machine.

type remoteView struct {
	Enrolled bool             `json:"enrolled"`
	Enabled  bool             `json:"enabled"`
	Person   string           `json:"person,omitempty"`
	Server   string           `json:"server,omitempty"`
	State    remotejobs.State `json:"state"`
	Options  []trustOption    `json:"options"` // the allow-window choices the page offers
}

type trustOption struct {
	Label   string `json:"label"`
	Minutes int    `json:"minutes"`
}

var trustOptions = []trustOption{{"30 minutes", 30}, {"2 hours", 120}, {"8 hours", 480}}

func (s *Server) loadClientConfig() (panel.ClientConfig, string, error) {
	path, err := config.PanelClientFile()
	if err != nil {
		return panel.ClientConfig{}, "", err
	}
	cfg, err := panel.LoadClientConfig(path)
	return cfg, path, err
}

// handleRemote is the whole picture for the page.
func (s *Server) handleRemote(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := s.loadClientConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, remoteView{
		Enrolled: cfg.Configured(),
		Enabled:  cfg.Configured() && cfg.Remote,
		Person:   cfg.PersonName,
		Server:   cfg.Server,
		State:    remotejobs.Default.Snapshot(),
		Options:  trustOptions,
	})
}

// handleRemoteEnable turns remote help on or off — the same switch as
// `clawdh remote on|off`. Turning it off also ends any allow window.
func (s *Server) handleRemoteEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	cfg, path, err := s.loadClientConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !cfg.Configured() {
		writeError(w, http.StatusBadRequest, "This machine isn't connected to a panel, so there's nothing to turn on.")
		return
	}
	cfg.Remote = in.On
	if err := panel.SaveClientConfig(path, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !in.On {
		remotejobs.Default.Untrust()
	}
	s.handleRemote(w, r)
}

// handleRemoteDecide allows or denies one held request.
func (s *Server) handleRemoteDecide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch strings.ToLower(r.PathValue("decision")) {
	case "allow":
		err = remotejobs.Default.Allow(r.Context(), id)
	case "deny":
		err = remotejobs.Default.Deny(r.Context(), id)
	default:
		writeError(w, http.StatusNotFound, "Allow or deny.")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.handleRemote(w, r)
}

// handleRemoteTrust starts an allow window ("allow requests for the next N
// minutes"), which also runs whatever is waiting; a zero or negative length
// ends the window.
func (s *Server) handleRemoteTrust(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	if in.Minutes <= 0 {
		remotejobs.Default.Untrust()
	} else {
		if in.Minutes > 24*60 {
			in.Minutes = 24 * 60 // a day is the most one click can grant
		}
		remotejobs.Default.Trust(r.Context(), time.Duration(in.Minutes)*time.Minute)
	}
	s.handleRemote(w, r)
}
