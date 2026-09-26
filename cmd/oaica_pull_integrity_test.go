package cmd

// oaica_pull_integrity_test.go — `oaica pull` runs three network legs (the
// manifest, the router's byte stream, and optionally a HuggingFace blob) and
// installs the result as a model file that `oaica serve` will load. Every
// field those legs carry is DATA from the router, and the file they produce is
// executed on later (2026-09-26 audit, third round):
//
//   - the authority checks on pull_url / hf_url constrain the FIRST hop only,
//     so a 3xx moved the distribution licence and the installed bytes
//     somewhere else;
//   - a 4-byte chunk length in the encrypted frame format was allocated
//     verbatim (4 GiB from a 17-byte response → a fatal OOM kill);
//   - the byte stream was not bounded by the manifest's declared size and the
//     client had no stall bound;
//   - hf_url was fetched verbatim, so the router chose any host this machine
//     could reach, including loopback;
//   - the router leg checked neither the declared sha256 nor (on the encrypted
//     path) that the stream was complete.

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pullRouter serves a manifest for one model and answers /v1/pull/<model>.
// The handler for the byte stream is supplied by the test.
type pullRouter struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newPullRouter(t *testing.T, manifest map[string]any, stream http.HandlerFunc) *pullRouter {
	t.Helper()
	pr := &pullRouter{}
	pr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr.mu.Lock()
		pr.seen = append(pr.seen, fmt.Sprintf("%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization")))
		pr.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/manifest/"):
			_ = jsonEncode(w, manifest)
		case strings.HasPrefix(r.URL.Path, "/v1/pull/"):
			if stream != nil {
				stream(w, r)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(pr.Close)
	return pr
}

func (pr *pullRouter) requests() []string {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return append([]string(nil), pr.seen...)
}

func jsonEncode(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func installedModelBytes(t *testing.T, model string) []byte {
	t.Helper()
	p, err := oaicaModelPath(model)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("no model was installed: %v", err)
	}
	return b
}

// B1: a redirect off the router's authority must not carry the licence, and
// its bytes must not be installed as the model.
func TestPullDoesNotFollowARedirectOffTheRouter(t *testing.T) {
	invPullSetup(t)

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("EVIL!"))
	}))
	defer other.Close()

	router := newPullRouter(t, map[string]any{
		"model": "steal", "size_bytes": 5, "pull_url": "/v1/pull/steal", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/pull/steal", http.StatusFound)
	})
	t.Setenv("OAICA_HOST", router.URL)

	dest, err := oaicaPullModel("steal")
	if err == nil {
		t.Errorf("a 302 to %s was followed and reported success (%s) — the manifest's pull_url guard constrains the first hop only; the licence key and the installed bytes both moved off the router", other.URL, dest)
	}
	if _, serr := os.Stat(dest); serr == nil {
		if string(installedModelBytes(t, "steal")) == "EVIL!" {
			t.Error("the redirect target's bytes were installed as the model")
		}
	}
}

// And on the manifest leg: the licence key must not be handed to a redirect
// target either.
func TestManifestRedirectOffTheRouterIsRefused(t *testing.T) {
	invPullSetup(t)

	var got []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_ = jsonEncode(w, map[string]any{"model": "m", "size_bytes": 0, "pull_url": "/v1/pull/m", "source": "file"})
	}))
	defer other.Close()

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/manifest/m", http.StatusFound)
	}))
	defer router.Close()
	t.Setenv("OAICA_HOST", router.URL)

	if _, err := oaicaPullModel("m"); err == nil {
		t.Error("a manifest redirect off the router was followed — the licence key rides that request as a bearer")
	}
	for _, a := range got {
		if strings.Contains(a, "sk-license") {
			t.Errorf("the redirect target received the distribution licence: %q", a)
		}
	}
}

// B2: a chunk length in the frame format is a number the SENDER chooses. A
// 4-byte field must not size an allocation.
func TestDecryptChunkLengthIsBounded(t *testing.T) {
	// FF FF FF FF = 4 GiB of claimed ciphertext, then 12 nonce bytes and one
	// stray byte. Only 17 bytes of input, so a correct reader errors long
	// before it allocates anything.
	frame := append([]byte{0xff, 0xff, 0xff, 0xff}, make([]byte, 13)...)
	key := make([]byte, 32)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := decryptChunkedAESGCMStream(strings.NewReader(string(frame)), io.Discard, key)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Error("a frame declaring a 4 GiB chunk was accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("decrypting a 17-byte frame allocated %d bytes — the chunk length is attacker-chosen and must be bounded before make([]byte, ctLen)", grew)
	}
}

