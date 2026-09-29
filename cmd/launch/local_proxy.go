package launch

// local_proxy.go — a tiny local reverse proxy `oaica serve` runs in front of
// the spawned llama-server, porting the SAME /v1/messages system-message
// normalization prism-api-router/src/index.ts applies for cloud requests
// (see extractModelName there). Without this, Claude Code talking directly
// to a local llama-server hits the exact "Jinja Exception: System message
// must be at the beginning" crash the router fix solved — that fix lives
// server-side in the Cloudflare Worker, which `oaica serve` bypasses
// entirely (it's a direct local process, no router in the loop). Local
// self-host needs its own copy of the same fix.
//
// Two distinct real violations of the strict Jinja template's "exactly one
// system message, at position 0" requirement, both confirmed via captured
// real Claude Code requests (see the router's comment for the full story):
//  1. No system message at all (content packed into the first user message
//     instead) — Anthropic's /v1/messages puts system in a top-level field;
//     when absent there's no system role anywhere.
//  2. A LATER message (not index 0) also has role="system" — mid-conversation
//     system-reminder blocks Claude Code injects.

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ollama/ollama/cmd/internal/httpbody"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// flushResponse pushes the status line and headers out now rather than when
// the handler's buffer happens to drain. A client should see its response
// begin before the backend's first body byte — with a long prompt and a slow
// prefill that can be many seconds.
func flushResponse(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// relayFlushing copies src to the response, flushing after every write.
//
// A bare io.Copy does NOT stream here. net/http buffers a handler's writes in
// a 2048-byte bufio.Writer that drains only when it fills, when the handler
// returns, or on an explicit Flush — so a relay that never flushes hands the
// client the backend's frames in 2 KiB gulps and holds anything shorter than
// that until the turn ends. For a client parsing SSE that is indistinguishable
// from not streaming at all, and it is what both of these proxies did until
// the 2026-09-26 audit (the Anthropic-wire passthrough has always flushed, and
// says why in its own copy loop).
//
// It reports what reached the client: how many bytes were written, whether the
// failure was the client leaving (a write error — the leg delivered, this side
// had nowhere to put it) rather than the body dying (a read error), and that
// error itself. The caller logs off those; neither can be turned into a status,
// because the status line is long since sent.
func relayFlushing(w http.ResponseWriter, src io.Reader) (written int64, clientGone bool, err error) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return written, true, writeErr
			}
			written += int64(n)
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, false, nil
			}
			return written, false, readErr
		}
	}
}

func normalizeSystemMessages(pathname string, body []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON (or malformed) — forward as-is, let the backend reject it.
		return body, nil
	}
	// Valid JSON that is not an object. json.Unmarshal accepts the literal
	// `null` for a map by leaving it nil and reporting NO error, so the write
	// below was an assignment to an entry in a nil map — a panic inside the
	// serve proxy's handler on a single `curl -d null`, which net/http recovers
	// per connection: the client got a dropped connection instead of an error
	// and no usage row was written (2026-09-26 audit, ninth round). There is
	// nothing here to rewrite in a number, a string, an array or null, so they
	// take the same road as malformed input.
	if parsed == nil {
		return body, nil
	}

	if pathname == "/v1/messages" {
		var strayContents []string
		if msgs, ok := parsed["messages"].([]any); ok {
			var rest []any
			for _, m := range msgs {
				mm, ok := m.(map[string]any)
				if ok && mm["role"] == "system" {
					strayContents = append(strayContents, contentToString(mm["content"]))
					continue
				}
				rest = append(rest, m)
			}
			parsed["messages"] = rest
		}
		existingSystem := textOfContent(parsed["system"])
		parts := append([]string{existingSystem}, strayContents...)
		combined := joinNonEmpty(parts, "\n\n")
		if combined == "" {
			combined = "You are a helpful assistant."
		}
		parsed["system"] = combined
	} else if msgs, ok := parsed["messages"].([]any); ok {
		var systemMsgs []string
		var rest []any
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if ok && mm["role"] == "system" {
				systemMsgs = append(systemMsgs, contentToString(mm["content"]))
				continue
			}
			rest = append(rest, m)
		}
		firstIsSystem := len(msgs) > 0
		if firstIsSystem {
			if mm, ok := msgs[0].(map[string]any); !ok || mm["role"] != "system" {
				firstIsSystem = false
			}
		}
		if len(systemMsgs) > 0 || !firstIsSystem {
			combined := joinNonEmpty(systemMsgs, "\n\n")
			if combined == "" {
				combined = "You are a helpful assistant."
			}
			sysMsg := map[string]any{"role": "system", "content": combined}
			parsed["messages"] = append([]any{sysMsg}, rest...)
		}
	}

	return json.Marshal(parsed)
}

