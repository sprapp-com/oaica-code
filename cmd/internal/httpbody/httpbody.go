// Package httpbody reads HTTP bodies with a bound.
//
// Every response body oaica buffers is written by somebody else: a router, a
// vendor's API, a model server on the network. io.ReadAll on one of those
// allocates whatever the sender chose to send, so a wrong Content-Length, a
// hostile endpoint, or simply a very large artifact turns a routine request
// into the process's peak memory — and on a machine with a cgroup limit, into
// a kill (2026-09-26 audit). ReadCapped is the one place that bound is
// applied, so a reader cannot forget it and two call sites cannot disagree
// about what "too big" means.
package httpbody

import (
	"fmt"
	"io"
)

// DefaultMax is the cap for a body oaica buffers in full and then parses: a
// chat completion, a model list, a manifest. Generous on purpose — this is a
// bound against the unbounded, not a policy about payload size.
const DefaultMax int64 = 64 << 20

// DiagnosticMax is the cap for a body read only to be quoted in an error
// message (an upstream's error text). Error bodies are not artifacts, and one
// that quotes a whole response is worse than useless.
const DiagnosticMax int64 = 64 << 10

// ReadCapped reads r up to max bytes. A body of exactly max is fine; a body
// with more is an error and is NOT returned — the caller asked for a value it
// can parse, and half of an oversized body is not one.
//
// what names the body in the error ("the model list from api.example.com"), so
// the message says which read hit the bound rather than leaving the user with
// a bare number.
func ReadCapped(r io.Reader, max int64, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than the %d bytes oaica will read into memory — refusing to buffer it (raise the limit only if this endpoint is trusted; a body that large is not something this command was going to use)", what, max)
	}
	return b, nil
}

// ReadCappedOrEmpty is ReadCapped for a body read only to be quoted back: it
// returns what it could read and swallows the error, because a diagnostic must
// never replace the failure it is describing. An over-limit body comes back
// TRUNCATED at the cap with a marker, never as the whole thing.
func ReadCappedOrEmpty(r io.Reader, max int64, what string) []byte {
	b, err := io.ReadAll(io.LimitReader(r, max))
	if err != nil {
		return b
	}
	if int64(len(b)) == max {
		if extra, _ := io.ReadAll(io.LimitReader(r, 1)); len(extra) > 0 {
			return append(b, []byte(fmt.Sprintf("\n…(%s truncated at %d bytes)", what, max))...)
		}
	}
	return b
}
