package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/panel"
)

// The client page shows the Claude logins already on this machine, by email, so
// the admin adds the right one to the panel by picking it — never by typing a
// name that has to match. This is the discover-and-add flow that replaces
// `clawdh panel push <name>`.

func (s *Server) handleListLogins(w http.ResponseWriter, r *http.Request) {
	list, _ := s.manager.List()
	writeJSON(w, http.StatusOK, map[string]any{"logins": accounts.DiscoverLogins(list)})
}

// handleAddLoginToPanel captures a discovered login and uploads it to the panel
// as a shareable account. It is an admin action — it asks for the panel password
// — because only the admin adds accounts; members just consume shares.
func (s *Server) handleAddLoginToPanel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConfigDir string `json:"configDir"`
		Email     string `json:"email"`
		Plan      string `json:"plan"`
		Panel     string `json:"panel"`
		Password  string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	in.Panel = strings.TrimRight(strings.TrimSpace(in.Panel), "/")
	if in.Panel == "" || in.Password == "" {
		writeError(w, http.StatusBadRequest, "Enter your panel address and password.")
		return
	}
	name := in.Email
	if name == "" {
		name = "account"
	}

	login, err := accounts.CaptureLogin(in.ConfigDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "That login could not be read on this machine any more.")
		return
	}

	httpc := &http.Client{Jar: &oneHostJar{}, Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	pusher := panel.PusherName(in.Panel)
	if err := panel.AdminLogin(ctx, httpc, in.Panel, in.Password, pusher); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	id, err := panel.CreateAccount(ctx, httpc, in.Panel, name, in.Email, in.Plan)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := panel.PushLogin(ctx, httpc, in.Panel, id, base64.StdEncoding.EncodeToString(login), pusher); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"added": name})
}

// oneHostJar holds the panel session cookie for one add operation.
type oneHostJar struct{ cookies []*http.Cookie }

func (j *oneHostJar) SetCookies(_ *url.URL, c []*http.Cookie) { j.cookies = c }
func (j *oneHostJar) Cookies(_ *url.URL) []*http.Cookie       { return j.cookies }
