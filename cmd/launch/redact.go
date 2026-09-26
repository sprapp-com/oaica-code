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
	"unicode"
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
// passphrase with a space in it, a bad port, or a key pasted across a line
// break. It is intentionally crude — everything from "://" to the last "@"
// before the path — and is only applied to a value already known to be one
// remote's base_url, never to free prose, so the over-match cannot mangle an
// error message that merely contains a URL and, later, an email address.
//
// [\s\S] and not "." because a newline inside the credential is a shape that
// reached the user in the clear: url.Parse refuses the value (so the parse
// branch of userinfoSecret returns nothing and the leak scan had no value to
// refuse on), and the un-escaped stored config value is what `oaica remote
// show`, `oaica remote list` and `oaica doctor --report` print — a key pasted
// with a line break in it passed every rule (2026-09-26 audit).
var unparseableUserinfo = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*://)[\s\S]*@`)

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
var quotedURLUserinfo = regexp.MustCompile(`"([a-zA-Z][a-zA-Z0-9+.\-]*://)[^"]*@`)

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
// The predicate is now a WORD test rather than an anchored list of full names.
// The list was the defect: it named the spellings its author thought of
// (`api_key`, `key`, `token`, `auth`, …), so a gateway that takes
// `?client_secret=` — OAuth 2.0's own parameter name — or `?api_token=`,
// `?X-Api-Key=`, `?sig=` was neither hidden at any print site NOR known to the
// pre-print leak scan, which shares this predicate: `oaica doctor --report`
// printed the key and then printed its own footer promising the report holds no
// credential values (2026-09-26 audit, fourth round). A name list can never
// catch the spelling nobody thought of; a word test catches the class.
//
// The name is split on separators and camelCase ("X-Api-Key" → x, api, key;
// "clientSecret" → client, secret) and every word is matched against the
// credential vocabulary, plus a suffix test for compounds written without a
// separator ("apikey", "authtoken", "clientsecret"). The suffix test
// deliberately over-redacts names that merely end in one of those words
// ("monkey", "hockey"): a parameter value hidden in a printed URL costs a
// diagnostic line, while a missed credential is a key in a pasted ticket.
func looksLikeCredentialParam(raw string) bool {
	name, err := url.QueryUnescape(raw)
	if err != nil {
		name = raw
	}
	words := splitParamWords(name)
	for _, w := range words {
		if credentialParamWords[strings.ToLower(w)] {
			return true
		}
	}
	joined := strings.ToLower(strings.Join(words, ""))
	// One trailing qualifier names a FIELD ON a credential, not a credential:
	// "key id", "password hash", "token value", "secret name". Stripping it
	// before the core test is what makes "keyid", "passwordhash" and
	// "secretvalue" reduce to the word they are built from.
	for _, q := range credentialParamQualifiers {
		if len(joined) > len(q) && strings.HasSuffix(joined, q) {
			joined = strings.TrimSuffix(joined, q)
			break
		}
	}
	for _, core := range credentialParamCores {
		// Suffix as well as prefix: the qualifier may sit on either side
		// ("keyid" and "apikeyid" strip to "key"/"apikey"; "secretkey",
		// "clientcreds" and "accesstoken" carry the core at the end).
		if strings.HasSuffix(joined, core) || strings.HasPrefix(joined, core) {
			return true
		}
	}
	return false
}

// credentialParamWords is the vocabulary a single word of a parameter name is
// compared against, matched exactly after lowercasing.
var credentialParamWords = map[string]bool{
	"key": true, "keys": true, "token": true, "tokens": true,
	"secret": true, "secrets": true, "password": true, "passwd": true, "pwd": true,
	"auth": true, "authorization": true, "bearer": true,
	"credential": true, "credentials": true, "creds": true,
	"sig": true, "signature": true, "session": true, "cookie": true,
	"license": true, "licence": true, "sas": true, "jwt": true,
	"pass": true, "phrase": true, "passphrase": true,
}

