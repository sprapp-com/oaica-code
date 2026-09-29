package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/types/model"
)

// A namespace directory that lives on another filesystem (symlinked host dir).
func TestRound116SymlinkedHostDirOtherFS(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	root, _ := Path()
	other, err := os.MkdirTemp("/dev/shm", "probe116-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(other)
	if err := os.Symlink(other, filepath.Join(root, "registry.ollama.ai")); err != nil {
		t.Fatal(err)
	}
	n := model.ParseName("registry.ollama.ai/library/m:latest")
	err = WriteManifest(n, Layer{MediaType: "application/vnd.docker.container.image.v1+json", Digest: "sha256:aa", Size: 1}, nil)
	t.Logf("WriteManifest err = %v", err)
	if err != nil {
		t.Fatalf("RED: create into a symlinked host dir on another filesystem fails")
	}
}
