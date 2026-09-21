// Command clawdh-server runs the gateway data plane. This first cut serves one
// subscription with a static member key, to prove the client->gateway->Anthropic
// path end to end; the DB-backed multi-account version builds on it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

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

	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	memberKey := flag.String("member-key", config.Env("GW_MEMBER_KEY"), "the gateway key a client presents")
	flag.Parse()

	var up gateway.Upstream
	var rec gateway.Recorder // the DB upstream also meters and enforces quotas;
	var lim gateway.Limiter  // the static path does neither.
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		u, err := newDBUpstream(context.Background(), dsn, config.Env("PANEL_KEY"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "gateway: connecting to the panel database:", err)
			os.Exit(1)
		}
		up, rec, lim = u, u, u
		fmt.Println("clawdh-server: serving from the panel database")
		// Keep every shared login's usage reading fresh, idle or not.
		go runUsagePoller(context.Background(), u)
	} else {
		token := config.Env("GW_TOKEN")
		if token == "" || *memberKey == "" {
			fmt.Fprintln(os.Stderr, "set DATABASE_URL + CLAWDH_PANEL_KEY, or CLAWDH_GW_TOKEN + --member-key")
			os.Exit(2)
		}
		up = staticUpstream{key: *memberKey, token: token}
	}
	h := gateway.New(up, rec, lim)
	fmt.Printf("clawdh-server on http://%s (forwarding to api.anthropic.com)\n", *addr)
	if err := http.ListenAndServe(*addr, h); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