// contentToString renders a message's content as text.
func contentToString(v any) string {
	return textOfContent(v)
}

// textOfContent renders an Anthropic content value as prose: a string as
// itself, and an array of content blocks by joining the text of each TEXT
// block. Anything else states nothing — this renders a SYSTEM prompt, and a
// system value that is neither a string nor an array of text is not prose on
// any leg.
//
// The array case used to be json.Marshal'd whole, so Claude Code's system
// prompt — which arrives as an array of text blocks, one of them carrying a
// cache_control marker — was delivered to the model as escaped JSON with the
// block metadata inline: `[{\"cache_control\":...,\"text\":\"You are
// Claude Code.\",\"type\":\"text\"},...]` instead of prose. Every
// instruction in it was present and unrecognisable (2026-09-26 audit, fourth
// round). anthropic.FromMessagesRequest already joined these blocks with a
// blank line; this path now agrees with it.
//
// The remaining arms were rewritten the same way in round 50. A non-text block
// used to be kept as its JSON "so it is not silently dropped", which for an
// image meant the payload — up to a megabyte of base64 — was pasted into the
// system prompt as prose: no leg's converter puts an image there (the local
// and gateway converters both take text blocks only, from a system array or a
// system value alike), the model was charged for the bytes, and the picture
// itself was still never sent. A system value of any other kind (a number, a
// bool, an object) is likewise not prose on either sibling leg, where it
// contributes nothing at all.
func textOfContent(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, b := range x {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] != "text" {
				continue
			}
			if t, ok := bm["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func joinNonEmpty(parts []string, sep string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, sep)
}

// runLocalNormalizingProxy listens on listenPort and forwards every request
// to http://127.0.0.1:backendPort, rewriting the body for POST /v1/messages
// and POST /v1/chat/completions along the way. Blocks — run in a goroutine.
func RunLocalNormalizingProxy(listenPort, backendPort int) error {
	return RunNormalizingProxyOn("127.0.0.1", listenPort, backendPort, "")
}

// RunNormalizingProxyOn is RunLocalNormalizingProxy with an explicit bind
// host and optional bearer token.
//
// bindHost of "0.0.0.0" exposes the model to the network. apiKey is then
// the ONLY thing standing between the internet and an unauthenticated
// inference server, so when a key is set every request must carry
// `Authorization: Bearer <key>`. An empty key disables the check — fine
// on loopback, dangerous off it, which is why `oaica serve` refuses that
// combination unless explicitly forced.
//
// Note the BACKEND stays on 127.0.0.1 regardless: llama-server itself is
// never exposed, only this proxy is, so the auth check cannot be bypassed
// by hitting the backend port directly from off-box.
// normalizingProxyHeaderTimeout is a var so a test does not wait ten seconds.
var normalizingProxyHeaderTimeout = 10 * time.Second

func RunNormalizingProxyOn(bindHost string, listenPort, backendPort int, apiKey string) error {
	return RunNormalizingProxyOnKeyed(bindHost, listenPort, backendPort, apiKey, "")
}

// loopbackOrigin reports whether an Origin header names a page served from this machine.
func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	// The machine's own name is a page served from this machine (localHostHeader accepts it too).
	if name, err := os.Hostname(); err == nil && h != "" && h == strings.ToLower(name) {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// RunNormalizingProxyOnKeyed is RunNormalizingProxyOn with a key the proxy presents to the backend in
// place of whatever the client sent: llama-server is started with it, so its internal port is no longer
// an unauthenticated way around the proxy's own key for any other local user (2026-09-29 audit,
// round 126, F126-L1-2).
func RunNormalizingProxyOnKeyed(bindHost string, listenPort, backendPort int, apiKey, backendKey string) error {
	backend := fmt.Sprintf("http://127.0.0.1:%d", backendPort)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A web page is not this machine's client. The backend answers every Origin with
		// Access-Control-Allow-Origin: <that origin> and serves a text/plain POST it never preflights,
		// so on a loopback bind with no key any site the operator visited could drive the model and
		// read its answers. Pages served from this machine (a local chat UI) still work (2026-09-29
		// audit, round 126, F126-L1-1).
		//
		// Origin decides when there is one. Sec-Fetch-Site does not: a browser treats localhost and
		// 127.0.0.1 (and http and https) as different SITES, so a chat UI on http://localhost:3000
		// pointed at the printed http://127.0.0.1:PORT is `cross-site` by the browser's reckoning and
		// still a page from this machine. `same-origin` cannot be forged by a page, so a page served
		// through this proxy's own origin (behind nginx or a tunnel) passes on it (2026-09-29 audit,
		// round 127, F127-L2-1).
		// Only when no key is set: with one, a blind page cannot authenticate, and refusing a page behind
		// nginx or a tunnel that presents it broke that setup for nothing (F127-L1-2).
		if apiKey == "" && isLoopbackBind(bindHost) && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			o := r.Header.Get("Origin")
			if o != "" && !loopbackOrigin(o) {
				http.Error(w, `{"error":{"message":"cross-origin request refused","type":"forbidden"}}`, http.StatusForbidden)
				return
			}
			if o == "" && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, `{"error":{"message":"cross-site request refused","type":"forbidden"}}`, http.StatusForbidden)
				return
			}
		}
		// /health stays unauthenticated ONLY on a loopback bind — on a
		// network-facing bind (0.0.0.0) it is behind the same bearer check
		// as everything else, so it leaks nothing (liveness, backend
		// presence) to off-box scanners (audit L2).
		if apiKey != "" && (r.URL.Path != "/health" || !isLoopbackBind(bindHost)) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(apiKey)) != 1 {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"missing or invalid Authorization: Bearer <key>","type":"unauthorized"}}`))
				return
			}
		}
		body, err := httpbody.ReadCapped(r.Body, httpbody.DefaultMax, "the request body")
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/chat/completions") && len(body) > 0 {
			if fixed, err := normalizeSystemMessages(r.URL.Path, body); err == nil {
				body = fixed
			}
		}

		// The caller's context, not a detached one: a client that disconnects
		// mid-request must release the backend with it, or a backend that
		// accepted and then said nothing holds this handler and both sockets
		// until oaica exits. proxyUpstreamClient bounds connection setup only —
		// never the response, since a slow local model may legitimately stream
		// past any fixed timeout.
		req, err := http.NewRequestWithContext(r.Context(), r.Method, backend+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.ContentLength = int64(len(body))
		if backendKey != "" {
			req.Header.Set("Authorization", "Bearer "+backendKey)
		}

		resp, err := proxyUpstreamClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flushResponse(w)
		_, _, _ = relayFlushing(w, resp.Body)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindHost, listenPort))
	if err != nil {
		return err
	}
	// A header timeout: the proxy is meant to face the network behind a bearer check that runs only
	// once the headers are in, so a half-sent request held a goroutine and a descriptor for ever
	// and 500 of them took the model offline unauthenticated. No whole-request timeout: streamed
	// turns run long (2026-09-29 audit, round 121, F121-L2-3).
	srv := &http.Server{
		Handler:           rebindingGuard(isLoopbackBind(bindHost), handler),
		ReadHeaderTimeout: normalizingProxyHeaderTimeout,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}

// isLoopbackBind reports whether bindHost is a loopback-only bind.
func isLoopbackBind(bindHost string) bool {
	switch bindHost {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
