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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

func normalizeSystemMessages(pathname string, body []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON (or malformed) — forward as-is, let the backend reject it.
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
		existingSystem := ""
		switch s := parsed["system"].(type) {
		case string:
			existingSystem = s
		case []any:
			b, _ := json.Marshal(s)
			existingSystem = string(b)
		}
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

func contentToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
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
func RunNormalizingProxyOn(bindHost string, listenPort, backendPort int, apiKey string) error {
	backend := fmt.Sprintf("http://127.0.0.1:%d", backendPort)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/chat/completions") && len(body) > 0 {
			if fixed, err := normalizeSystemMessages(r.URL.Path, body); err == nil {
				body = fixed
			}
		}

		req, err := http.NewRequest(r.Method, backend+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.ContentLength = int64(len(body))

		resp, err := http.DefaultClient.Do(req)
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
		io.Copy(w, resp.Body)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindHost, listenPort))
	if err != nil {
		return err
	}
	return http.Serve(ln, handler)
}

// isLoopbackBind reports whether bindHost is a loopback-only bind.
func isLoopbackBind(bindHost string) bool {
	switch bindHost {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