// B3: the stream is bounded by the size the manifest declared, and a server
// that keeps sending does not fill the disk or hang the command.
func TestPullStreamIsBoundedByTheDeclaredSize(t *testing.T) {
	invPullSetup(t)

	const declared = 1024
	stop := make(chan struct{})
	defer close(stop)
	router := newPullRouter(t, map[string]any{
		"model": "flood", "size_bytes": declared, "pull_url": "/v1/pull/flood", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		buf := make([]byte, 32*1024)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := w.Write(buf); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	t.Setenv("OAICA_HOST", router.URL)

	done := make(chan struct{})
	var err error
	var dest string
	go func() {
		dest, err = oaicaPullModel("flood")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a server that never stops sending never ended the download — the client has no bound on the stream and no stall timeout")
	}
	if err == nil {
		t.Errorf("a stream far longer than the declared %d bytes was accepted (installed at %s)", declared, dest)
	}
	if _, serr := os.Stat(dest); serr == nil {
		if fi, _ := os.Stat(dest); fi.Size() > declared {
			t.Errorf("installed file is %d bytes for a manifest declaring %d", fi.Size(), declared)
		}
	}
}

// B4: hf_url is router-supplied, so it must not address an arbitrary host —
// not loopback, not the LAN, not a metadata endpoint.
func TestHFURLMustBeHuggingFace(t *testing.T) {
	invPullSetup(t)

	var hits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("INTERNAL-ONLY"))
	}))
	defer internal.Close()

	router := newPullRouter(t, map[string]any{
		"model": "ssrf", "size_bytes": 12, "source": "hf", "hf_url": internal.URL + "/admin/rotate",
	}, nil)
	t.Setenv("OAICA_HOST", router.URL)

	if _, err := oaicaPullModel("ssrf"); err == nil {
		t.Error("a manifest naming an internal host as hf_url was fetched and installed — the router chooses this URL and the client may reach the LAN and loopback through it")
	}
	if hits != 0 {
		t.Errorf("the internal host received %d request(s)", hits)
	}

	// The default rule itself, pinned: https on huggingface.co, nothing else.
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://huggingface.co/oaica/x/resolve/main/a.gguf", true},
		{"https://cdn-lfs.huggingface.co/x", true}, // resolve URLs redirect here; the token must ride it
		{"http://huggingface.co/x", false},
		{"https://127.0.0.1/x", false},
		{"https://huggingface.co.evil.example/x", false},
		{"https://169.254.169.254/latest/meta-data/", false},
	} {
		if got := hfURLIsTrusted(tc.url); got != tc.want {
			t.Errorf("hfURLIsTrusted(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// encryptChunkedForTest writes the frame format decryptChunkedAESGCMStream
// reads, so a test can produce a real encrypted blob.
func encryptChunkedForTest(t *testing.T, key []byte, chunks [][]byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for i, pt := range chunks {
		nonce := make([]byte, gcm.NonceSize())
		for j := range nonce {
			nonce[j] = byte(i + j)
		}
		ct := gcm.Seal(nil, nonce, pt, nil)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
		out = append(out, l[:]...)
		out = append(out, nonce...)
		out = append(out, ct...)
	}
	return out
}

// B5: a failing transport on either leg must print through the same redaction
// the manifest leg already uses. net/http echoes the URL — including any
// credential in its query string — verbatim in its own error text.
func TestPullTransportErrorsAreRedacted(t *testing.T) {
	t.Run("manifest leg", func(t *testing.T) {
		invPullSetup(t)
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		base := dead.URL
		dead.Close()
		t.Setenv("OAICA_HOST", base+"?api_key=sk-live-QUERYSECRET")

		_, err := oaicaPullModel("m")
		if err == nil {
			t.Fatal("premise: pulling from a closed port should fail")
		}
		if strings.Contains(err.Error(), "sk-live-QUERYSECRET") {
			t.Errorf("the manifest leg printed the host's credential: %v", err)
		}
	})

	t.Run("pull leg", func(t *testing.T) {
		invPullSetup(t)
		// The host carries the credential in its query string, which also
		// means the manifest URL's path resolves to "/" (the query eats it) —
		// so the first request, whatever it asks for, is answered with the
		// manifest, and the SECOND one is the byte stream: it dies without
		// responding, the shape that makes net/http build its error text out
		// of the whole URL, credential included.
		var n int
		router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				_ = jsonEncode(w, map[string]any{"model": "m", "size_bytes": 4, "pull_url": "/v1/pull/m", "source": "file"})
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("premise: the test server cannot hijack")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
		}))
		defer router.Close()
		t.Setenv("OAICA_HOST", router.URL+"?api_key=sk-live-QUERYSECRET")

		_, err := oaicaPullModel("m")
		if err == nil {
			t.Fatal("premise: a connection that dies mid-request should fail the pull")
		}
		if n < 2 {
			t.Fatalf("only %d request(s) reached the router, so the byte-stream leg was never exercised: %v", n, err)
		}
		if strings.Contains(err.Error(), "sk-live-QUERYSECRET") {
			t.Errorf("the pull leg printed the host's credential: %v", err)
		}
	})
}