// credentialParamCores is the credential vocabulary as STEMS, for a parameter
// name written with no separator at all — the case credentialParamWords cannot
// cover because there is no word boundary to match against. A stem is tested at
// either end of the joined name ("secretkey", "clientcreds", "accesstoken" end
// with one; "publickey" is the same word from the other side).
//
// This list replaced a fixed set of whole compounds ("apikey", "authkey", …)
// that only recognised the spellings someone had happened to write down.
// `?secretkey=sk-live-…` is the word "secret" glued to the word "key", exactly
// like the "apikey" the fixed list knew — and it was printed verbatim by the
// redactor and skipped by the leak scan, so the doctor reported a clean
// deployment while the key travelled in the clear (2026-09-26 audit, seventh
// round).
var credentialParamCores = []string{
	"key", "keys", "token", "tokens", "secret", "secrets",
	"password", "passwd", "pwd", "pass", "phrase", "passphrase",
	"auth", "authkey", "bearer", "cred", "creds",
	"credential", "credentials", "sig", "signature",
	"session", "cookie", "license", "licence", "sas", "jwt",
}

// credentialParamQualifiers name a FIELD ON a credential rather than a
// credential. One is stripped from the end of the joined name before the core
// test, so "keyid", "apikeyid", "passwordhash" and "secretvalue" all reduce to
// the credential word they are built from.
var credentialParamQualifiers = []string{
	"id", "ids", "hash", "value", "val", "name", "index", "number",
}

// splitParamWords splits a parameter name into its words: on every separator
// (-, _, ., space, +) and at each camelCase boundary.
func splitParamWords(name string) []string {
	var words []string
	var cur strings.Builder
	var prevLower bool
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range name {
		switch {
		case r == '-' || r == '_' || r == '.' || r == ' ' || r == '+':
			flush()
			prevLower = false
		case unicode.IsUpper(r) && prevLower:
			flush()
			cur.WriteRune(r)
			prevLower = false
		default:
			cur.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		}
	}
	flush()
	return words
}

// queryCredentialValue finds one credential-looking query parameter anywhere in
// text and captures its value, so both the redactor and the leak scan can agree
// on what a URL carries. The "[?&;#]" prefix is deliberate: a bare "?key="
// inside prose is matched only when something before it looks like a URL… which
// is not something a regexp can decide, so callers pass single URLs. ";" is
// included because RFC 3986 lets it separate query parameters: with only "[?&]"
// the scanner matched the FIRST parameter, consumed the rest of the query as
// its value, and never looked at "key=…" again — so a ";"-separated credential
// was neither hidden nor known to the leak scan ("?seed=1;api_key=sk=…",
// 2026-09-26 audit). "#" is included because a fragment is not part of the
// request the transport sends, but it IS part of the URL string every error
// message quotes and every report prints, so `…#api_key=sk-…` leaked into a
// pasted ticket the same way (2026-09-26 audit, third round). The value class
// excludes ";" and "#" for the same reason.
var queryCredentialValue = regexp.MustCompile(`(?i)[?&;#]([^=&#;\s]+)=(\s*[^&#;\s]*)`)

// queryCredentialValueAll is queryCredentialValue for a string that IS one URL
// (a remote's base_url, never free prose): the value runs to the next "&", ";"
// or "#" even across whitespace, so a key pasted with a space in it is captured
// WHOLE rather than only up to the space. The whitespace-bounded rule above is
// for error text, where running past a space would swallow the diagnostic that
// follows the URL; this one is only used where the string is the URL.
var queryCredentialValueAll = regexp.MustCompile(`(?i)[?&;#]([^=&#;\s]+)=([^&#;]*)`)

