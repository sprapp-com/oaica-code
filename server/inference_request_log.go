package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/envconfig"
)

type inferenceRequestLogger struct {
	dir     string
	counter uint64
}

func newInferenceRequestLogger() (*inferenceRequestLogger, error) {
	dir, err := os.MkdirTemp("", "ollama-request-logs-*")
	if err != nil {
		return nil, err
	}

	return &inferenceRequestLogger{dir: dir}, nil
}

func (s *Server) initRequestLogging() error {
	if !envconfig.DebugLogRequests() {
		return nil
	}

	requestLogger, err := newInferenceRequestLogger()
	if err != nil {
		return fmt.Errorf("enable OLLAMA_DEBUG_LOG_REQUESTS: %w", err)
	}

	s.requestLogger = requestLogger
	slog.Info(fmt.Sprintf("request debug logging enabled; inference request logs will be stored in %s and include request bodies and replay curl commands", requestLogger.dir))

	return nil
}

func (s *Server) withInferenceRequestLogging(route string, handlers ...gin.HandlerFunc) []gin.HandlerFunc {
	if s.requestLogger == nil {
		return handlers
	}

	return append([]gin.HandlerFunc{s.requestLogger.middleware(route)}, handlers...)
}

func (l *inferenceRequestLogger) middleware(route string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		method := c.Request.Method
		host := c.Request.Host
		scheme := "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
		contentType := c.GetHeader("Content-Type")

		var body []byte
		if c.Request.Body != nil {
			var err error
			body, err = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			if err != nil {
				slog.Warn("failed to read request body for debug logging", "route", route, "error", err)
			}
		}

		c.Next()
		l.log(route, method, scheme, host, contentType, body)
	}
}

func (l *inferenceRequestLogger) log(route, method, scheme, host, contentType string, body []byte) {
	if l == nil || l.dir == "" {
		return
	}

	if contentType == "" {
		contentType = "application/json"
	}
	if host == "" || scheme == "" {
		base := envconfig.Host()
		if host == "" {
			host = base.Host
		}
		if scheme == "" {
			scheme = base.Scheme
		}
	}

	routeForFilename := sanitizeRouteForFilename(route)
	timestamp := fmt.Sprintf("%s-%06d", time.Now().UTC().Format("20060102T150405.000000000Z"), atomic.AddUint64(&l.counter, 1))
	bodyFilename := fmt.Sprintf("%s_%s_body.json", timestamp, routeForFilename)
	curlFilename := fmt.Sprintf("%s_%s_request.sh", timestamp, routeForFilename)
	bodyPath := filepath.Join(l.dir, bodyFilename)
	curlPath := filepath.Join(l.dir, curlFilename)

	if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
		slog.Warn("failed to write debug request body", "route", route, "error", err)
		return
	}

	url := fmt.Sprintf("%s://%s%s", scheme, host, route)
	// Every field the CLIENT chose (the method, the Host it named, its Content-Type)
	// is single-quoted, not %q: %q escapes a quote and a backslash and leaves
	// `$(...)` and a backtick live inside the double quotes of a /bin/sh script, and
	// the method, a valid HTTP token, may itself contain a backtick or `$`. Replaying
	// the script ran whatever a client that could reach the port had put there
	// (2026-09-29 audit, round 109, F109-L1-2).
	curl := fmt.Sprintf("#!/bin/sh\nSCRIPT_DIR=\"$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\"\ncurl --request %s --url %s --header %s --data-binary @\"${SCRIPT_DIR}/%s\"\n",
		shellSingleQuote(method), shellSingleQuote(url), shellSingleQuote("Content-Type: "+contentType), bodyFilename)
	if err := os.WriteFile(curlPath, []byte(curl), 0o600); err != nil {
		slog.Warn("failed to write debug request replay command", "route", route, "error", err)
		return
	}

	slog.Info(fmt.Sprintf("logged to %s, replay using curl with `sh %s`", bodyPath, curlPath))
}

func sanitizeRouteForFilename(route string) string {
	route = strings.TrimPrefix(route, "/")
	if route == "" {
		return "root"
	}

	var b strings.Builder
	b.Grow(len(route))
	for _, r := range route {
		if ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	return b.String()
}

// shellSingleQuote quotes s for /bin/sh: nothing inside single quotes is expanded,
// and an embedded single quote is written as close-quote, backslash-quote,
// open-quote.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
