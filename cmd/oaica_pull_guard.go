package cmd

// oaica_pull_guard.go — the bounds and policies `oaica pull` applies to the
// data its router sends it. Split out of oaica_pull_serve.go because every
// value these enforce is chosen by the SENDER, not by this machine, and so
// each one needs its own rule rather than being inlined at a call site
// (2026-09-26 audit, third round):
//
//   - a redirect: net/http re-sends Authorization whenever the redirect
//     target's HOSTNAME matches, ignoring scheme and port, so "the pull_url
//     is on the router" was only ever true of the first hop;
//   - a chunk length (4 bytes, big-endian) that sized an allocation;
//   - a stream with no bound and no stall timeout;
//   - an hf_url that chose which host this machine would fetch from;
//   - an error body echoed in full into a terminal.

import (
	"errors"
	"fmt"
	"github.com/ollama/ollama/cmd/launch"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	// errPullTooLong means the sender kept going past the size its own
	// manifest declared.
	errPullTooLong = errors.New("stream is longer than the size the manifest declared")
	// errPullStalled means no bytes arrived within pullStallTimeout.
	errPullStalled = errors.New("no data received")
)

// maxPullErrorBodyBytes caps the error text a server's response body can put
// on this machine's stderr. A 96 MiB body was measured echoing into the
// message (2026-09-26 audit) — 288 MiB of heap and an unreadable terminal.
const maxPullErrorBodyBytes = 4 << 10

// maxEncryptedChunkBytes bounds the ciphertext length a single frame may
// declare. The format writes 8 MiB plaintext per chunk (see
// decryptChunkedAESGCMStream); this is 8x that, generous enough for a
// differently-configured producer and small enough that a 4-byte field
// cannot size a multi-gigabyte allocation.
const maxEncryptedChunkBytes = 64 << 20

// pullStallTimeout is how long a byte stream may make no progress at all
// before the transfer is abandoned. It is a variable so a test can lower it;
// production never sets it.
var pullStallTimeout = 5 * time.Minute

// pullHTTPClient returns a client for one leg of a pull. redirects decides
// what a 3xx may do — never "follow it": net/http's rule for re-sending
// Authorization compares only hostnames, so a redirect to the same name on a
// plaintext scheme, or on another port, receives the credential intact.
func pullHTTPClient(redirects func(*http.Request) error, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return redirects(req)
		},
	}
}

// noRedirects refuses every redirect. The router's own manifest and pull
// endpoints answer directly — a 3xx from them is not a normal answer, it is
// the manifest choosing a second host to receive the distribution licence and
// to supply the bytes installed as a model.
func noRedirects(*http.Request) error {
	return errors.New("the router answered with a redirect; refusing to follow it off the router")
}

// trustedHostRedirects allows a redirect only within the set of hosts already
// trusted for this leg, and only over https. HuggingFace's resolve URLs do
// redirect (to cdn-lfs.huggingface.co), so refusing all redirects would break
// real downloads; the allowlist is what makes following them safe.
func trustedHostRedirects(trusted func(string) bool) func(*http.Request) error {
	return func(req *http.Request) error {
		if req.URL.Scheme != "https" || !trusted(req.URL.String()) {
			return fmt.Errorf("refusing a redirect to %s: not a trusted https host for this leg", req.URL.Redacted())
		}
		return nil
	}
}

// hfAllowedHost is the host hf_url may name. hf_url is DATA from the router's
// manifest, so without this the router chose which host this machine would
// fetch from — including 127.0.0.1, the LAN, or a cloud metadata endpoint —
// and the response body was installed as the model. A variable so tests can
// point the HF path at a loopback server.
var hfAllowedHost = "huggingface.co"

// hfAllowedScheme is the only scheme an hf_url may use. Kept beside the host
// so the test hook can relax it for a loopback server, which speaks http.
var hfAllowedScheme = "https"

