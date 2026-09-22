package httpserver

import (
	"context"
	"net/http"
	"time"
)

// Updating on request, rather than only when the poll timer comes round.
//
// The service checks for a new build on its own every couple of minutes, which
// is the right default and invisible when it works. It is not much use when it
// has not been working — a machine whose service was not running at all updates
// exactly never, and the only signal was a build tag in the corner that someone
// had to think to compare against a release page. So: a button, and a `clawdh
// update` that presses the same button.

// UpdateOutcome is what one check did, in the words the page and the CLI both
// show. Installed is false when the machine was already current, which is a
// success and has to read like one.
type UpdateOutcome struct {
	Installed bool   `json:"installed"`
	Release   string `json:"release,omitempty"`
	Message   string `json:"message"`
}

// UpdateFunc checks for a published release and installs it if there is one,
// returning what happened. The server does not own the updater — the service
// that starts it does — so this is handed in (see cmd_serve).
type UpdateFunc func(ctx context.Context) (UpdateOutcome, error)

// SetUpdater gives the server a way to update on request. Without one, the
// endpoint says so plainly rather than pretending to have tried.
func (s *Server) SetUpdater(fn UpdateFunc) { s.update = fn }

func (s *Server) handleUpdateNow(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable,
			"Automatic updates are switched off on this machine, so there is nothing to check.")
		return
	}
	// Generous: a check is one small request, but installing means
	// downloading a release over whatever connection this machine has.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	outcome, err := s.update(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}
