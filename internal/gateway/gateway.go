// Package gateway is clawdh's data plane: a reverse proxy that lets many people
// share one Claude subscription at once, without any of them ever holding its
// credential.
//
// Each person's Claude Code is pointed at this gateway in "gateway mode"
// (ANTHROPIC_BASE_URL + a per-person key). The gateway authenticates the key,
// swaps in the one subscription's real token, and forwards to Anthropic —
// streaming the reply straight back. The subscription's OAuth token is
// refreshed centrally here, so no client ever rotates it, which is the whole
// reason two live sessions on one login stop colliding.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

// ErrUnknownKey means the presented gateway key belongs to no live share: it was
// never issued, or its share was revoked. The gateway answers it with 401, which
// is how revoking a share cuts a member off instantly. Any other error from
// Resolve means the share is real but its subscription login could not produce a
// token right now (a failed central refresh, usually because the same login is
// still being used first-party somewhere) — a 502, not a 401, because the member
// did nothing wrong and retrying may help.
var ErrUnknownKey = errors.New("unknown or revoked gateway key")

const (
	anthropicHost = "api.anthropic.com"
	// oauthBeta is the anthropic-beta value a subscription (OAuth) token needs
	// on /v1/messages — verified live against a real Max login.
	oauthBeta        = "oauth-2025-04-20"
	anthropicVersion = "2023-06-01"
)

// Resolution is what a member key maps to: the subscription token to forward
// with, a log label (never the token), and the identity — account + person —
// the key belongs to, which the gateway attributes usage to.
type Resolution struct {
	AccessToken string
	Label       string
	AccountID   string
	PersonID    string
}

// Upstream identifies which subscription a member's request should be served by.
type Upstream interface {
	// Resolve maps a member's gateway key to its Resolution. A nil error means
	// forward; ErrUnknownKey means answer 401; any other error means the share is
	// valid but its login is unusable right now, answered with 502.
	Resolve(memberKey string) (Resolution, error)
}

// Renewer is an Upstream that can replace an access token Anthropic has just
// refused. Anthropic revokes a login's previous access token whenever its
// credential rotates — the panel re-adding the login, `clawdh-server
// diagnose` testing the refresh, the same account used first-party — so a
// token the clock still calls valid can come back 401. A gateway whose
// upstream renews replays the request once with the new token instead of
// handing the member the 401; an upstream that can't renew makes it a
// collision (502 with the "add the login again" message).
type Renewer interface {
	// Renew returns an access token for the account other than bad, or an error
	// if the login can't produce one.
	Renew(accountID, bad string) (string, error)
}

// maxRequestBody is the most the gateway buffers of a request so it can be
// replayed after a token renewal. Claude Code requests are JSON — large
// contexts run to a few MB — never streams.
const maxRequestBody = 64 << 20

// retryWithToken is how ModifyResponse hands a renewed token to the
// ErrorHandler, which replays the request with it.
type retryWithToken struct{ token string }

func (e *retryWithToken) Error() string { return "retry with a renewed token" }

// Limiter reports a member's standing against their tightest clawdh quota, so
// the gateway can answer 429 before forwarding when they are over — the same
// shape a real spend limit uses — and warn them as they approach. Optional; nil
// means no quotas are enforced.
type Limiter interface {
	// Status returns the standing for a request — a person using a specific
	// account — against the tightest quota that applies: the person's own, the
	// account's, or the org's. A request with no applicable quota comes back
	// zero-valued (Fraction 0, Over false).
	Status(personID, accountID string) QuotaStatus
}

// QuotaStatus is a person's standing against the tightest clawdh quota that
// applies to them: whether they are over it (turned away with a 429), how much
// of it is used (0..1+, where >=0.75 is a warning), when the window resets, and
// the message to show at the cap.
type QuotaStatus struct {
	Over     bool
	Fraction float64
	ResetAt  time.Time
	Message  string
}

// warnFraction is where a member starts being told they are approaching their
// clawdh quota — the 75% mark Anthropic's own spend-limit warnings use. Claude
// Code turns anthropic-ratelimit-unified-status: allowed_warning into an
// "approaching usage limit" notice on the member's next response.
const warnFraction = 0.75

