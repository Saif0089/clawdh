package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"clawdh/internal/gateway"
	"clawdh/panel"
)

// runDiagnose reports, for the live database and key this gateway is configured
// with, exactly why a share would or would not resolve: whether the panel key
// unseals each account's credential, whether its access token is still valid,
// and whether its refresh token still works. It is read-mostly — the one thing
// it writes is a successfully refreshed credential, so testing the refresh never
// throws away a good token (the refresh is single-use).
//
//	clawdh-server diagnose
func runDiagnose(ctx context.Context, dsn, keyB64 string) error {
	u, err := newDBUpstream(ctx, dsn, keyB64)
	if err != nil {
		return fmt.Errorf("connecting to the panel database: %w", err)
	}
	d, err := u.store.Load()
	if err != nil {
		return fmt.Errorf("loading the panel state: %w", err)
	}

	fmt.Printf("panel: %d account(s), %d person(people), %d share(s)\n\n", len(d.Accounts), len(d.People), len(d.Shares))

	now := time.Now()
	for _, a := range d.Accounts {
		fmt.Printf("account %q (id %s)\n", a.Name, a.ID)
		if !a.HasLogin() {
			fmt.Println("  NO LOGIN stored — cannot be shared")
			fmt.Println()
			continue
		}
		raw, err := u.secret.Open(a.Credential)
		if err != nil {
			fmt.Printf("  UNSEAL FAILED: %v\n", err)
			fmt.Println("  -> the gateway's CLAWDH_PANEL_KEY does not match the key that sealed this (the panel's). This is the 401.")
			fmt.Println()
			continue
		}
		access, refreshTok, expires := parseCredential(raw)
		fmt.Printf("  unseal OK; access token %s; expires %s (%s)\n",
			present(access), expires.Format(time.RFC3339), validity(now, expires))
		if refreshTok == "" {
			fmt.Println("  NO REFRESH TOKEN — once the access token expires there is nothing to roll it forward")
			fmt.Println()
			continue
		}
		// Test the refresh. On success, persist it — the old refresh token is now
		// spent, so keeping the new one is the only safe thing to do.
		fresh, err := gateway.Refresh(ctx, refreshTok)
		if err != nil {
			fmt.Printf("  REFRESH FAILED: %v\n", err)
			fmt.Println("  -> the stored refresh token is dead. Most likely the same login is still being used first-party")
			fmt.Println("     somewhere (e.g. a local `claude-…` alias), which rotates this single-use token out from under the gateway.")
			fmt.Println()
			continue
		}
		u.persist(a.ID, fresh)
		fmt.Printf("  REFRESH OK; new token expires %s — persisted. This account should serve now.\n", fresh.ExpiresAt.Format(time.RFC3339))
		fmt.Println()
	}

	// Metering: is usage actually being recorded? The question an owner asks
	// when a board looks empty — and the box has no psql to answer it with.
	fmt.Println("metering:")
	if last, err := u.pg.LatestEventAt(ctx); err != nil {
		fmt.Printf("  usage_events: cannot read: %v\n", err)
	} else if last.IsZero() {
		fmt.Println("  usage_events: none yet — no shared session has completed a request through this gateway")
	} else {
		fmt.Printf("  usage_events: newest %s (%s ago)\n", last.Format(time.RFC3339), now.Sub(last).Round(time.Second))
	}
	if people, err := u.pg.UsageBySubject(ctx, "person", now.Add(-7*24*time.Hour)); err == nil {
		for _, p := range people {
			fmt.Printf("  this week %-20s %10.0f weighted tokens  $%.2f\n", nameOf(d.People, p.SubjectID), p.Weighted, p.CostUSD)
		}
	}
	if ws, err := u.pg.AccountWindows(ctx); err == nil {
		for _, w := range ws {
			fmt.Printf("  windows   %-20s 5h %3.0f%%  weekly %3.0f%%  (read %s ago)\n",
				nameOfAccount(d.Accounts, w.AccountID), w.FiveH*100, w.SevenD*100, now.Sub(w.UpdatedAt).Round(time.Second))
		}
	}
	fmt.Println()

	fmt.Println("shares:")
	for _, sh := range d.Shares {
		fmt.Printf("  person %q -> account %q  (keyHash %s…, sealedKey %s)\n",
			nameOf(d.People, sh.PersonID), nameOfAccount(d.Accounts, sh.AccountID),
			short(sh.KeyHash), present(string(sh.SealedKey)))
	}

	// End-to-end probe: unseal each member key and send a minimal request through
	// the running gateway on localhost, exactly as a member's Claude Code does.
	// This is what separates "our gateway rejected it (401)" from "Anthropic did
	// (e.g. 429 rate limit)".
	fmt.Println("\nprobe (through 127.0.0.1:8787):")
	for _, sh := range d.Shares {
		if len(sh.SealedKey) == 0 {
			continue
		}
		key, err := u.secret.Open(sh.SealedKey)
		if err != nil {
			fmt.Printf("  %s: cannot unseal member key: %v\n", nameOf(d.People, sh.PersonID), err)
			continue
		}
		// Confirm the key actually resolves the way the gateway will look it up.
		_, found := d.ShareByKeyHash(panel.HashToken(string(key)))
		status, snippet := probeGateway(ctx, string(key))
		fmt.Printf("  %s: key resolves=%v  gateway said HTTP %s  %s\n",
			nameOf(d.People, sh.PersonID), found, status, snippet)
	}
	return nil
}

// probeGateway sends the smallest possible message through the local gateway
// with a member key, and returns the status line and a short body snippet.
func probeGateway(ctx context.Context, memberKey string) (status, snippet string) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8787/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "err", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+memberKey)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "err", err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 300)
	n, _ := resp.Body.Read(buf)
	return resp.Status, strings.ReplaceAll(strings.TrimSpace(string(buf[:n])), "\n", " ")
}

func present(s string) string {
	if s == "" {
		return "MISSING"
	}
	return "present"
}
func validity(now, expires time.Time) string {
	if expires.IsZero() {
		return "no expiry recorded"
	}
	if now.Before(expires) {
		return "still valid for " + time.Until(expires).Round(time.Minute).String()
	}
	return "EXPIRED " + now.Sub(expires).Round(time.Minute).String() + " ago"
}
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
func nameOf(people []panel.Person, id string) string {
	for _, p := range people {
		if p.ID == id {
			return p.Name
		}
	}
	return "?"
}
func nameOfAccount(accts []panel.Account, id string) string {
	for _, a := range accts {
		if a.ID == id {
			return a.Name
		}
	}
	return "?"
}
