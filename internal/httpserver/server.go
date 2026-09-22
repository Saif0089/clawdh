// Package httpserver serves clawdh's REST/SSE API and embedded web UI on
// 127.0.0.1 only — this tool manages login credentials, so it must never
// be reachable from anything but the same machine.
package httpserver

import (
	"clawdh/internal/editors"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/service"
	"clawdh/internal/shellrc"
	"clawdh/internal/termlauncher"
	"clawdh/internal/usage"
)

// Server holds every dependency the HTTP handlers need.
type Server struct {
	manager      *accounts.Manager
	syncer       *shellrc.Syncer
	claudeBinary string

	// usage reports plan limits and login lifetime per account.
	usage *usage.Service

	// launchTerminal opens a terminal scoped to an account; a field
	// (rather than calling termlauncher.Launch directly) so tests can
	// stub it out without actually opening a window.
	launchTerminal func(configDir, label string) error

	// defaultCheck throttles re-detection of the default account, which
	// costs a `claude auth status` subprocess.
	defaultCheckMu   sync.Mutex
	defaultCheckedAt time.Time

	// restart is closed when something — in practice the updater,
	// after replacing this binary — asks the server to hand over to a
	// fresh process. Closing a channel rather than calling a function
	// keeps the actual restart in Serve, which is the only place that
	// knows the port it bound and can close the listener first.
	restart     chan struct{}
	restartOnce sync.Once

	// update checks for and installs a published release on request, so
	// the page and `clawdh update` can do what the poll timer does. Nil
	// when automatic updates are off (see SetUpdater).
	update UpdateFunc

	mu     sync.Mutex
	logins map[string]*loginBroadcast // accountID -> in-progress/last login, if any
	// startMu serialises login starts, which span a subprocess spawn
	// and so can't be done under mu.
	startMu sync.Mutex
}

// New builds a Server. claudeBinary is the executable to spawn for
// logins and probes (normally "claude", overridable for tests).
func New(manager *accounts.Manager, syncer *shellrc.Syncer, claudeBinary string) *Server {
	svc := usage.NewService()
	svc.Gateway = gatewayUsageFor // a login the gateway holds shows the gateway's reading
	return &Server{
		manager:        manager,
		syncer:         syncer,
		claudeBinary:   claudeBinary,
		usage:          svc,
		launchTerminal: termlauncher.Launch,
		logins:         map[string]*loginBroadcast{},
		restart:        make(chan struct{}),
	}
}

// RequestRestart asks the server to shut down and start its replacement
// from the binary now on disk. Safe to call more than once; only the
// first call is acted on.
func (s *Server) RequestRestart() {
	s.restartOnce.Do(func() { close(s.restart) })
}

// Handler returns the complete http.Handler: the embedded web UI plus
// the JSON/SSE API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return withLocalOnly(withLogging(mux))
}

