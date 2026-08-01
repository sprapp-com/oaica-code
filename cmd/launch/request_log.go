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
	"io"
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
	HardSignalMatch  bool   `json:"hard_signal_match"`  // mirrors classifyFlashplan's regex check
	WouldBeHardByLen bool   `json:"would_be_hard_by_len"` // mirrors classifyFlashplan's length check
	StatusCode       int    `json:"status_code"`
	DurationMs       int64  `json:"duration_ms"`
}

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
	f.Write(b)
	f.Write([]byte("\n"))
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		req, err := http.NewRequest(r.Method, targetBaseURL+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
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

		if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/chat/completions") && len(body) > 0 {
			var modelField struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &modelField)
			lastLen, totalLen := extractLastAndTotalMessageLen(body)
			appendRequestLog(requestLogEntry{
				Timestamp:        time.Now().UTC().Format(time.RFC3339),
				Model:            modelField.Model,
				Path:             r.URL.Path,
				Backend:          targetBaseURL,
				LastMessageLen:   lastLen,
				TotalMessagesLen: totalLen,
				HardSignalMatch:  requestLogHardSignalRE.MatchString(string(body)),
				WouldBeHardByLen: lastLen > requestLogHardLengthThreshold || totalLen > requestLogHardLengthThreshold*3,
				StatusCode:       resp.StatusCode,
				DurationMs:       time.Since(start).Milliseconds(),
			})
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