// hfURLIsTrusted reports whether a manifest's hf_url may be fetched at all:
// https, on huggingface.co or a subdomain of it. Same rule hfHostAcceptsToken
// applies to the token, now applied to the request.
func hfURLIsTrusted(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	if u.Scheme != hfAllowedScheme {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == hfAllowedHost || strings.HasSuffix(host, "."+hfAllowedHost)
}

// hfURLIsTrustedForTest points the hf_url allowlist at loopback so the HF legs
// can be exercised against a local server. It returns a restore function.
func hfURLIsTrustedForTest() func() {
	prevHost, prevScheme := hfAllowedHost, hfAllowedScheme
	hfAllowedHost = "127.0.0.1"
	hfAllowedScheme = "http" // httptest's plain server
	return func() { hfAllowedHost, hfAllowedScheme = prevHost, prevScheme }
}

// cappedReader enforces the size the manifest declared. Reading past it is an
// error, not a silent truncation: a sender that keeps going has already been
// measured at 1113600% of the declared size with the file still growing.
type cappedReader struct {
	r         io.Reader
	remaining int64
	probed    bool
}

func newCappedReader(r io.Reader, limit int64) *cappedReader {
	return &cappedReader{r: r, remaining: limit}
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		if c.probed {
			return 0, errPullTooLong
		}
		c.probed = true
		var probe [1]byte
		if n, _ := c.r.Read(probe[:]); n > 0 {
			return 0, errPullTooLong
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// countingReader counts the bytes actually taken off the wire, so a caller can
// tell a complete transfer from one that stopped at a chunk boundary.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// stallGuard turns "the server stopped sending but never closed" into an
// error. Without it the client has Timeout: 0 (a multi-GB file must not be
// killed by a total deadline) and a half-open connection hangs the command
// forever.
type stallGuard struct {
	r       io.Reader
	closer  io.Closer
	timeout time.Duration

	mu      sync.Mutex
	last    time.Time
	stalled bool
	done    chan struct{}
}

func newStallGuard(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}
	s := &stallGuard{r: body, closer: body, timeout: timeout, last: time.Now(), done: make(chan struct{})}
	go s.watch()
	return s
}

func (s *stallGuard) watch() {
	tick := s.timeout / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			expired := time.Since(s.last) >= s.timeout
			if expired {
				s.stalled = true
			}
			s.mu.Unlock()
			if expired {
				_ = s.closer.Close() // unblocks the pending Read
				return
			}
		}
	}
}

func (s *stallGuard) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.last = time.Now()
		s.mu.Unlock()
	}
	if err != nil {
		s.mu.Lock()
		stalled := s.stalled
		s.mu.Unlock()
		if stalled {
			return n, fmt.Errorf("%w within %s", errPullStalled, s.timeout)
		}
	}
	return n, err
}

func (s *stallGuard) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.closer.Close()
}

// readPullErrorBody reads a failing response's body for the message, capped
// and with a note when it was cut.
func readPullErrorBody(body io.Reader) string {
	// Redact BEFORE cutting: a body that echoes the licence key across the cap would otherwise print the part of
	// it that fell before the cut (round 132, F132-L2-4). Read a margin past the cap for the redactor to see.
	b, err := io.ReadAll(io.LimitReader(body, maxPullErrorBodyBytes+512))
	msg := oaicaDiagnosis(strings.TrimSpace(string(b)))
	if len(msg) > int(maxPullErrorBodyBytes) {
		cut := int(maxPullErrorBodyBytes)
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "… (truncated)"
	} else if err == nil && int64(len(b)) > maxPullErrorBodyBytes {
		msg += "… (truncated)"
	}
	// The body is the server's: it can echo the licence bearer this request carried and it can carry terminal
	// escapes (2026-09-29 audit, round 131, F131-L2-3).
	return launch.PrintableCell(msg)
}

// validateModelName refuses a model name that would address anything other
// than a file directly inside the models directory. A model name reaches
// oaicaModelPath from a command line and (for the router's manifest) from the
// network; in both cases it is a name, not a path.
func validateModelName(model string) error {
	if model == "" {
		return errors.New("empty model name")
	}
	if strings.ContainsAny(model, `/\`) || model != filepath.Base(model) {
		return fmt.Errorf("invalid model name %q: a model is a name, not a path", model)
	}
	if model == "." || model == ".." || strings.HasPrefix(model, ".") {
		return fmt.Errorf("invalid model name %q", model)
	}
	return nil
}