// querySecrets returns every credential value text's query strings carry.
// Ordered left to right for stable error text and stable test assertions.
func querySecrets(text string) []string {
	var out []string
	for _, m := range queryCredentialValueAll.FindAllStringSubmatch(text, -1) {
		name, err := url.QueryUnescape(m[1])
		if err != nil {
			name = m[1]
		}
		if !looksLikeCredentialParam(name) {
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
	return redactQueryMatches(queryCredentialValue, text)
}

// redactQueryCredentialsAll is redactQueryCredentials for a string that IS one
// URL — see queryCredentialValueAll.
func redactQueryCredentialsAll(text string) string {
	return redactQueryMatches(queryCredentialValueAll, text)
}

func redactQueryMatches(re *regexp.Regexp, text string) string {
	return re.ReplaceAllStringFunc(text, func(m string) string {
		sub := re.FindStringSubmatch(m)
		name, err := url.QueryUnescape(sub[1])
		if err != nil {
			name = sub[1]
		}
		if !looksLikeCredentialParam(name) || strings.TrimSpace(sub[2]) == "" {
			return m
		}
		return m[:len(m)-len(sub[2])] + "REDACTED"
	})
}

// quotedSpan matches one double-quoted span of text — the shape net/http uses
// to quote the request URL inside its own errors.
var quotedSpan = regexp.MustCompile(`"[^"]*"`)

// redactQuotedQueryCredentials applies the URL-shaped query rule inside each
// quoted span of a message, where the closing quote is what bounds the value:
// a key pasted with a space in it ("...?api_key= sk-live-…": invalid character
// " " in host name) is otherwise only redacted up to the space, leaving the
// rest of the key on a line the user reads (2026-09-26 audit).
func redactQuotedQueryCredentials(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	return quotedSpan.ReplaceAllStringFunc(text, func(span string) string {
		if !strings.Contains(span, "://") {
			return span
		}
		return redactQueryCredentialsAll(span)
	})
}

// redactCredentials replaces every credential a piece of text could carry
// with REDACTED: the userinfo of a URL (including one url.Parse refuses, whose
// password may hold a "/" or a newline), a credential-bearing query value, and
// the parse-error fragment that re-states one outside the URL's quotes. Text
// with nothing to hide is returned unchanged, so this is safe to apply
// unconditionally to anything on its way out.
//
// This is the general sanitizer, so every shape has to be handled HERE, not by
// its callers: the Anthropic↔OpenAI translation proxy feeds an upstream's
// error body through it and hands the result back to the launched client, and
// two shapes — a key in the query string and a Basic password containing "/" —
// went through it in the clear (2026-09-26 audit). redactErr is the same work
// for an error value; redactBaseURL is it plus the single-URL rules.
func redactCredentials(text string) string {
	if strings.Contains(text, "@") {
		text = credentialInURL.ReplaceAllString(text, "${1}REDACTED@")
		// The quoted-URL rule catches a userinfo url.Parse cannot parse (a
		// Basic password with a "/" in it), which the "://…@" rule cannot see
		// past.
		text = quotedURLUserinfo.ReplaceAllString(text, `"${1}REDACTED@`)
	}
	// A base URL can also carry its key in the query string, and net/http
	// echoes the whole request URL in a transport error ("Get
	// \"https://host/v1?api_key=…\": dial tcp …"), so userinfo is not the
	// only place a key hides. The prose-bounded rule first (it cannot run past
	// a space into the diagnostic that follows), then the quote-bounded one,
	// where the closing quote is what bounds the value.
	text = redactQueryCredentials(text)
	text = redactQuotedQueryCredentials(text)
	// The parse-error path re-states the credential fragment outside the URL's
	// quotes, so it gets its own rule rather than relying on the quoted one.
	return invalidPortFragment.ReplaceAllString(text, `invalid port "REDACTED"`)
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
	// Two passes: the prose-bounded rule first (which keeps its behavior when
	// this is handed something longer than a URL), then the URL-shaped rule,
	// which is what catches a value with a space inside it.
	return redactQueryCredentialsAll(redactQueryCredentials(out))
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
	// doctor --report printed a query-string key on the line below one
	// redactBaseURL had already cleaned, which is why the query rules live in
	// redactCredentials itself (2026-09-26 audit).
	text := redactCredentials(err.Error())
	if text == err.Error() {
		return err
	}
	return redactedError{err: err, text: text}
}
