package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func TestRound115PullManifestPartialWrite(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())

	writeBlob := func(b []byte) string {
		d := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
		p, err := manifest.BlobsPath(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	cfg1 := writeBlob([]byte(`{"model_format":"gguf","model_family":"one"}`))
	cfg2 := writeBlob([]byte(`{"model_format":"gguf","model_family":"two"}`))
	layer := writeBlob([]byte("weights"))
	mfJSON := func(cfg string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":%q,"size":44},"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":%q,"size":7}]}`, cfg, layer)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			fmt.Fprint(w, mfJSON(cfg2))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	n := model.ParseName(u.Host + "/test/m")
	n.ProtocolScheme = "http"

	// The installed, working model: manifest v1.
	fp, err := manifest.PathForName(n)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(fp), 0o755)
	if err := os.WriteFile(fp, []byte(mfJSON(cfg1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.ParseNamedManifest(n); err != nil {
		t.Fatalf("before: %v", err)
	}
	t.Logf("before pull: installed manifest parses (%d bytes)", len(mfJSON(cfg1)))

	// Disk fills: at most 64 bytes more per file.
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old)
	lim := syscall.Rlimit{Cur: 64, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	perr := PullModel(t.Context(), n.String(), &registryOptions{Insecure: true}, func(api.ProgressResponse) {})
	syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	t.Logf("pull (disk full while writing manifest): err = %v", perr)

	bts, _ := os.ReadFile(fp)
	t.Logf("installed manifest on disk now: %d bytes %q", len(bts), string(bts))
	if _, err := manifest.ParseNamedManifest(n); err != nil {
		t.Fatalf("RED: a failed pull destroyed the model that was installed and working: %v", err)
	}
}
