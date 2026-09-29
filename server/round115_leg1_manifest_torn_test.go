package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func TestRound115PullManifestTornRead(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	writeBlob := func(b []byte) string {
		d := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
		p, _ := manifest.BlobsPath(d)
		os.WriteFile(p, b, 0o644)
		return d
	}
	cfg := writeBlob([]byte(`{"model_format":"gguf","model_family":"one"}`))
	layer := writeBlob([]byte("weights"))
	body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":%q,"size":44},"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":%q,"size":7}]}`, cfg, layer)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			fmt.Fprint(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	n := model.ParseName(u.Host + "/test/m")
	n.ProtocolScheme = "http"
	fp, _ := manifest.PathForName(n)
	os.MkdirAll(filepath.Dir(fp), 0o755)
	os.WriteFile(fp, []byte(body), 0o644)

	var stop atomic.Bool
	var reads, torn atomic.Int64
	var firstErr atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			reads.Add(1)
			if _, err := manifest.ParseNamedManifest(n); err != nil {
				torn.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
			}
		}
	}()
	for i := 0; i < 300; i++ {
		// the same model, re-pulled while it is installed (an up-to-date re-pull rewrites the same bytes)
		if err := PullModel(t.Context(), n.String(), &registryOptions{Insecure: true}, func(api.ProgressResponse) {}); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	<-done
	t.Logf("re-pulls=300 reads=%d failed reads of an installed, unchanged model=%d first=%v", reads.Load(), torn.Load(), firstErr.Load())
	if torn.Load() > 0 {
		t.Fatalf("RED: a reader saw the installed model's manifest torn while a pull rewrote identical bytes")
	}
}

// The temporary file a manifest write goes through is invisible to a listing: Manifests walks
// `*/*/*/*`, and a file it globbed and then lost to the rename made it fail outright.
func TestRound115ManifestTempFileIsNotListed(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	root, err := manifest.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tmp", "manifest-1.tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manifest.WriteManifest(model.ParseName("example.com/test/m"), manifest.Layer{MediaType: "x", Digest: "sha256:aa", Size: 1}, nil); err != nil {
		t.Fatal(err)
	}
	ms, err := manifest.Manifests(false)
	if err != nil {
		t.Fatalf("a leftover temp file broke the listing: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("listed %d manifests, want 1", len(ms))
	}
}
