package httpserver

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/editors"
	"clawdh/internal/service"
	"clawdh/internal/sessions"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// Sessions: what is running on this machine right now, and the account new
// work starts on.
//
// This is the half of clawdh that had no page. Running an account was one
// command a card could show; moving a conversation you were already in was a
// phrase you had to have been told — type `clawdh <name>` at the prompt — and
// in an editor, where the command and the chat look nothing like a terminal,
// people simply did not find it. Meanwhile every editor chat was already a
// supervised session that would have accepted exactly that.
//
// So the register is read here and the same switch is staged from a button.
// Nothing new is asked of a session: a staged handoff is what the in-chat
// command has always written, and the supervisor relaunches on it the same way.

type sessionView struct {
	PID       int    `json:"pid"`
	Account   string `json:"account"`
	AccountID string `json:"accountId,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Shared    bool   `json:"shared,omitempty"`
	// Host is "terminal" or "editor" — which of the two ways of running Claude
	// Code this is, since that is the distinction people were missing.
	Host   string `json:"host"`
	Editor string `json:"editor,omitempty"`
	// Where is the folder it was started in, shortened for reading: the thing a
	// person recognises their own window by.
	Where     string    `json:"where,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// defaultView is the account new sessions start as, as the page says it.
type defaultView struct {
	Set       bool   `json:"set"`
	Label     string `json:"label"`
	AccountID string `json:"accountId,omitempty"`
	Shared    string `json:"shared,omitempty"`
	// Problem is set when the recorded default names something that has since
	// gone — a deleted account, a share taken back, a login handed to the
	// gateway. New sessions fall back to the machine's own login, and saying so
	// here is the difference between "it is set" and "it is working".
	Problem string `json:"problem,omitempty"`
}

type editorView struct {
	Name         string `json:"name"`
	HasExtension bool   `json:"hasExtension"`
	Managed      bool   `json:"managed"`
}

type sessionsResponse struct {
	Sessions []sessionView `json:"sessions"`
	Default  defaultView   `json:"default"`
	Editors  []editorView  `json:"editors"`
	// Supported is false where the register cannot be read at all; the page then
	// says so rather than showing an empty list that looks like "nothing running".
	Supported bool `json:"supported"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sessionsSnapshot())
}

func (s *Server) sessionsSnapshot() sessionsResponse {
	out := sessionsResponse{Sessions: []sessionView{}, Editors: []editorView{}}
	dir, err := config.SessionsDir()
	if err != nil {
		return out
	}
	out.Supported = true
	for _, live := range sessions.List(dir) {
		out.Sessions = append(out.Sessions, sessionView{
			PID:       live.PID,
			Account:   live.Account,
			AccountID: live.AccountID,
			Slug:      live.Slug,
			Shared:    live.Shared,
			Host:      string(live.Host),
			Editor:    live.Editor,
			Where:     shortenDir(live.Dir),
			StartedAt: live.StartedAt,
		})
	}
	out.Default = s.defaultView()
	out.Editors = s.editorViews()
	return out
}

// defaultView resolves the recorded default against what exists now, so the
// page shows the account's live name and notices when it has gone.
func (s *Server) defaultView() defaultView {
	path, err := config.NewSessionDefaultFile()
	if err != nil {
		return defaultView{}
	}
	rec := sessions.ReadDefault(path)
	v := defaultView{Set: rec.Set(), AccountID: rec.AccountID, Shared: rec.Shared, Label: rec.Name}
	switch {
	case rec.Shared != "":
		for _, sh := range s.currentPanelStatus().Shared {
			if strings.EqualFold(sh.Slug, rec.Shared) {
				v.Label = sh.Account
				return v
			}
		}
		v.Problem = "That shared account isn't shared with this machine any more, so new sessions use your own login."
	case rec.AccountID != "":
		list, _ := s.manager.List()
		for _, a := range list {
			if a.ID != rec.AccountID {
				continue
			}
			v.Label = a.Name
			if _, err := accounts.CaptureLogin(a.ConfigDir); err != nil {
				v.Problem = a.Name + " has no login on this machine, so new sessions use your own login until you reconnect it."
			}
			return v
		}
		v.Problem = "That account no longer exists, so new sessions use your own login."
	}
	return v
}

// editorViews is every VS Code-family editor on this machine and whether clawdh
// is in its launch path. The service wires them at startup; this is how a
// person sees that it happened rather than taking it on trust.
func (s *Server) editorViews() []editorView {
	out := []editorView{}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	self, _ := service.SelfPath()
	for _, ed := range editors.Installed(home) {
		out = append(out, editorView{
			Name:         ed.Name,
			HasExtension: ed.HasExtension,
			Managed:      ed.HasExtension && self != "" && editors.WrapperPath(ed.Settings) == self,
		})
	}
	return out
}

// switchTarget is an account a session can be moved to, resolved once and used
// for every session in the request.
type switchTarget struct {
	name   string // what the handoff names: an account's slug, or a share's
	label  string // what to call it when reporting
	shared bool
	id     string // the local account's id, for "it is already on this one"
}

// resolveTarget turns what the page asked for into a target, or the reason it
// cannot be one — worded for someone reading a web page, not a terminal.
func (s *Server) resolveTarget(accountID, shared string) (switchTarget, string) {
	if shared = strings.TrimSpace(shared); shared != "" {
		sharesPath, err := config.SharesFile()
		if err != nil {
			return switchTarget{}, "This machine cannot read its shared accounts."
		}
		list, _ := panel.LoadShares(sharesPath)
		for _, sh := range list {
			if strings.EqualFold(sh.Slug, shared) {
				return switchTarget{name: sh.Slug, label: sh.Account, shared: true}, ""
			}
		}
		return switchTarget{}, "That account isn't shared with this machine any more."
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return switchTarget{}, "Pick an account first."
	}
	acct, err := s.manager.Get(accountID)
	if err != nil {
		return switchTarget{}, "That account is not on this machine any more."
	}
	if _, err := accounts.CaptureLogin(acct.ConfigDir); err != nil {
		return switchTarget{}, acct.Name + " isn't signed in on this machine. Connect it first, then sessions can run as it."
	}
	return switchTarget{name: acct.Slug, label: acct.Name, id: acct.ID}, ""
}

// onTarget reports whether a session is already running as this target, so
// moving everything doesn't restart the sessions that are already there.
func onTarget(live sessions.Session, t switchTarget) bool {
	if t.shared {
		return live.Shared && strings.EqualFold(live.Slug, t.name)
	}
	return !live.Shared && live.AccountID == t.id
}

// handleSetSessionDefault records the account new sessions start as — an
// editor's chats and a plain `claude` alike — and makes sure every editor with
// the extension actually launches through clawdh, so the setting has something
// to take effect in.
func (s *Server) handleSetSessionDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID string `json:"accountId"`
		Shared    string `json:"shared"`
		Clear     bool   `json:"clear"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	path, err := config.NewSessionDefaultFile()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Clear {
		if err := sessions.ClearDefault(path); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.sessionsSnapshot())
		return
	}
	target, problem := s.resolveTarget(in.AccountID, in.Shared)
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}
	rec := sessions.Default{Name: target.label}
	if target.shared {
		rec.Shared = target.name
	} else {
		rec.AccountID = target.id
		if acct, err := s.manager.Get(target.id); err == nil {
			rec.ConfigDir = acct.ConfigDir
		}
	}
	if err := sessions.WriteDefault(path, rec); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// An editor that isn't launching through clawdh would ignore the setting
	// entirely. The service wires them at startup; doing it again here means
	// an editor installed since then is picked up the moment someone asks for
	// this, instead of at the next restart.
	if home, err := os.UserHomeDir(); err == nil {
		if err := configureEditors(home); err != nil {
			log.Printf("could not point every editor at clawdh: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, s.sessionsSnapshot())
}

// handleSwitchSessions moves running sessions to another account: one of them,
// or every one at once.
//
// It stages exactly what typing `clawdh <name>` into that session would have —
// the supervisor picks it up within a moment and relaunches the session there,
// conversation and all. Anything running inside it at that moment does not
// survive, which is true of the typed command too and is said on the page.
func (s *Server) handleSwitchSessions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PID       int    `json:"pid"`
		All       bool   `json:"all"`
		AccountID string `json:"accountId"`
		Shared    string `json:"shared"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	target, problem := s.resolveTarget(in.AccountID, in.Shared)
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}
	dir, err := config.SessionsDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var want []sessions.Session
	live := sessions.List(dir)
	if in.All {
		want = live
	} else {
		found := false
		for _, l := range live {
			if l.PID == in.PID {
				want, found = []sessions.Session{l}, true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "That session has already ended.")
			return
		}
	}

	moved, already, failed := 0, 0, 0
	for _, l := range want {
		if onTarget(l, target) {
			already++
			continue
		}
		h := switching.Handoff{
			Account:   target.name,
			SessionID: l.SessionID,
			Shared:    target.shared,
			For:       l.Nonce,
		}
		if err := switching.WriteHandoff(l.Handoff, h); err != nil {
			log.Printf("could not stage a switch for session %d: %v", l.PID, err)
			failed++
			continue
		}
		moved++
	}
	if moved == 0 && failed > 0 {
		writeError(w, http.StatusInternalServerError, "Those sessions could not be moved.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"moved": moved, "already": already, "failed": failed, "label": target.label,
	})
}

// shortenDir is a working directory as a person reads it: the home directory
// as ~, and only the last couple of parts of a long path, since the end of it
// is what names the project.
func shortenDir(dir string) string {
	if dir == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
			dir = "~/" + filepath.ToSlash(rel)
		}
	}
	parts := strings.Split(filepath.ToSlash(dir), "/")
	if len(parts) > 3 {
		return ".../" + strings.Join(parts[len(parts)-2:], "/")
	}
	return dir
}

// handleSetUpEditors puts clawdh into every editor's launch path now, rather
// than at the next service start.
//
// The service does this on its way up, so it is normally already true. It can
// fail to be: an editor installed since then, an editor whose settings.json
// could not be written, a machine where the opt-out was set and has since been
// removed. Without it, the account chosen for new sessions has nothing to take
// effect in — the chat launches Claude Code directly and runs on the machine's
// own login — which looked exactly like the setting being ignored.
func (s *Server) handleSetUpEditors(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := configureEditors(home); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not update an editor's settings: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.sessionsSnapshot())
}
