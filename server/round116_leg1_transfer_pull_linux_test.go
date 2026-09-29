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

func pullProbe(t *testing.T, media string) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	writeBlob := func(b []byte) (string, int) {
		d := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
		p, _ := manifest.BlobsPath(d)
		os.WriteFile(p, b, 0o644)
		return d, len(b)
	}
	cfg1, s1 := writeBlob([]byte(`{"model_format":"safetensors","model_family":"one"}`))
	cfg2, s2 := writeBlob([]byte(`{"model_format":"safetensors","model_family":"two"}`))
	layer, sl := writeBlob([]byte("weights"))
	mfJSON := func(cfg string, cs int) string {
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`, cfg, cs, media, layer, sl)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			fmt.Fprint(w, mfJSON(cfg2, s2))
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
	os.WriteFile(fp, []byte(mfJSON(cfg1, s1)), 0o644)

	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old)
	syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 64, Max: old.Max})
	perr := PullModel(t.Context(), n.String(), &registryOptions{Insecure: true}, func(api.ProgressResponse) {})
	syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	bts, _ := os.ReadFile(fp)
	t.Logf("media=%s pull err=%v; installed manifest now %d bytes", media, perr, len(bts))
	if _, err := manifest.ParseNamedManifest(n); err != nil {
		t.Fatalf("RED: failed pull destroyed installed model: %v", err)
	}
}

func TestRound116PullGGUF(t *testing.T)   { pullProbe(t, "application/vnd.ollama.image.model") }
func TestRound116PullTensor(t *testing.T) { pullProbe(t, "application/vnd.ollama.image.tensor") }
