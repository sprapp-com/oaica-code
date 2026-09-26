package launch

// request_log.go — local request logging for `oaica launch`, so we have
// real labeled data to evaluate/improve the router's flashplan classifier
// (cmd/oaica.go's classifyFlashplan is a hand-tuned regex+length heuristic
// that has never been measured against real traffic — see the wins doc/
// conversation this was scoped from). Logged LOCALLY to
// ~/.oaica/requests.log, NOT to any server — no KV, no D1, no per-request
// cost, and the data never leaves the user's machine unless they choose to
// share it. This deliberately mirrors classifyFlashplan's own signals
// client-side so the log records "would flashplan have called this hard or
// easy" without needing the router to report back its decision.

import (
	"bytes"
	"encoding/json"
	"github.com/ollama/ollama/cmd/internal/httpbody"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Mirrors prism-api-router/src/index.ts's HARD_SIGNAL_RE /
// HARD_LENGTH_THRESHOLD exactly — keep these two in sync if the router's
// heuristic changes, that's the whole point of logging this signal.
var requestLogHardSignalRE = regexp.MustCompile(`(?i)\b(prove|proof|algorithm|complexity|architecture|design a|debug|root cause|why (does|is|isn't)|trade[- ]?off|step[- ]?by[- ]?step|multi[- ]?step|refactor|optimi[sz]e|derive|reason(ing)?|analy[sz]e|compare and contrast|edge case)\b`)

const requestLogHardLengthThreshold = 600

type requestLogEntry struct {
	Timestamp        string `json:"ts"`
	Model            string `json:"model"`
	Path             string `json:"path"`
	Backend          string `json:"backend"` // where this request was actually forwarded (cloud router or local server)
	LastMessageLen   int    `json:"last_message_len"`
	TotalMessagesLen int    `json:"total_messages_len"`
	HardSignalMatch  bool   `json:"hard_signal_match"`    // mirrors classifyFlashplan's regex check
	WouldBeHardByLen bool   `json:"would_be_hard_by_len"` // mirrors classifyFlashplan's length check
	StatusCode       int    `json:"status_code"`
	DurationMs       int64  `json:"duration_ms"`
	// ProxyPort is the local listen port of the launch proxy that handled
	// this request. Each `oaica launch` session runs its own proxy on an
	// auto-assigned port, so this is what makes rows from concurrent
	// sessions on the same machine attributable (a bare ~/.oaica/
	// requests.log otherwise interleaves every session's rows with no way
	// to tell them apart).
	ProxyPort int `json:"proxy_port,omitempty"`
}

// requestLogProxyPort is the port of the (single, per-process) launch
// proxy; set once at listener bind time, read by appendRequestLog.
var requestLogProxyPort int

// setRequestLogProxyPort records ln's port for requestLogProxyPort
// attribution. Best-effort: a non-TCP listener just leaves the port at 0
// (omitted from the JSON).
func setRequestLogProxyPort(ln net.Listener) {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		requestLogProxyPort = addr.Port
	}
}

// RequestLogPath is requestLogPath, exported for `oaica usage` (cmd.go) to
// print in its "no traffic logged yet" message.
func RequestLogPath() (string, error) { return requestLogPath() }

func requestLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "requests.log"), nil
}

func appendRequestLog(entry requestLogEntry) {
	entry.ProxyPort = requestLogProxyPort
	path, err := requestLogPath()
	if err != nil {
		return // best-effort — never break a real request over a logging failure
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// ONE write for row + newline. Two Writes (f.Write(b) then f.Write("\n"))
	// are not atomic with respect to other processes: concurrent sessions
	// interleave their bytes and a reader sees a line that splices two rows
	// ("…}{…"), which `oaica usage` cannot parse — it silently dropped those
	// rows and still exited 0 (2026-09-26 audit, third round). O_APPEND makes
	// the single Write land at the end atomically.
	f.Write(append(b, '\n'))
}

// requestLogModelFromBody reads the model id a completion request names. Used
// by the passthrough leg, which forwards the client's body verbatim and so has
// no translated request struct to read it from; an unparseable or absent field
// logs an empty model rather than skipping the row — the row's job is to
// record that the attempt happened and failed (2026-09-26 audit).
func requestLogModelFromBody(body []byte) string {
	var parsed struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Model
}

// extractLastAndTotalMessageLen pulls the same two signals
// classifyFlashplan uses server-side, from either shape (OpenAI
// messages[] or Anthropic top-level system + messages[]) — best-effort,
// never errors, a shape it doesn't recognize just logs zero lengths
// rather than failing the request.
func extractLastAndTotalMessageLen(body []byte) (lastLen, totalLen int) {
	var parsed struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Messages) == 0 {
		return 0, 0
	}
	contentLen := func(raw json.RawMessage) int {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return len(s)
		}
		return len(raw) // non-string content (blocks) — approximate with raw JSON length
	}
	for _, m := range parsed.Messages {
		totalLen += contentLen(m.Content)
	}
	lastLen = contentLen(parsed.Messages[len(parsed.Messages)-1].Content)
	return lastLen, totalLen
}

