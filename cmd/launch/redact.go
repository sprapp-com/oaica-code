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
	"regexp"
	"strings"
)

// credentialInURL matches "scheme://userinfo@" wherever it appears — in a bare
// URL, or embedded in an error message or log line.
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)

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
func redactBaseURL(baseURL string) string {
	return redactCredentials(baseURL)
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
