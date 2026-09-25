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
	"io"
	"net/http"
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
var unparseableUserinfo = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*://).*@`)

// quotedURLUserinfo is unparseableUserinfo for a URL quoted inside error text,
// where the value cannot be anchored to the start of the string. net/http
// quotes the request URL in both of its messages ("Get \"URL\": dial …",
// "parse \"URL\": invalid port …"), so the closing quote bounds the match — no
// spanning into surrounding prose, which is what keeps a bare
// `[^\s]*@` from mangling an error that happens to mention a URL and later an
// email address.
//
// It exists because a Basic password may CONTAIN a slash: url.Parse then
// rejects the URL ("invalid port \":aBc9\" after host", the authority ending at
// the first "/"), so url.Parse cannot tell us where the userinfo ends, and the
// "://…@" rules that stop at "/" miss the credential entirely — the report
// printed the password in the clear and its leak scan had never been told the
// value (2026-09-26 audit).
var quotedURLUserinfo = regexp.MustCompile(`"([a-zA-Z][a-zA-Z0-9+.\-]*://)[^"\n]*@`)

// invalidPortFragment matches the second half of the same parse failure: Go
// cuts the authority at the "/" inside the credential and then reports that
// span back verbatim as a bad port —
//
//	parse "https://user:aBc9/xY7z@host/v1": invalid port ":aBc9" after host
//
// so redacting the quoted URL still leaves the password's head sitting outside
// the quotes in the same message (2026-09-26 audit: doctor --report printed
// ":aBc9" for a password "aBc9/xY7z…").
//
// Only a fragment that does NOT begin with a digit is touched. A real bad port
// (":99999", ":80x" is not a port either but is a typo a user needs to read) is
// diagnostic; a fragment starting with anything else is credential text the
// parser mistook for a port.
var invalidPortFragment = regexp.MustCompile(`invalid port ":[^0-9][^"]*"`)

// credentialQueryParam matches a query-parameter name that carries a credential
// in the URL itself. It is the second place a key can ride in a base URL, and
// it leaked the same way userinfo did: the transport echoes the request URL in
// its errors, `ps` shows the argv, and `oaica doctor --report` prints the URL
// it failed on — so the value reaches the same four places, and the pre-print
// leak scan did not know to look for it.
//
// Anchored (^…$) and matched case-insensitively against the DECODED parameter
// name, so `?api_key=`, `?API-KEY=`, `?token=` and `?access_token=` are covered
// without a substring match turning "?monkey=" into a credential.
var credentialQueryParam = regexp.MustCompile(`(?i)^(?:api[_-]?key|key|token|access[_-]?token|auth[_-]?token|apikey|secret|password)$`)

// queryCredentialValue finds one credential-looking query parameter anywhere in
// text and captures its value, so both the redactor and the leak scan can agree
// on what a URL carries. The "[?&]" prefix is deliberate: a bare "?key=" inside
// prose is matched only when something before it looks like a URL… which is not
// something a regexp can decide, so callers pass single URLs.
var queryCredentialValue = regexp.MustCompile(`(?i)[?&]([^=&#\s]+)=([^&#\s]*)`)

// querySecrets returns every credential value text's query strings carry.
// Ordered left to right for stable error text and stable test assertions.
func querySecrets(text string) []string {
	var out []string
	for _, m := range queryCredentialValue.FindAllStringSubmatch(text, -1) {
		name, err := url.QueryUnescape(m[1])
		if err != nil {
			name = m[1]
		}
		if !credentialQueryParam.MatchString(name) {
			continue
		}
		value, err := url.QueryUnescape(m[2])
		if err != nil {
			value = m[2]
		}
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// redactQueryCredentials replaces the value of every credential-looking query
// parameter with REDACTED, keeping the parameter NAME so a support report still
// says which shape failed ("/v1?api_key=REDACTED" reads as "the key rode in the
// query", which is the diagnostic the line exists for).
func redactQueryCredentials(text string) string {
	return queryCredentialValue.ReplaceAllStringFunc(text, func(m string) string {
		sub := queryCredentialValue.FindStringSubmatch(m)
		name, err := url.QueryUnescape(sub[1])
		if err != nil {
			name = sub[1]
		}
		if !credentialQueryParam.MatchString(name) || strings.TrimSpace(sub[2]) == "" {
			return m
		}
		return m[:len(m)-len(sub[2])] + "REDACTED"
	})
}

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
	return redactQueryCredentials(out)
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

// NewRedactedRequest is http.NewRequest with its error run through redactErr.
//
// net/http quotes the URL back in its own errors two ways, and both reach
// stderr: a transport failure ("Post \"https://user:pass@host/v1\": dial tcp
// ...") and a parse failure ("parse \"https://sk-live-…@ho st/v1\": invalid
// character \" \" in host name"). The first is handled where the Do error is
// wrapped; the second is not, because it is returned before any request
// exists. Callers that build a URL from a user-supplied base URL should use
// this instead of http.NewRequest (2026-09-26 audit).
func NewRedactedRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, redactErr(err)
	}
	return req, nil
}

// baseURLSecrets returns every credential value a base URL could print, in the
// forms it could appear in: the userinfo as a whole ("user:password"), the
// password alone (a parse error quotes the URL, but a caller that prints only
// the credential would show just this), and every credential-looking query
// value. The leak scan is only as good as this list — a value missing from it
// is one the report can print while still claiming it holds no credentials.
func baseURLSecrets(baseURL string) []string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil
	}
	var out []string
	if ui := userinfoSecret(baseURL); ui != "" {
		out = append(out, ui)
		if _, password, ok := strings.Cut(ui, ":"); ok && strings.TrimSpace(password) != "" {
			out = append(out, password)
			// url.Parse cuts the authority at the first "/" *inside* the
			// password, so an error can repeat just that head as a "port"
			// (see invalidPortFragment). A value the scan does not know is a
			// value a print site can add without anything noticing.
			if i := strings.Index(password, "/"); i > 0 {
				out = append(out, password[:i])
			}
		}
	}
	return append(out, querySecrets(baseURL)...)
}

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
	// The quoted-URL rule catches a userinfo url.Parse cannot parse (a Basic
	// password with a "/" in it), which the "://…@" rules cannot see past.
	if strings.Contains(text, "@") {
		text = quotedURLUserinfo.ReplaceAllString(text, `"${1}REDACTED@`)
	}
	// A base URL can also carry its key in the query string, and net/http echoes
	// the whole request URL in a transport error ("Get \"https://host/v1?api_key=
	// …\": dial tcp …"), so redacting userinfo is not enough — doctor --report
	// printed this value on the line below one redactBaseURL had already cleaned
	// (2026-09-26 audit).
	text = redactQueryCredentials(text)
	// The parse-error path re-states the credential fragment outside the URL's
	// quotes, so it gets its own rule rather than relying on the quoted one.
	text = invalidPortFragment.ReplaceAllString(text, `invalid port "REDACTED"`)
	if text == err.Error() {
		return err
	}
	return redactedError{err: err, text: text}
}