// RunLocalLoggingProxy serves on an already-bound listener (see
// ListenLocalLoggingProxy — binding synchronously before the caller
// proceeds avoids a race where the client connects before this is ready)
// and forwards every request unchanged to targetBaseURL, logging
// model/message-size/hard-signal features (NOT full message content — see
// the doc comment above) for every /v1/messages or /v1/chat/completions
// POST. Used by claude.go so `oaica launch claude` always routes through
// this, whether the real destination is the cloud router or a local
// `oaica serve` instance.
func RunLocalLoggingProxy(ln net.Listener, targetBaseURL string) error {
	setRequestLogProxyPort(ln)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := httpbody.ReadCapped(r.Body, httpbody.DefaultMax, "the request body")
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}

		// The caller's context, not a detached one: when Claude Code gives up on
		// a request (or the user Ctrl-Cs) this handler has to give up on the
		// upstream with it, or both sockets stay open until the process exits.
		// proxyUpstreamClient bounds connection setup only — never the response,
		// because a local model may legitimately stream for longer than any
		// fixed timeout.
		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetBaseURL+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
		if err != nil {
			// redactErr: a target that carries the key as URL userinfo (an
			// OAICA_HOST like https://sk-...@api.oaica.com, the same shape
			// remotes.json leaked) appears in net/http's own error text —
			// and this response goes back to the caller.
			http.Error(w, redactErr(err).Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.ContentLength = int64(len(body))

		// The row is built BEFORE the upstream call and written from a defer,
		// like the Anthropic proxy's rows: it used to be created behind the
		// request, so a transport failure (refused connection, DNS, TLS,
		// timeout) returned early and left no evidence at all — `oaica usage`
		// reported ERR 0 for a session whose every turn failed (2026-09-26
		// audit).
		var entry *requestLogEntry
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/chat/completions") && len(body) > 0 {
			var modelField struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &modelField)
			lastLen, totalLen := extractLastAndTotalMessageLen(body)
			e := requestLogEntry{
				Timestamp:        time.Now().UTC().Format(time.RFC3339),
				Model:            modelField.Model,
				Path:             r.URL.Path,
				Backend:          redactBaseURL(targetBaseURL),
				LastMessageLen:   lastLen,
				TotalMessagesLen: totalLen,
				HardSignalMatch:  requestLogHardSignalRE.MatchString(string(body)),
				WouldBeHardByLen: lastLen > requestLogHardLengthThreshold || totalLen > requestLogHardLengthThreshold*3,
			}
			entry = &e
			defer func() {
				entry.DurationMs = time.Since(start).Milliseconds()
				appendRequestLog(*entry)
			}()
		}

		resp, err := proxyUpstreamClient.Do(req)
		if err != nil {
			if entry != nil {
				entry.StatusCode = http.StatusBadGateway
			}
			// A transport error quotes the request URL verbatim.
			http.Error(w, redactErr(err).Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		rec := &statusCapturingWriter{ResponseWriter: w}
		rec.WriteHeader(resp.StatusCode)
		flushResponse(rec)
		relayedBody, clientGone, relayErr := relayFlushing(rec, resp.Body)

		// The status logged is the one the CLIENT ended up with, and a sub-300
		// status is not by itself a delivered turn: a body that died mid-answer
		// after the headers were already a 200 was logged as a clean turn, and
		// so was an upstream that answered 200 and closed without a byte —
		// neither is a turn any client can use, and a session of nothing but
		// those reported ERR 0 (2026-09-26 audit). A client that hung up is the
		// one case that is NOT a failed turn: the leg delivered, this side had
		// nowhere to put it.
		if entry != nil {
			entry.StatusCode = rec.status()
			if entry.StatusCode < 300 && !clientGone && r.Context().Err() == nil &&
				(relayErr != nil || relayedBody == 0) {
				entry.StatusCode = http.StatusBadGateway
			}
		}
	})

	return http.Serve(ln, handler)
}

// ListenLocalLoggingProxy binds a local listener on an auto-assigned port
// SYNCHRONOUSLY, returning it (with its port) for the caller to pass to
// RunLocalLoggingProxy in a goroutine — split from that function so the
// caller can be certain the port is bound and ready before proceeding
// (e.g. before setting ANTHROPIC_BASE_URL to it and launching a client
// that will immediately try to connect).
func ListenLocalLoggingProxy() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}
