package launch

// redact_test.go — the credential-in-URL redaction, pinned against the shapes
// that actually leaked: a transport error echoing the request URL, a proxy
// response body, and a bare base URL in doctor output.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const redactKey = "sk-live-leak-me-please-0123456789"

func TestRedactCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare base URL with key as userinfo",
			in:   "https://" + redactKey + "@example.test/v1",
			want: "https://REDACTED@example.test/v1",
		},
		{
			name: "user:password form",
			in:   "https://user:" + redactKey + "@example.test/v1",
			want: "https://REDACTED@example.test/v1",
		},
		{
			name: "transport error echoing the URL",
			in:   `Get "https://` + redactKey + `@example.test/v1/models": dial tcp: lookup example.test: no such host`,
			want: `Get "https://REDACTED@example.test/v1/models": dial tcp: lookup example.test: no such host`,
		},
		{
			name: "URL inside a JSON error body",
			in:   `{"error":{"message":"upstream https://` + redactKey + `@example.test/v1 refused"}}`,
			want: `{"error":{"message":"upstream https://REDACTED@example.test/v1 refused"}}`,
		},
		{
			name: "no credential at all",
			in:   "upstream HTTP 502: bad gateway",
			want: "upstream HTTP 502: bad gateway",
		},
		{
			name: "an email address in prose is not a URL",
			in:   "support: oaica@sprapp.com",
			want: "support: oaica@sprapp.com",
		},
		{
			name: "two remote URLs in one message",
			in:   "tried https://sk-aaaaaaaaaa@h1.example/v1 then https://ghu_bbbbbbbbbb@h2.example/v1",
			want: "tried https://REDACTED@h1.example/v1 then https://REDACTED@h2.example/v1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactCredentials(tc.in); got != tc.want {
				t.Fatalf("redactCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactErr keeps the wrapped cause: callers (the proxy's retry logic, the
// picker) classify errors with errors.Is, and losing the chain to make the
// text safe would turn a deadline into an unknown failure.
func TestRedactErr(t *testing.T) {
	if got := redactErr(nil); got != nil {
		t.Fatalf("redactErr(nil) = %v, want nil", got)
	}
	plain := errors.New("upstream HTTP 502")
	if got := redactErr(plain); got != plain {
		t.Fatalf("an error with no URL should pass through unchanged, got %v", got)
	}

	inner := errors.New(`Get "https://` + redactKey + `@example.test/v1/models": context deadline exceeded`)
	wrapped := redactErr(inner)
	if strings.Contains(wrapped.Error(), redactKey) {
		t.Fatalf("redactErr leaked the key: %s", wrapped.Error())
	}
	if !strings.Contains(wrapped.Error(), "https://REDACTED@example.test/v1/models") {
		t.Fatalf("redactErr should keep the URL readable: %s", wrapped.Error())
	}
	// The cause must survive so errors.Is/As still work.
	wrappedInner := redactErr(wrapWithSentinel(inner, context.DeadlineExceeded))
	if !errors.Is(wrappedInner, context.DeadlineExceeded) {
		t.Fatalf("errors.Is lost the chain: %v", wrappedInner)
	}
	if errors.Unwrap(wrapped) != inner {
		t.Fatalf("Unwrap should return the original error")
	}
}

func wrapWithSentinel(err, sentinel error) error {
	return &sentinelJoiner{msg: err.Error(), errs: []error{err, sentinel}}
}

// sentinelJoiner is a minimal multi-error with Unwrap() []error, like the
// errors.Join result an http.Client returns for TLS/context failures.
type sentinelJoiner struct {
	msg  string
	errs []error
}

func (e *sentinelJoiner) Error() string   { return e.msg }
func (e *sentinelJoiner) Unwrap() []error { return e.errs }
