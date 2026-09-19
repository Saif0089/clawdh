package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The handful of calls `clawdh panel push` makes as the administrator. They live
// here rather than in the CLI so the panel's wire format has exactly one
// definition, on both ends of it.

// AdminLogin signs in as `name` — the name the panel records this session's
// changes under (the pusher's member name, for a push) — and leaves the session
// cookie in httpc's jar.
func AdminLogin(ctx context.Context, httpc *http.Client, server, password, name string) error {
	var out struct {
		Error string `json:"error"`
	}
	code, err := adminCall(ctx, httpc, http.MethodPost, server+"/api/login",
		map[string]string{"password": password, "name": name}, &out)
	if err != nil {
		return err
	}
	if code == http.StatusUnauthorized {
		return errors.New("that panel password is not right")
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}

// FindAccountID looks up the panel's id for an account by name, so the person
// running push does not have to copy one out of the interface.
func FindAccountID(ctx context.Context, httpc *http.Client, server, name string) (string, error) {
	var out struct {
		Accounts []accountView `json:"accounts"`
		Error    string        `json:"error"`
	}
	if _, err := adminCall(ctx, httpc, http.MethodGet, server+"/api/panel", nil, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", errors.New(out.Error)
	}
	for _, a := range out.Accounts {
		if strings.EqualFold(a.Name, name) {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("the panel has no account called %q — add it there first", name)
}

// PushLogin stores a base64 login against an account. pusher names the machine
// doing the push, which the panel records as a member with its own revocable
// gateway access; an empty pusher records no member.
func PushLogin(ctx context.Context, httpc *http.Client, server, accountID, credential, pusher string) error {
	var out struct {
		Error string `json:"error"`
	}
	if _, err := adminCall(ctx, httpc, http.MethodPost,
		server+"/api/accounts/"+accountID+"/login",
		map[string]string{"credential": credential, "pusher": pusher}, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}

func adminCall(ctx context.Context, httpc *http.Client, method, url string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, &buf)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reaching the panel at %s: %w", url, err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode, nil
}

// CreateAccount creates an account on the panel (named for its email) and
// returns its id, for the client's "add this login to the panel" flow.
func CreateAccount(ctx context.Context, httpc *http.Client, server, name, email, plan string) (string, error) {
	var out struct {
		Error string `json:"error"`
	}
	code, err := adminCall(ctx, httpc, http.MethodPost, server+"/api/accounts",
		map[string]string{"name": name, "email": email, "plan": plan}, &out)
	if err != nil {
		return "", err
	}
	// 409 means it already exists — fine, we just want its id.
	if code != http.StatusCreated && code != 201 && code != http.StatusConflict && out.Error != "" &&
		!strings.Contains(out.Error, "already") {
		return "", errors.New(out.Error)
	}
	return FindAccountID(ctx, httpc, server, name)
}
