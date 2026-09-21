package main

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"time"

	"clawdh/internal/gateway"
	"clawdh/internal/usage"
	"clawdh/panel"
)

// The usage poller keeps every shared login's window numbers fresh whether or
// not anyone is using it. Until now the only source of an account's 5h/weekly
// utilisation was the headers on traffic the gateway forwarded — so an idle
// account had no reading at all, and a busy one had a reading as old as its
// last request. The gateway is the one place that holds each login's token
// without colliding with anyone, so it asks Anthropic's usage endpoint
// directly (the same one a machine's page reads for its own logins), on a
// schedule, and stores the answer where the boards and every machine's
// check-in already read it (account_windows). A token Anthropic refuses is
// renewed the same way a forwarded request's is.

// usagePollEvery is how often each held login is read. The usage endpoint
// rate-limits eagerly, so this stays coarse; boards poll the stored reading
// every few seconds, machines every check-in.
const usagePollEvery = 4 * time.Minute

// runUsagePoller reads every held login's usage on a schedule, forever.
func runUsagePoller(ctx context.Context, u *dbUpstream) {
	client := usage.NewClient()
	// A little jitter so restarts across a fleet never line up on the endpoint.
	time.Sleep(time.Duration(rand.IntN(20)) * time.Second)
	for {
		pollUsageOnce(ctx, u, client)
		select {
		case <-ctx.Done():
			return
		case <-time.After(usagePollEvery):
		}
	}
}

// pollUsageOnce reads each account that has a login and records its windows.
// Best-effort per account: one failing login never stops the others.
func pollUsageOnce(ctx context.Context, u *dbUpstream, client *usage.Client) {
	d := u.snapshot()
	for _, acct := range d.Accounts {
		if !acct.HasLogin() {
			continue
		}
		if err := pollAccountUsage(ctx, u, client, acct); err != nil {
			var rl *usage.RateLimited
			if errors.As(err, &rl) {
				log.Printf("usage: Anthropic is rate-limiting usage reads for %s; next round waits", acct.Name)
				return // the whole endpoint is throttled, not this one login
			}
			log.Printf("usage: reading %s: %v", acct.Name, err)
		}
	}
}

func pollAccountUsage(ctx context.Context, u *dbUpstream, client *usage.Client, acct panel.Account) error {
	mgr, err := u.managerFor(acct)
	if err != nil {
		return err
	}
	tok, err := mgr.Token(ctx)
	if err != nil {
		return err
	}
	report, err := client.Fetch(ctx, usage.Credentials{AccessToken: tok})
	if err != nil && usage.IsUnauthorized(err) {
		// Anthropic refused the token — it was rotated elsewhere. Renew and try
		// once more, exactly as a forwarded request would.
		fresh, rerr := u.Renew(acct.ID, tok)
		if rerr != nil {
			return rerr
		}
		report, err = client.Fetch(ctx, usage.Credentials{AccessToken: fresh})
	}
	if err != nil {
		return err
	}
	w, ok := windowsFromReport(report)
	if !ok {
		return errors.New("the usage reply carried no session or weekly window")
	}
	u.RecordWindows(acct.ID, w)
	return nil
}

// windowsFromReport lifts the two rolling windows the boards show — the
// current session and the all-models week — out of a usage report.
func windowsFromReport(r *usage.Report) (gateway.Windows, bool) {
	var w gateway.Windows
	found := false
	for _, l := range r.Limits {
		switch l.Kind {
		case "session":
			w.FiveH = l.Percent / 100
			if l.ResetsAt != nil {
				w.FiveHReset = *l.ResetsAt
			}
			found = true
		case "weekly_all":
			w.SevenD = l.Percent / 100
			if l.ResetsAt != nil {
				w.SevenDReset = *l.ResetsAt
			}
			found = true
		}
	}
	return w, found
}
