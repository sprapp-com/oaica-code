package launch

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// credentialSafeRedirect is the CheckRedirect every outbound client that carries
// a credential uses. Go strips Authorization, Cookie and WWW-Authenticate when a
// redirect changes host, and nothing else: the x-api-key an Anthropic-wire plan
// row authenticates with, an api-key header, and the X-Session-Id the proxy adds
// all followed a 307 to whatever host the upstream named, so a mirror or plan-row
// upstream that was misconfigured, hijacked or malicious harvested the operator's
// key, the request body and the referring URL (2026-09-29 audit, round 109,
// F109-L2-1). The redirect is still followed — a vendor that moves an endpoint
// keeps working — but it arrives without a credential, so it fails closed at the
// new host instead of handing it over.
func credentialSafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && originOf(req.URL) != originOf(via[0].URL) {
		for _, h := range []string{"X-Api-Key", "Api-Key", "Authorization", "Proxy-Authorization", "X-Session-Id", "Cookie"} {
			req.Header.Del(h)
		}
		req.Header.Del("Referer")
	}
	return nil
}

// originOf is the origin a credential may be sent to: the scheme, the lowercased
// hostname and the EFFECTIVE port (443 for https and 80 for http when none is spelled).
// Comparing the Host string alone kept the credential on an https to http redirect of
// the same host, and treated "h" and "h:443" as different places; comparing the
// hostname alone treated another port of the same host as the same place (2026-09-29
// audit, round 111, F111-L2-3 and F111-L2-4).
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// credentialSafeDefaultClient is http.DefaultClient with the redirect policy, read
// at call time so a test that swaps http.DefaultClient (the seam the probes are
// tested through) still reaches its transport. The probes it serves carry a
// context and no timeout of their own, as http.DefaultClient does.
func credentialSafeDefaultClient() *http.Client {
	c := *http.DefaultClient
	c.CheckRedirect = credentialSafeRedirect
	return &c
}