func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// Serve runs the HTTP server on 127.0.0.1:port (0 to pick any free
// port), writes the chosen port to clawdh's port file so other clawdh
// invocations and the installer's health check can find it, records
// this process's PID, and blocks until ctx is canceled — at which point
// it shuts down gracefully and cleans up both files.
func Serve(ctx context.Context, srv *Server, port int) error {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("listening on 127.0.0.1:%d: %w", port, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	if err := writePortFile(actualPort); err != nil {
		ln.Close()
		return err
	}
	// Cleared when this process is handing the port to a successor:
	// the replacement writes the same port file on its way up, and
	// removing it on the way out would strand it — nothing could find
	// the server that is actually serving.
	handingOver := false
	defer func() {
		if !handingOver {
			removePortFile(actualPort)
		}
	}()

	if err := service.RecordSelf(); err != nil {
		log.Printf("warning: could not record pid file: %v", err)
	}

	// Adopt the account plain `claude` uses, and keep watching for it.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	srv.StartDefaultAccountWatch(watchCtx)

	// De-isolation migration, in the background: move any still-isolated
	// managed account's transcripts into the shared ~/.claude and flip it to
	// credentials-only. Deliberately AFTER the port file is written and off
	// the startup path — a first-boot copy can be hundreds of MB, and running
	// it before the listener would delay the port file past the updater's
	// restart handshake. The copy itself holds no account lock; only the quick
	// isolation flip does. Best-effort: a failure is logged and retried next
	// start, and never re-logs-in an account (the login is found by the same
	// keychain hash under either scheme).
	go func() {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Printf("de-isolation: cannot resolve home: %v", err)
			return
		}
		report, err := srv.manager.MigrateManagedToShared(filepath.Join(home, ".claude"))
		if err != nil {
			log.Printf("warning: de-isolation migration could not run: %v", err)
			return
		}
		for _, a := range report.Accounts {
			switch {
			case a.Err != nil:
				log.Printf("de-isolation: account %q not migrated, will retry: %v", a.ID, a.Err)
			case a.Failed > 0:
				log.Printf("de-isolation: account %q kept on old scheme, %d files could not be copied, will retry", a.ID, a.Failed)
			case a.Flipped:
				log.Printf("de-isolation: migrated %q — %d transcripts copied, %d already shared", a.ID, a.Copied, a.Skipped)
			}
		}

		// Re-render the managed rc block on every start. This is how a new
		// block body — an account that changed how it is scoped, or a new
		// feature like the `claude` wrapper that makes a plain session
		// switchable — reaches an installation that only ever auto-updates.
		// It is not the churn it looks like: UpsertBlock compares the rendered
		// body with what is in the file and returns without writing when they
		// match, so the steady state is one read per rc file per boot and no
		// write at all.
		if err := srv.syncAliases(); err != nil {
			log.Printf("shell aliases: could not refresh: %v", err)
		}

		// Same idea for editors. The Claude Code extension starts Claude
		// itself and never sees a shell, so the only way clawdh reaches it is
		// this setting — and the only way a user gets in-conversation
		// switching without being told to run a command is for clawdh to set it
		// on their behalf, the way it already writes shell aliases. Editors
		// without the extension are left alone, and the write is skipped when
		// the setting already names this binary.
		if err := configureEditors(home); err != nil {
			log.Printf("editors: could not configure: %v", err)
		}
	}()

	httpSrv := &http.Server{Handler: srv.Handler()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	log.Printf("clawdh listening on http://127.0.0.1:%d", actualPort)

	select {
	case <-ctx.Done():
		// End any in-flight logins first: their `claude` children are
		// in their own session (setsid), so shutting down without this
		// would leave them running with nothing to stop them.
		srv.stopAllLogins()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case <-srv.restart:
		// An update replaced this binary on disk. Shut down first so
		// the port is free, then start the file that is there now: no
		// service manager supervises clawdh (see internal/service), so
		// nothing else would ever bring it back.
		srv.stopAllLogins()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)

		// Someone asked clawdh to stop while it was handing over —
		// `clawdh stop`, `clawdh uninstall`, or a logout. Starting a
		// successor now would leave a server running that the thing
		// which just stopped us no longer knows how to stop.
		if ctx.Err() != nil {
			log.Print("update installed, but clawdh was asked to stop before it could restart")
			return nil
		}

		path, err := service.SelfPath()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRestartFailed, err)
		}
		// Set before the handover, not after: on Unix, Respawn replaces
		// this process image outright and never returns, so anything
		// after it is Windows-only or a failure. Either way the port
		// file now belongs to the successor.
		handingOver = true
		if err := service.Respawn(path, actualPort); err != nil {
			handingOver = false
			return fmt.Errorf("%w: %v", ErrRestartFailed, err)
		}
		// A successor that never comes up is the one outcome nobody
		// would notice: this process is gone, so there is nothing left
		// to retry or report it. Wait for it to answer, and say so
		// plainly when it does not.
		if !waitForSuccessor(actualPort, 15*time.Second) {
			return fmt.Errorf("%w: the replacement never answered on port %d", ErrRestartFailed, actualPort)
		}
		log.Printf("restarted into the updated binary on port %d", actualPort)
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// ErrRestartFailed means an update was installed but this process could
// not hand over to it. The caller turns that into something the person
// sees, because the service is now down and only they can start it.
var ErrRestartFailed = errors.New("clawdh updated itself but could not restart")

// waitForSuccessor reports whether a clawdh is answering on port again.
func waitForSuccessor(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := service.Running(); err == nil && info != nil && info.Port == port {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func writePortFile(port int) error {
	path, err := config.PortFile()
	if err != nil {
		return err
	}
	if err := config.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(port)), 0o600)
}

// removePortFile clears the record only if it still names this
// server's port. A second `clawdh serve --port N` on a different port is
// a supported thing to do, and deleting the *first* one's record on the
// way out would strand it: status stops finding it and stop can no
// longer stop it.
func removePortFile(port int) {
	path, err := config.PortFile()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(port) {
		return
	}
	_ = os.Remove(path)
}

// configureEditors points every editor that has the Claude Code extension at
// clawdh, so its conversations each get their own credential store.
func configureEditors(home string) error {
	// The one setting clawdh writes outside its own directory and the user's
	// shell rc, so it takes an opt-out: CLAWDH_MANAGE_EDITORS=0 in the service's
	// environment leaves every editor alone.
	if config.Env("MANAGE_EDITORS") == "0" {
		return nil
	}
	self, err := service.SelfPath()
	if err != nil {
		return err
	}
	for _, ed := range editors.Installed(home) {
		if !ed.HasExtension {
			continue
		}
		// Configured means everything PointAtWrapper does, not just the value
		// it is named after. An editor set up by an older clawdh has the wrapper
		// AND the per-editor entry that overrides it, and checking only the
		// wrapper meant that editor was skipped for ever and never repaired.
		if editors.WrapperPath(ed.Settings) == self &&
			editors.ReadStoreDir(ed.Settings, accounts.SecureStorageEnvVar) == "" {
			continue
		}
		if err := editors.PointAtWrapper(ed.Settings, self); err != nil {
			return err
		}
		log.Printf("editors: %s now launches Claude through clawdh", ed.Name)
	}
	return nil
}
