package launch

import "net/http"

// CredentialSafeRedirect is credentialSafeRedirect for the callers outside this package: a
// redirect that leaves the request's origin (scheme, host, effective port) drops the credential
// headers. net/http's own rule compares hostnames only, so the router key followed a 307 to
// another port or down to http on the same host (2026-09-29 audit, round 119, F119-L2-1).
func CredentialSafeRedirect(req *http.Request, via []*http.Request) error {
	return credentialSafeRedirect(req, via)
}
