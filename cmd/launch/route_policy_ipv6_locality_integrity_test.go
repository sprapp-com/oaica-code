package launch

// route_policy_ipv6_locality_integrity_test.go — hostOf cut the host at the
// FIRST colon, which is the port separator only for a DNS name or an IPv4
// literal (2026-09-26 audit). A bracketless-of-its-colons IPv6 URL —
// `http://[::1]:11434/v1`, the form a local daemon bound to IPv6 loopback is
// addressed by — yielded "[" and routeLocality answered "remote".
//
// That is not cosmetic: under local-first/auto the local leg is deprioritized,
// and under local-only it is refused by name as if it were a remote backend,
// so a launch against the machine's own daemon failed with "policy
// local-only forbids remote". TestLocalhostLocality only ever listed
// 127.0.0.1/localhost/an IPv4-port case, so nothing caught it.

import (
	"testing"
)

func TestALoopbackBaseURLIsLocalOverIPv6Too(t *testing.T) {
	for _, url := range []string{
		"http://[::1]:11434/v1",
		"http://[::1]/v1",
		"http://[0:0:0:0:0:0:0:1]:11434/v1",
		"http://[::1]:11434",
	} {
		if got := hostOf(url); got == "[" || got == "" {
			t.Errorf("hostOf(%q) = %q — the host is not the text before the first colon when the address itself is made of colons", url, got)
		}
		if got := routeLocality(url); got != "local" {
			t.Errorf("routeLocality(%q) = %q, want local — an IPv6 loopback daemon addressed as [::1] is this machine, and a local-only launch must not refuse it as a remote", url, got)
		}
	}
}

// The control: an IPv6 host that is NOT loopback is still remote, so this
// cannot be passed by calling every bracketed address local.
func TestANonLoopbackIPv6BaseURLStaysRemote(t *testing.T) {
	for _, url := range []string{
		"http://[2606:4700::1111]:8080/v1",
		"https://[2001:db8::1]/v1",
	} {
		if got := routeLocality(url); got != "remote" {
			t.Errorf("routeLocality(%q) = %q, want remote", url, got)
		}
	}
}

// And the forms hostOf already handled keep working, including the userinfo
// strip that the credential-bearing remote URLs use.
func TestHostOfKeepsItsOldAnswers(t *testing.T) {
	for url, want := range map[string]string{
		"http://127.0.0.1:11434/v1":            "127.0.0.1",
		"http://localhost:8081/v1":             "localhost",
		"https://api.deepseek.com/v1":          "api.deepseek.com",
		"https://key:secret@host.example/v1":   "host.example",
		"http://box:8080/v1":                   "box",
		"https://api.example.com:443?a=1#frag": "api.example.com",
	} {
		if got := hostOf(url); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", url, got, want)
		}
	}
}
