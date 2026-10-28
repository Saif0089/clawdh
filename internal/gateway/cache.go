package gateway

import (
	"bytes"
	"net/http"
)

// Claude Code keeps its prompt cache for an hour when it is signed in to a
// claude.ai subscription, and for Anthropic's default five minutes when it is
// handed a token, as every gateway session is. A shared session is the same
// subscription, so the gateway gives its requests the hour the login would have
// had: past any pause over five minutes, a five-minute cache has to write the
// whole conversation again at 1.25x instead of reading it at 0.1x.
//
// It makes exactly the change Claude Code makes for a signed-in login: every
// cache breakpoint gets "ttl":"1h" and the request names the extended-TTL beta.
// The breakpoints themselves are Claude Code's own; the gateway never moves or
// adds one.
const hourCacheBeta = "extended-cache-ttl-2025-04-11"

var (
	cacheControlKey  = []byte(`"cache_control":`)
	bareBreakpoint   = []byte(`"cache_control":{"type":"ephemeral"}`)
	hourBreakpoint   = []byte(`"cache_control":{"type":"ephemeral","ttl":"1h"}`)
	messagesEndpoint = "/v1/messages"
)

// withHourCache returns body with every cache breakpoint kept for an hour, or
// body unchanged when there is nothing it can safely change: a request that is
// not a message, has no breakpoints, or already chose a lifetime for any of
// them (mixing lifetimes in one request is an error when an hour follows five
// minutes, so a request that chose for itself is left wholly as it is).
//
// It works on the bytes rather than decoding and re-encoding the JSON so
// nothing else in the prompt shifts and no cached prefix is lost. The match is
// safe on raw bytes: inside a JSON string every quote is escaped, so the
// unescaped key can only be a real cache_control field.
func withHourCache(r *http.Request, body []byte) []byte {
	if r.Method != http.MethodPost || r.URL.Path != messagesEndpoint {
		return body
	}
	bare := bytes.Count(body, bareBreakpoint)
	if bare == 0 || bare != bytes.Count(body, cacheControlKey) {
		return body
	}
	r.Header.Set("anthropic-beta", withBeta(r.Header.Get("anthropic-beta"), hourCacheBeta))
	return bytes.ReplaceAll(body, bareBreakpoint, hourBreakpoint)
}
