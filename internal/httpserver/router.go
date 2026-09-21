package httpserver

import (
	"encoding/json"
	"net/http"

	"clawdh/internal/httpserver/webui"
)

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)

	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.handleRenameAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)

	mux.HandleFunc("GET /api/accounts/{id}/usage", s.handleAccountUsage)
	mux.HandleFunc("POST /api/accounts/{id}/usage/expire", s.handleExpireUsage)

	mux.HandleFunc("POST /api/accounts/{id}/login", s.handleStartLogin)
	mux.HandleFunc("GET /api/accounts/{id}/login/events", s.handleLoginEvents)
	mux.HandleFunc("POST /api/accounts/{id}/login/code", s.handleSubmitLoginCode)
	mux.HandleFunc("POST /api/accounts/{id}/login/cancel", s.handleCancelLogin)

	mux.HandleFunc("POST /api/accounts/{id}/launch-terminal", s.handleLaunchTerminal)

	mux.HandleFunc("GET /api/panel", s.handlePanelStatus)
	mux.HandleFunc("POST /api/panel/connect", s.handlePanelConnect)
	mux.HandleFunc("POST /api/panel/disconnect", s.handlePanelDisconnect)
	mux.HandleFunc("GET /api/remote", s.handleRemote)
	mux.HandleFunc("POST /api/remote/enable", s.handleRemoteEnable)
	mux.HandleFunc("POST /api/remote/requests/{id}/{decision}", s.handleRemoteDecide)
	mux.HandleFunc("POST /api/remote/trust", s.handleRemoteTrust)
	mux.HandleFunc("GET /api/logins", s.handleListLogins)
	mux.HandleFunc("POST /api/logins/add-to-panel", s.handleAddLoginToPanel)
	mux.HandleFunc("POST /api/logins/withdraw", s.handleWithdrawFromPanel)
	mux.HandleFunc("/panel/", s.handlePanelProxy)

	mux.Handle("/", webui.Handler())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
