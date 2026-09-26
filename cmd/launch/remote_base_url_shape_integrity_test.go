package launch

// remote_base_url_shape_integrity_test.go — the --base-url guard accepted two
// shapes that cannot work, and both were stored, listed as "added", and failed
// only later at request time (2026-09-26 audit).
//
//   - A query or fragment swallows the path oaica appends. The base is joined
//     with "/v1/chat/completions", so `https://host/v1?k=1` becomes
//     `https://host/v1?k=1/v1/chat/completions` — the query truncates it and
//     every request goes to `/v1` with a nonsense query. `oaica doctor` prints
//     the mangled URL and a lookup failure that names neither the flag nor the
//     real cause.
//   - A port outside 1-65535 parses fine and fails at request time with
//     "address 99999999: invalid port".

import (
	"strings"
	"testing"
)

func TestBaseURLWithAQueryOrFragmentIsRefused(t *testing.T) {
	for _, bad := range []string{
		"https://myhost.example.com/v1?k=1",
		"https://myhost.example.com/v1#frag",
		"https://myhost.example.com?k=1",
	} {
		if err := validateRemoteBaseURL(bad); err == nil {
			t.Errorf("validateRemoteBaseURL(%q) accepted a value whose query or fragment truncates the path oaica appends — the remote is stored, listed as added, and then every request goes to the wrong URL with a diagnostic that names neither this flag nor the cause", bad)
		} else if !strings.Contains(err.Error(), "--base-url") {
			t.Errorf("validateRemoteBaseURL(%q) refused with %q, which does not name the flag the user typed", bad, err)
		}
	}
}

func TestBaseURLWithAnImpossiblePortIsRefused(t *testing.T) {
	for _, bad := range []string{
		"https://myhost.example.com:99999999/v1",
		"https://myhost.example.com:0/v1",
	} {
		if err := validateRemoteBaseURL(bad); err == nil {
			t.Errorf("validateRemoteBaseURL(%q) accepted a port outside 1-65535, which fails only at request time with \"invalid port\"", bad)
		}
	}
}

// The controls: everything a working remote uses is still accepted, including
// the credential-in-userinfo form this fleet's own mirrors use.
func TestOrdinaryBaseURLsAreStillAccepted(t *testing.T) {
	for _, ok := range []string{
		"https://api.example.com",
		"https://api.example.com/v1",
		"HTTPS://api.example.com/v1",
		"https://sk-live-KEY@mirror.example.com/v1",
		"http://127.0.0.1:8080/v1",
		"https://api.example.com:8443/v1",
	} {
		if err := validateRemoteBaseURL(ok); err != nil {
			t.Errorf("validateRemoteBaseURL(%q) = %v, want it accepted", ok, err)
		}
	}
}
