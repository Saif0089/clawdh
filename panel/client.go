package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
)

// ErrNotEnrolled means the panel no longer recognises this machine — it was
// cut off, or the panel was rebuilt. Everything the panel lent is given back.
var ErrNotEnrolled = errors.New("this machine is no longer enrolled with the panel")

// ClientConfig is what a machine remembers about the panel it answers to.
type ClientConfig struct {
	Server     string `json:"server"`
	DeviceID   string `json:"deviceId"`
	Token      string `json:"token"`
	PersonName string `json:"personName,omitempty"`
	// Remote is whether this machine's owner turned remote help on: the consent
	// that lets the panel ask it to diagnose itself or send a transcript. Off by
	// default — nothing remote happens until the person runs `clawdh remote on`.
	Remote bool `json:"remote,omitempty"`
}

// Configured reports whether this machine answers to a panel at all. clawdh
// without one behaves exactly as it always has.
func (c ClientConfig) Configured() bool {
	return strings.TrimSpace(c.Server) != "" && strings.TrimSpace(c.Token) != ""
}

// LoadClientConfig reads the panel a machine is enrolled with. A missing file
// means it is enrolled with none.
func LoadClientConfig(path string) (ClientConfig, error) {
	var c ClientConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}

// PusherName is the member name to record for the machine pushing a login: the
// person this machine already enrolled as with the same panel, or the machine's
// hostname when it is not enrolled there. It is only a label for the panel — the
// account's own login is unaffected either way.
func PusherName(server string) string {
	server = strings.TrimRight(server, "/")
	if path, err := config.PanelClientFile(); err == nil {
		if c, err := LoadClientConfig(path); err == nil &&
			c.Configured() && strings.TrimRight(c.Server, "/") == server &&
			strings.TrimSpace(c.PersonName) != "" {
			return c.PersonName
		}
	}
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return h
	}
	return "the machine that added it"
}

// SaveClientConfig writes it back, readable only by this user: it holds this
// machine's token.
func SaveClientConfig(path string, c ClientConfig) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// Enroll trades a one-shot join code for this machine's own token.
func Enroll(ctx context.Context, server, code, machine string) (ClientConfig, error) {
	var out struct {
		DeviceID   string `json:"deviceId"`
		Token      string `json:"token"`
		PersonName string `json:"personName"`
		Error      string `json:"error"`
	}
	if err := post(ctx, http.DefaultClient, server+"/api/v1/enroll", "",
		map[string]string{"code": code, "machine": machine}, &out); err != nil {
		return ClientConfig{}, err
	}
	if out.Error != "" {
		return ClientConfig{}, errors.New(out.Error)
	}
	return ClientConfig{Server: strings.TrimRight(server, "/"), DeviceID: out.DeviceID, Token: out.Token, PersonName: out.PersonName}, nil
}

// Client reconciles this machine against the panel.
type Client struct {
	Config   ClientConfig
	Accounts *accounts.Manager
	HTTP     *http.Client
	// AfterChange runs when something was gained or given back, so the caller
	// can re-sync shell aliases. Optional.
	AfterChange func()
	// SharesPath, when set, is where the gateway shares this machine was granted
	// are cached (0600), so `clawdh shared <slug>` and the shell aliases can run a
	// shared account without the key ever touching a dotfile. Empty on a machine
	// that only ever runs its own accounts.
	SharesPath string
}

// GatewayShare is one shared account this machine may run through the gateway:
// where to route, and this person's key. It mirrors the panel's check-in reply.
type GatewayShare struct {
	Account string `json:"account"`
	Slug    string `json:"slug"`
	Gateway string `json:"gateway"`
	Key     string `json:"key"`
}

// LoadShares reads the cached gateway shares. A missing file is no shares.
func LoadShares(path string) ([]GatewayShare, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []GatewayShare
	return out, json.Unmarshal(raw, &out)
}

// SaveShares writes the cached gateway shares, readable only by this user: it
// holds gateway keys.
func SaveShares(path string, shares []GatewayShare) error {
	raw, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// Change is what one check-in altered, for the caller to report: the accounts
// this machine can newly use, and the ones it can no longer use.
type Change struct {
	Gained []string
	Lost   []string
	// Jobs are the consented remote jobs the panel handed back this check-in for
	// this machine to run. Always empty unless the owner turned remote help on.
	Jobs []RemoteJob
	// Notices are short things to tell the person at this machine — a quota
	// warning, an account whose login broke — that the panel computes for them.
	// Shown as desktop notifications, deduplicated by ID so a standing condition
	// isn't re-shown on every check-in.
	Notices []Notice
}

// Notice is one short, self-contained thing to tell the person at a machine.
// ID identifies the underlying event (a window's threshold, one login breakage)
// so the client shows it once however many check-ins carry it; Body is the line
// itself, kept short.
type Notice struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// Empty reports whether the check-in changed the set of shared accounts. Jobs
// are handled separately, so they do not count as a share change.
func (c Change) Empty() bool { return len(c.Gained) == 0 && len(c.Lost) == 0 }

// RemoteJob is one thing the panel asked this machine to do: look at itself and
// report back. Kind is diagnose | sessions | transcript; Params is the argument
// (a session id, for a transcript).
type RemoteJob struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Params      string `json:"params"`
	RequestedBy string `json:"requestedBy"` // the panel signer who asked, so the owner knows who
}

