package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/panel"
)

// The client page shows the Claude logins already on this machine, by email, so
// a person adds the right one to the panel by picking it — never by typing a
// name that has to match. Adding and taking back happen as the person this
// machine joined the panel as; the panel's password is never asked for here.

func (s *Server) handleListLogins(w http.ResponseWriter, r *http.Request) {
	list, _ := s.manager.List()
	writeJSON(w, http.StatusOK, map[string]any{"logins": accounts.DiscoverLogins(list)})
}

// panelClient is this machine's client for the panel it is joined to, or an
// error that reads as the page should show it when it is joined to none.
func (s *Server) panelClient() (*panel.Client, error) {
	path, err := config.PanelClientFile()
	if err != nil {
		return nil, err
	}
	cfg, err := panel.LoadClientConfig(path)
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, errors.New("This machine isn't connected to a panel yet. Paste your invite link under “Got an invite?” first — then adding takes one click.")
	}
	sharesPath, _ := config.SharesFile()
	return &panel.Client{Config: cfg, Accounts: s.manager, SharesPath: sharesPath, UpdateError: s.updateHealth().Error, AfterChange: func() { _ = s.syncAliases() }}, nil
}

// handleAddLoginToPanel captures a discovered login and hands it up to the
// panel this machine is joined to, as a shareable account added by this
// person. One click: the machine's enrolment is the credential.
func (s *Server) handleAddLoginToPanel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConfigDir string `json:"configDir"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		Plan      string `json:"plan"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	client, err := s.panelClient()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	login, err := accounts.CaptureLogin(in.ConfigDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "That login could not be read on this machine any more.")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.Email
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	added, err := client.Contribute(ctx, name, in.Email, in.Plan, login)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Access to it comes back on a check-in; do one now so the page shows it
	// under "Shared with you" at once, not in half a minute.
	_, _ = client.CheckIn(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"added": added.Name, "refreshed": added.Refreshed, "panel": client.Config.Server})
}

// handleWithdrawFromPanel takes back a login this person handed up: the account
// leaves the panel and everyone sharing it loses access. The panel refuses for
// an account somebody else added, and its reason is shown as it is.
func (s *Server) handleWithdrawFromPanel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.AccountID) == "" {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	client, err := s.panelClient()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := client.Withdraw(ctx, strings.TrimSpace(in.AccountID)); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// The share on it is gone with it; a check-in now makes the page agree.
	_, _ = client.CheckIn(ctx)
	writeJSON(w, http.StatusOK, s.currentPanelStatus())
}