// New builds the gateway handler over an Upstream. rec, if non-nil, meters each
// forwarded response off the hot path; lim, if non-nil, is checked before each
// forward and answers over-quota members with a 429.
func New(up Upstream, rec Recorder, lim Limiter) http.Handler {
	proxy := &httputil.ReverseProxy{
		// -1 flushes every write immediately, which is what keeps streamed
		// (SSE) responses streaming instead of buffering to the end.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			ctx := resp.Request.Context()
			// A 401 on a resolved share means Anthropic refused the login's
			// access token, not the member's key. Renew it and replay once;
			// if that can't be done, it is the collision the 502 describes.
			if resp.StatusCode == http.StatusUnauthorized {
				if ident, _ := ctx.Value(identKey).(Event); ident.AccountID != "" {
					bad, _ := ctx.Value(tokenKey).(string)
					retried, _ := ctx.Value(retriedKey).(bool)
					if ren, ok := up.(Renewer); ok && !retried {
						if fresh, err := ren.Renew(ident.AccountID, bad); err == nil && fresh != bad {
							resp.Body.Close()
							return &retryWithToken{token: fresh}
						}
					}
					resp.Body.Close()
					replaceWithDenial(resp, http.StatusBadGateway, "api_error", collisionMessage)
					return nil
				}
			}
			// Capture the subscription's real window utilisation from Anthropic's
			// own unified rate-limit headers (the authoritative "% of the 5h /
			// weekly window") before those headers are replaced below. Per account
			// (the shared login), keyed by the resolved AccountID.
			if wr, ok := rec.(WindowRecorder); ok {
				if id, _ := ctx.Value(identKey).(Event); id.AccountID != "" {
					if w, has := parseWindows(resp.Header); has {
						wr.RecordWindows(id.AccountID, w)
					}
				}
			}
			// The member's clawdh quota is authoritative for what they may spend,
			// so replace the upstream's own rate-limit headers (which reflect the
			// whole shared login — everyone at once — not this person) with
			// clawdh's per-person standing. A person with no quota (or none used
			// yet) keeps the upstream headers untouched.
			if st, ok := ctx.Value(quotaKey).(QuotaStatus); ok && (st.Fraction > 0 || st.Over) {
				stripUnifiedRateLimit(resp.Header)
				status := "allowed"
				if st.Fraction >= warnFraction {
					status = "allowed_warning"
				}
				resp.Header.Set("anthropic-ratelimit-unified-status", status)
				if !st.ResetAt.IsZero() {
					resp.Header.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(st.ResetAt.Unix(), 10))
				}
			}
			if rec == nil || resp.Body == nil {
				return nil
			}
			ident, _ := ctx.Value(identKey).(Event)
			ident.RequestID = resp.Header.Get("request-id")
			resp.Body = &meteringBody{inner: resp.Body, rec: rec, base: ident}
			return nil
		},
		Director: func(r *http.Request) {
			token, _ := r.Context().Value(tokenKey).(string)
			host, scheme := anthropicHost, "https"
			if testTargetHost != "" { // set only by tests
				host, scheme = testTargetHost, "http"
			}
			r.URL.Scheme = scheme
			r.URL.Host = host
			r.Host = host

			// Replace the member's key with the subscription's real credential,
			// and add exactly what a first-party subscription request carries.
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Del("x-api-key")
			// The compatibility guide says to forward anthropic-version and
			// anthropic-beta unchanged; the version is only filled in when the
			// client sent none, and the beta list keeps everything the client
			// asked for plus the OAuth capability a subscription token needs.
			if r.Header.Get("anthropic-version") == "" {
				r.Header.Set("anthropic-version", anthropicVersion)
			}
			r.Header.Set("anthropic-beta", withBeta(r.Header.Get("anthropic-beta"), oauthBeta))

			// Drop the client's Accept-Encoding so the meter can read the body.
			// Claude Code sends `Accept-Encoding: gzip, br`; if we forward that, Go's
			// transport hands the compressed response through untouched (it only
			// transparently decodes gzip it requested itself), and Anthropic often
			// answers in Brotli — which Go's stdlib can't decode at all. Either way
			// the token scanner would see compressed bytes and meter nothing, which
			// is exactly why usage_events stayed empty while window headers (plain
			// HTTP headers) recorded fine. Dropping the header lets the transport
			// request gzip on its own and decode it transparently, so the body
			// reaches both the meter and the client as plaintext. Accept-Encoding is
			// a hop preference, not response content, so this doesn't rewrite the
			// body Claude Code recovers errors from — identity is always valid.
			r.Header.Del("Accept-Encoding")
		},
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		var retry *retryWithToken
		if errors.As(err, &retry) && r.GetBody != nil {
			if body, err := r.GetBody(); err == nil {
				r.Body = body
				ctx := withToken(r.Context(), retry.token)
				ctx = context.WithValue(ctx, retriedKey, true)
				proxy.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		// The stock behaviour: a plain 502 for a transport failure.
		if !errors.Is(err, context.Canceled) {
			log.Printf("gateway: upstream error: %v", err)
		}
		w.WriteHeader(http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := memberKey(r)
		if key == "" {
			deny(w, http.StatusUnauthorized, "authentication_error",
				"No gateway key was sent. Run this account through clawdh (`clawdh shared <name>`), which supplies your key.")
			return
		}
		res, err := up.Resolve(key)
		switch {
		case errors.Is(err, ErrUnknownKey):
			deny(w, http.StatusUnauthorized, "authentication_error",
				"Your access to this shared account was removed, or this key isn't one the gateway knows. Ask whoever shared it to give you access again — a running `clawdh shared` session reconnects on its own once they do; `clawdh list` shows what you can run.")
			return
		case err != nil:
			// The share is real but its shared login can't be used right now —
			// almost always because the same account is still signed in and in
			// use first-party somewhere, which rotates the login's refresh token
			// out from under the gateway. Nothing the member does will fix it, so
			// say what will, and don't have the client retry into it.
			deny(w, http.StatusBadGateway, "api_error", collisionMessage)
			return
		}
		// Buffer the body so the request can be replayed if the login's token
		// turns out to have been revoked (see Renewer).
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil || len(body) > maxRequestBody {
			deny(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "That request is too large for the gateway to forward.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		r.ContentLength = int64(len(body))
		// Quota gate: an over-cap member is turned away here, before their request
		// reaches Anthropic, with a definitive 429 pointing at the window reset. A
		// member under the cap is forwarded, and their standing rides along on the
		// context so ModifyResponse can put a warning on the way back.
		var quota QuotaStatus
		if lim != nil && (res.PersonID != "" || res.AccountID != "") {
			quota = lim.Status(res.PersonID, res.AccountID)
			if quota.Over {
				denyQuota(w, resetSeconds(quota.ResetAt), quota.Message)
				return
			}
		}
		ctx := withToken(r.Context(), res.AccessToken)
		ctx = context.WithValue(ctx, identKey, Event{AccountID: res.AccountID, PersonID: res.PersonID})
		ctx = context.WithValue(ctx, quotaKey, quota)
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// collisionMessage is what a member sees when the shared login can't serve —
// the one thing that fixes it is the owner adding the login to the panel again.
const collisionMessage = "The shared login for this account stopped working — usually because the same account is also being used directly on another machine, which invalidates the copy the gateway holds. The account's owner needs to add its login to the panel again."

// replaceWithDenial rewrites an upstream response, in place, into the same
// definitive error envelope deny writes — for the case where the upstream's
// answer would mislead the member (a 401 that is the login's, not theirs).
func replaceWithDenial(resp *http.Response, status int, errType, message string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	})
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header = http.Header{}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("x-should-retry", "false")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.ContentLength = int64(len(body))
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

// denyQuota answers an over-quota member with the shape a real spend limit uses:
// 429, error.type billing_error, no retry, and a retry-after at the reset — so
// Claude Code shows the message and stops rather than retrying into the cap.
func denyQuota(w http.ResponseWriter, retryAfterSec int, message string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "billing_error", "message": message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false")
	if retryAfterSec > 0 {
		w.Header().Set("retry-after", strconv.Itoa(retryAfterSec))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// deny answers with the Anthropic error envelope Claude Code expects, and with
// x-should-retry: false. Every gateway-issued error here is definitive — a
// missing or revoked key, a login that needs re-adding — so retrying is pure
// delay; without the header Claude Code retries a 5xx up to ten times with
// backoff before the person ever sees the message.
func deny(w http.ResponseWriter, status int, errType, message string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// memberKey pulls the caller's gateway key from where Claude Code puts it in
// gateway mode: a Bearer Authorization (ANTHROPIC_AUTH_TOKEN) or x-api-key
// (ANTHROPIC_API_KEY).
func memberKey(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// withBeta ensures need is present in a comma-separated anthropic-beta header,
// keeping whatever betas Claude Code already asked for.
func withBeta(have, need string) string {
	if have == "" {
		return need
	}
	for _, b := range strings.Split(have, ",") {
		if strings.TrimSpace(b) == need {
			return have
		}
	}
	return have + "," + need
}

// testTargetHost redirects the proxy to a stand-in Anthropic in tests; empty in production.
var testTargetHost string

type ctxKey int

const (
	tokenKey   ctxKey = iota
	identKey          // carries the resolved Event{AccountID,PersonID} for metering
	quotaKey          // carries the member's QuotaStatus for the response warning
	retriedKey        // set on the one replay after a token renewal, so a second 401 is final
)

// resetSeconds is how many whole seconds until a window reset, at least 1 (a
// retry-after of 0 reads as "retry now", which would loop into the cap).
func resetSeconds(reset time.Time) int {
	if reset.IsZero() {
		return 0
	}
	s := int(time.Until(reset).Seconds())
	if s < 1 {
		return 1
	}
	return s
}

// stripUnifiedRateLimit drops the upstream's own anthropic-ratelimit-unified-*
// headers so clawdh's per-person quota state is the only rate-limit signal the
// member's client sees. The upstream set reflects the shared login as a whole,
// which would mislead any one member reading their /usage.
func stripUnifiedRateLimit(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-unified-") {
			h.Del(k)
		}
	}
}

// Windows is a subscription's real utilisation of its rolling usage windows,
// read from Anthropic's own unified rate-limit headers — the authoritative
// "% of the 5h / weekly window" that /usage shows. Fractions are 0..1.
type Windows struct {
	FiveH       float64
	SevenD      float64
	FiveHReset  time.Time
	SevenDReset time.Time
}

// WindowRecorder stores a subscription's window utilisation. A Recorder that
// also implements it is handed each account's readings as responses come back;
// a Recorder that does not is simply never asked.
type WindowRecorder interface {
	RecordWindows(accountID string, w Windows)
}

// parseWindows reads the unified utilisation/reset headers off a response. has
// is false when neither window's utilisation is present (most non-Anthropic or
// error responses). Anthropic reports utilisation as a percentage; a value >1
// is treated as one (÷100), a value <=1 as an already-fractional reading, so
// either encoding lands as 0..1.
func parseWindows(h http.Header) (w Windows, has bool) {
	frac := func(name string) (float64, bool) {
		v := h.Get(name)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		if f > 1 {
			f /= 100
		}
		return f, true
	}
	unix := func(name string) time.Time {
		if sec, err := strconv.ParseInt(h.Get(name), 10, 64); err == nil && sec > 0 {
			return time.Unix(sec, 0)
		}
		return time.Time{}
	}
	if f, ok := frac("anthropic-ratelimit-unified-5h-utilization"); ok {
		w.FiveH, has = f, true
	}
	if f, ok := frac("anthropic-ratelimit-unified-7d-utilization"); ok {
		w.SevenD, has = f, true
	}
	w.FiveHReset = unix("anthropic-ratelimit-unified-5h-reset")
	w.SevenDReset = unix("anthropic-ratelimit-unified-7d-reset")
	return w, has
}

func withToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey, token)
}
