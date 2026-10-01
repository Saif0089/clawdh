// Command clawdh-server runs the gateway data plane. This first cut serves one
// subscription with a static member key, to prove the client->gateway->Anthropic
// path end to end; the DB-backed multi-account version builds on it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clawdh/internal/config"
	"clawdh/internal/gateway"
)

type staticUpstream struct{ key, token string }

func (s staticUpstream) Resolve(k string) (gateway.Resolution, error) {
	if k != "" && k == s.key {
		return gateway.Resolution{AccessToken: s.token, Label: "static"}, nil
	}
	return gateway.Resolution{}, gateway.ErrUnknownKey
}

func main() {
	// `clawdh-server diagnose` reports why shares do or don't resolve against the
	// live DB, then exits. Handled before flag parsing so it needs no flags.
	if len(os.Args) > 1 && os.Args[1] == "diagnose" {
		if err := runDiagnose(context.Background(), os.Getenv("DATABASE_URL"), config.Env("PANEL_KEY")); err != nil {
			fmt.Fprintln(os.Stderr, "diagnose:", err)
			os.Exit(1)
		}
		return
	}
	// `clawdh-server check` is a deploy's preflight: can this build read what it
	// would serve from? Nothing is restarted into a gateway that cannot.
	if len(os.Args) > 1 && os.Args[1] == "check" {
		if err := runCheck(context.Background(), os.Getenv("DATABASE_URL"), config.Env("PANEL_KEY")); err != nil {
			fmt.Fprintln(os.Stderr, "check:", err)
			os.Exit(1)
		}
		return
	}
	// `clawdh-server panel-serve [--addr]` runs the admin panel as a long-lived
	// server against the same database, so the panel can live on the VPS beside
	// the gateway instead of on a serverless host.
	if len(os.Args) > 1 && os.Args[1] == "panel-serve" {
		fs := flag.NewFlagSet("panel-serve", flag.ExitOnError)
		paddr := fs.String("addr", "127.0.0.1:8789", "listen address")
		_ = fs.Parse(os.Args[2:])
		if err := runPanelServe(*paddr); err != nil {
			fmt.Fprintln(os.Stderr, "panel-serve:", err)
			os.Exit(1)
		}
		return
	}
	// `clawdh-server wipe-usage [--yes]` shows what the metering tables hold
	// and, with --yes, clears them so the boards start over.
	if len(os.Args) > 1 && os.Args[1] == "wipe-usage" {
		yes := len(os.Args) > 2 && os.Args[2] == "--yes"
		if err := runWipeUsage(context.Background(), os.Getenv("DATABASE_URL"), yes); err != nil {
			fmt.Fprintln(os.Stderr, "wipe-usage:", err)
			os.Exit(1)
		}
		return
	}

	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	memberKey := flag.String("member-key", config.Env("GW_MEMBER_KEY"), "the gateway key a client presents")
	flag.Parse()

	// SIGTERM is how systemd restarts the gateway (every deploy): it ends ctx,
	// which winds serving down gracefully below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var up gateway.Upstream
	var rec gateway.Recorder // the DB upstream also meters and enforces quotas;
	var lim gateway.Limiter  // the static path does neither.
	var dbu *dbUpstream
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		u, err := newDBUpstream(ctx, dsn, config.Env("PANEL_KEY"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "gateway: connecting to the panel database:", err)
			os.Exit(1)
		}
		up, rec, lim, dbu = u, u, u, u
		fmt.Println("clawdh-server: serving from the panel database")
		// Keep every shared login's usage reading fresh, idle or not.
		go runUsagePoller(ctx, u)
	} else {
		token := config.Env("GW_TOKEN")
		if token == "" || *memberKey == "" {
			fmt.Fprintln(os.Stderr, "set DATABASE_URL + CLAWDH_PANEL_KEY, or CLAWDH_GW_TOKEN + --member-key")
			os.Exit(2)
		}
		up = staticUpstream{key: *memberKey, token: token}
	}
	srv := &http.Server{Addr: *addr, Handler: gateway.New(up, rec, lim)}
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Let the answers in flight finish — a restart used to cut every member's
		// stream mid-answer — but not for long: systemd stops waiting at 90s, and
		// the write queue still has to be flushed after this.
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = srv.Shutdown(c)
		cancel()
		close(stopped)
	}()
	fmt.Printf("clawdh-server on http://%s (forwarding to api.anthropic.com)\n", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	<-stopped
	// Last: anything still queued for the database — above all a rotated
	// credential, which is the only one that works once it has been issued.
	if dbu != nil {
		dbu.drain(10 * time.Second)
	}
}
