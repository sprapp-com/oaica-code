package launch

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// rebindingGuard refuses, on a loopback listener, a request whose Host header
// names a foreign DOMAIN. The local server does this (allowedHostsMiddleware); the
// two loopback proxies in front of the same model did not, so a web page the
// operator visits could rebind its own domain to 127.0.0.1 and read completions
// from the local model, and spend its compute, with no credential, on a
// well-known port (2026-09-29 audit, round 110, F110-L2-1). The accepted set is
// the server's: an IP literal (a rebinding page cannot make a browser send one),
// localhost, this machine's hostname, and the local TLDs .localhost, .local and
// .internal. A listener that is not on loopback is not guarded here — it is
// network-facing on purpose and is behind its bearer check.
func rebindingGuard(loopback bool, next http.Handler) http.Handler {
	if !loopback {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localHostHeader(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackListener reports whether ln is bound to a loopback address.
func isLoopbackListener(ln net.Listener) bool {
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	return err == nil && ap.Addr().IsLoopback()
}

func localHostHeader(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if name, err := os.Hostname(); err == nil && host == strings.ToLower(name) {
		return true
	}
	for _, tld := range []string{"localhost", "local", "internal"} {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}
	return false
}
