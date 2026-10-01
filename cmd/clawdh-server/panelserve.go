package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clawdh/internal/config"
	"clawdh/panel"
	"clawdh/panelpg"
)

// runPanelServe serves the admin panel over HTTP, backed by the same Postgres
// the gateway reads. It is the long-running equivalent of the serverless
// handler in api/index.go, so the panel can live on the VPS beside the gateway
// and the database — no serverless host, and no database anywhere but here:
//
//	clawdh-server panel-serve --addr 127.0.0.1:8789
//
// The domain stays wherever it is; whatever serves it proxies to this address.
func runPanelServe(addr string) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("set DATABASE_URL")
	}
	key := config.Env("PANEL_KEY")
	if key == "" {
		return errors.New("set CLAWDH_PANEL_KEY")
	}
	secret, err := panel.SecretFromBase64(key)
	if err != nil {
		return fmt.Errorf("reading the panel key: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	back, err := panelpg.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to the panel database: %w", err)
	}
	store := panel.NewStoreWithBackend(back)
	// usage boards and the remote-jobs channel are both served by the backend,
	// exactly as the serverless handler wires them.
	handler := panel.NewServer(store, secret, back, back).Handler()

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()
	fmt.Printf("clawdh-server panel on http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