// CheckIn asks the panel which accounts are shared with this machine and makes
// that true: it caches the gateway keys (so `clawdh shared <slug>` and the shell
// aliases can run them) and re-syncs aliases when the set changed.
//
// Nothing here writes a Claude credential. That is the point of the gateway:
// the login stays on the server, the machine only ever holds a scoped key, and
// taking access away is the server dropping the share — the key simply stops
// working on the next request. This is what makes sharing safe and switching
// non-fragile, in place of the old model that copied logins onto disk.
func (c *Client) CheckIn(ctx context.Context) (Change, error) {
	var change Change
	if !c.Config.Configured() {
		return change, nil
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 20 * time.Second}
	}

	var out struct {
		Gateway []GatewayShare `json:"gateway"`
		Jobs    []RemoteJob    `json:"jobs"`
		Notices []Notice       `json:"notices"`
		Error   string         `json:"error"`
	}
	// Report this machine's remote-help consent every check-in; the panel only
	// hands back jobs when it is on.
	body := struct {
		Remote bool `json:"remote"`
	}{Remote: c.Config.Remote}
	err := post(ctx, httpc, c.Config.Server+"/api/v1/checkin", c.Config.Token, body, &out)
	if errors.Is(err, errUnauthorized) {
		// Cut off: forget every shared account, then say so.
		return c.forgetShares(), ErrNotEnrolled
	}
	if err != nil {
		return change, err
	}
	if out.Error != "" {
		return change, errors.New(out.Error)
	}

	change = c.applyShares(out.Gateway)
	if c.Config.Remote {
		change.Jobs = out.Jobs
	}
	// Notices are about the person, not remote help, so they ride back whether or
	// not this machine offers itself for jobs.
	change.Notices = out.Notices
	if !change.Empty() && c.AfterChange != nil {
		c.AfterChange()
	}
	return change, nil
}

// ReportResult posts a machine's answer to one of its remote jobs back to the
// panel. status is "done" or "error"; result is the answer (or the reason).
func (c *Client) ReportResult(ctx context.Context, jobID, status, result string) error {
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 20 * time.Second}
	}
	var out struct {
		Error string `json:"error"`
	}
	body := struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}{Status: status, Result: result}
	if err := post(ctx, httpc, c.Config.Server+"/api/v1/jobs/"+jobID+"/result", c.Config.Token, body, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}

// applyShares caches the shares just fetched and reports what changed, by
// account name, against what was cached — so an unchanged check-in reports
// nothing and does not rewrite dotfiles.
func (c *Client) applyShares(next []GatewayShare) Change {
	var change Change
	if c.SharesPath == "" {
		return change
	}
	prev, _ := LoadShares(c.SharesPath)
	change = diffShares(prev, next)
	if !change.Empty() {
		_ = SaveShares(c.SharesPath, next)
	}
	return change
}

// forgetShares drops every cached share, as when this machine is cut off.
func (c *Client) forgetShares() Change {
	change := c.applyShares(nil)
	if !change.Empty() && c.AfterChange != nil {
		c.AfterChange()
	}
	return change
}

// diffShares reports which accounts are newly usable and which are gone, keyed
// by the stable slug and reported by name.
func diffShares(prev, next []GatewayShare) Change {
	was := map[string]string{}
	for _, s := range prev {
		was[s.Slug] = s.Account
	}
	now := map[string]string{}
	for _, s := range next {
		now[s.Slug] = s.Account
	}
	var change Change
	for slug, name := range now {
		if _, had := was[slug]; !had {
			change.Gained = append(change.Gained, name)
		}
	}
	for slug, name := range was {
		if _, still := now[slug]; !still {
			change.Lost = append(change.Lost, name)
		}
	}
	return change
}

var errUnauthorized = errors.New("unauthorized")

func post(ctx context.Context, httpc *http.Client, url, bearer string, body, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("the panel answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