// B6: an encrypted HF download that stops at a chunk boundary is truncation,
// not completion.
func TestTruncatedEncryptedStreamIsRefused(t *testing.T) {
	invPullSetup(t)
	restore := hfURLIsTrustedForTest()
	defer restore()

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	full := encryptChunkedForTest(t, key, [][]byte{[]byte("chunk-one-"), []byte("chunk-two-")})
	ciphertextLen := int64(len(full))
	truncated := full[:len(full)/2] // stop after the first frame

	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(truncated)))
		_, _ = w.Write(truncated)
	}))
	defer blob.Close()

	router := newPullRouter(t, map[string]any{
		"model": "trunc", "size_bytes": ciphertextLen, "source": "hf",
		"hf_url": blob.URL + "/a.gguf", "decrypt_key": hex.EncodeToString(key),
	}, nil)
	t.Setenv("OAICA_HOST", router.URL)

	dest, err := oaicaPullModel("trunc")
	if err == nil {
		got, _ := os.ReadFile(dest)
		t.Errorf("a transfer that stopped at a chunk boundary was installed as a complete %d-byte model (manifest declared %d ciphertext bytes): %q",
			len(got), ciphertextLen, got)
	}
}

// B8: the router leg must honour the manifest's own sha256, and size_bytes: 0
// must not disarm the only remaining check.
func TestRouterPullChecksDeclaredSHA256(t *testing.T) {
	invPullSetup(t)

	body := []byte("EVIL!")
	router := newPullRouter(t, map[string]any{
		"model": "sha", "size_bytes": len(body), "pull_url": "/v1/pull/sha", "source": "file",
		"sha256": strings.Repeat("0", 64),
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	})
	t.Setenv("OAICA_HOST", router.URL)

	if _, err := oaicaPullModel("sha"); err == nil {
		t.Error("bytes that do not match the manifest's sha256 were installed (the HF plaintext path checks this; the router path did not)")
	}
	// The zero-size variant: no size to compare, so the digest is the only
	// thing standing between the manifest server and the installed model.
	router2 := newPullRouter(t, map[string]any{
		"model": "sha0", "size_bytes": 0, "pull_url": "/v1/pull/sha0", "source": "file",
		"sha256": strings.Repeat("0", 64),
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	t.Setenv("OAICA_HOST", router2.URL)
	if _, err := oaicaPullModel("sha0"); err == nil {
		t.Error("a manifest with size_bytes 0 and a sha256 that does not match still installed its bytes")
	}
}

// B7: an error body is echoed into the message — cap it.
func TestPullErrorBodyIsCapped(t *testing.T) {
	invPullSetup(t)

	huge := strings.Repeat("A", 4<<20)
	router := newPullRouter(t, map[string]any{
		"model": "big", "size_bytes": 1, "pull_url": "/v1/pull/big", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(huge))
	})
	t.Setenv("OAICA_HOST", router.URL)

	_, err := oaicaPullModel("big")
	if err == nil {
		t.Fatal("premise: a 401 should fail the pull")
	}
	if len(err.Error()) > 8192 {
		t.Errorf("the error message is %d bytes — a server's error body is echoed into it with no cap", len(err.Error()))
	}
}

// B9: the model name becomes a file name under the models directory.
func TestModelNameCannotEscapeTheModelsDir(t *testing.T) {
	invPullSetup(t)
	home := os.Getenv("HOME")
	outside := filepath.Join(home, "outside", "pwn.gguf")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../outside/pwn", "../../etc/passwd", "a/../../b"} {
		if p, err := oaicaModelPath(name); err == nil {
			t.Errorf("oaicaModelPath(%q) = %q — a model name is user input and must not address another directory", name, p)
		}
	}
	if _, err := oaicaModelPath("good-name"); err != nil {
		t.Errorf("oaicaModelPath refused an ordinary name: %v", err)
	}
}

// B10: downloaded weights are licensed plaintext; they are not world-readable.
func TestDownloadedModelIsNotGroupOrWorldReadable(t *testing.T) {
	invPullSetup(t)

	body := []byte("weights-bytes")
	router := newPullRouter(t, map[string]any{
		"model": "perm", "size_bytes": len(body), "pull_url": "/v1/pull/perm", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	})
	t.Setenv("OAICA_HOST", router.URL)

	dest, err := oaicaPullModel("perm")
	if err != nil {
		t.Fatalf("premise: a well-formed pull should succeed: %v", err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the installed model is mode %v — weights are licensed content and every other file this tool writes is 0600", fi.Mode().Perm())
	}
}

// B3, second half: a server that sends a little and then holds the connection
// open with no further bytes must not hang the command forever. Timeout: 0 is
// deliberate (a multi-GB file must survive a slow link), so something else has
// to notice the silence.
func TestStalledStreamIsAbandoned(t *testing.T) {
	invPullSetup(t)
	prev := pullStallTimeout
	pullStallTimeout = 500 * time.Millisecond
	t.Cleanup(func() { pullStallTimeout = prev })

	release := make(chan struct{})
	defer close(release)
	router := newPullRouter(t, map[string]any{
		"model": "stall", "size_bytes": 1 << 20, "pull_url": "/v1/pull/stall", "source": "file",
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("starting"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the connection open, sending nothing
	})
	t.Setenv("OAICA_HOST", router.URL)

	done := make(chan error, 1)
	go func() {
		_, err := oaicaPullModel("stall")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stream that stopped mid-transfer was reported as a complete download")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a server that stopped sending never ended the pull — with Timeout: 0 a half-open connection hangs the command with no bound at all")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".oaica", "models", "stall.gguf")); err == nil {
		t.Error("the incomplete download was installed")
	}
}
