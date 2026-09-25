package launch

// redact.go — keeping credentials out of text that leaves the process.
//
// Several setups configure a remote with the key inside the URL
// (https://KEY@host/v1). Go's transport turns that userinfo into a Basic auth
// header, which works — but net/http also echoes the request URL in every
// transport error, so an unsanitized error string writes the key into doctor
// output, picker warnings, proxy responses and logs. `oaica doctor --report`'s
// leak scan caught exactly that on 2026-09-26 (a failed probe printed
// `Get "https://sk-live-...@host/v1/models": dial tcp ...`).
//
// The rule here is textual on purpose: redact where the credential could
// appear rather than trusting every call site to know its URL was sensitive.
// A string-based check also catches URLs nested inside error text from other
// processes and libraries.

import (
	"net/url"
	"regexp"
	"strings"
)

// credentialInURL matches "scheme://userinfo@" wherever it appears — in a bare
// URL, or embedded in an error message or log line.
//
// The userinfo group repeats and the match is greedy up to the LAST "@" before
// the path, because that is where url.Parse also splits: in
// "http://user:x@password@host/v1" (a Basic password containing "@") the
// credential is "user:x@password". Matching only the first "@" printed the
// password's tail in the clear — on 2026-09-26 audit found it leaking through
// remote list/show, doctor and the --report scan, which did not refuse because
// a Basic userinfo is deliberately not collected as a secret (see
// splitRemoteUserinfo).
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)(?:[^\s/@]*@)+`)

// unparseableUserinfo catches a credential in a base URL that does not parse at
// all, so url.Parse cannot tell us where the userinfo ends: a copied
// passphrase with a space in it, or a bad port. It is intentionally crude —
// everything from "://" to the last "@" before the path — and is only applied
// to a value already known to be one remote's base_url, never to free prose,
// so the over-match cannot mangle an error message that merely contains a URL
// and, later, an email address.
var unparseableUserinfo = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/]*@`)

// redactCredentials replaces the userinfo of every URL in text with REDACTED.
// Text without such a URL is returned unchanged, so this is safe to apply
// unconditionally to anything on its way out.
func redactCredentials(text string) string {
	if !strings.Contains(text, "@") {
		// Fast path: no userinfo can exist without a "@".
		return text
	}
	return credentialInURL.ReplaceAllString(text, "${1}REDACTED@")
}

// redactBaseURL hides any userinfo embedded in a base URL, keeping the rest of
// the URL readable (a support report needs to say which host it failed on).
// The unparseable fallback runs only for a single URL, so it cannot reach into
// surrounding text.
func redactBaseURL(baseURL string) string {
	out := redactCredentials(baseURL)
	if strings.Contains(out, "@") && strings.Contains(out, "://") {
		out = unparseableUserinfo.ReplaceAllString(out, "${1}REDACTED@")
	}
	return out
}

// SplitUserinfoCredential is splitRemoteUserinfo for the cmd package and for
// OAICA_HOST, which is configured the same way a remote's base_url is and had
// the same leak: the key rode in the URL, so net/http printed it in transport
// errors, `ps` showed it, and every "couldn't reach %s" line carried it.
// Callers must keep the token and send it as a bearer.
func SplitUserinfoCredential(baseURL string) (cleanURL, token string) {
	return splitRemoteUserinfo(baseURL)
}

// RedactBaseURL is redactBaseURL for the cmd package, which builds the same
// user-facing strings (`oaica remote add`'s confirmation line) and cannot see
// unexported helpers. Same behavior, one implementation.
func RedactBaseURL(baseURL string) string { return redactBaseURL(baseURL) }

// RedactError is redactErr for callers outside this package.
func RedactError(err error) error { return redactErr(err) }

// userinfoSecret returns the credential a base URL carries, for a caller that
// is deciding whether some text would leak it. It answers for URLs url.Parse
// rejects too, by handing back the whole "://…@" span — a value that cannot be
// parsed cannot be transmitted either, but it can still be typed by a user who
// pasted it into a ticket, so the leak scan must recognize it.
func userinfoSecret(baseURL string) string {
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		if u.User == nil {
			return ""
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return u.User.String()
		}
		return u.User.Username()
	}
	m := unparseableUserinfo.FindStringSubmatch(baseURL)
	if m == nil {
		return ""
	}
	return strings.TrimSuffix(m[0][len(m[1]):], "@")
}

// redactedError carries sanitized text but keeps the original error as its
// cause, so errors.Is/errors.As and context-cancellation checks still work.
type redactedError struct {
	err  error
	text string
}

func (e redactedError) Error() string { return e.text }
func (e redactedError) Unwrap() error { return e.err }

// redactErr sanitizes an error whose message may embed a credential-bearing
// URL. It returns the error unchanged when there is nothing to redact, so
// callers can wrap unconditionally without allocating on the happy path.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	text := redactCredentials(err.Error())
	if text == err.Error() {
		return err
	}
	return redactedError{err: err, text: text}
}
