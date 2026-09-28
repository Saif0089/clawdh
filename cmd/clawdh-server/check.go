package main

import (
	"context"
	"errors"
	"fmt"

	"clawdh/panel"
	"clawdh/panelpg"
)

// runCheck is what a deploy runs before it restarts the gateway into a new
// build: can this binary, with this environment, read the panel state it would
// serve from, and open the logins in it? A gateway that cannot is not started —
// the running one keeps serving instead.
//
// It reads, and never refreshes a login: a refresh token is single-use, and
// spending one here would take it from the running gateway.
//
//	clawdh-server check
func runCheck(ctx context.Context, dsn, keyB64 string) error {
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	secret, err := panel.SecretFromBase64(keyB64)
	if err != nil {
		return fmt.Errorf("the panel key: %w", err)
	}
	back, err := panelpg.Open(ctx, dsn)
	if err != nil {
		return err
	}
	u, err := newUpstream(panel.NewStoreWithBackend(back), back, secret)
	if err != nil {
		return err
	}
	d := u.current()
	logins, opened := 0, 0
	for _, a := range d.Accounts {
		if !a.HasLogin() {
			continue
		}
		logins++
		if _, err := secret.Open(a.Credential); err == nil {
			opened++
		}
	}
	fmt.Printf("check: panel state read: %d account(s), %d share(s); the panel key opens %d of %d stored login(s)\n",
		len(d.Accounts), len(d.Shares), opened, logins)
	if logins > 0 && opened == 0 {
		return errors.New("the panel key opens none of the stored logins, so no share could be served")
	}
	return nil
}
