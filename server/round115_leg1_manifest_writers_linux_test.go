package server

// F115-L1-2 (2026-09-29 audit, round 115): the other writers of a manifest — create and copy —
// leave an installed model whole when the disk fills mid-write. Linux only: RLIMIT_FSIZE fault
// injection, with SIGXFSZ ignored so the write fails with an error instead of killing the process.

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func limitFileSize(t *testing.T, n uint64) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) })
}

func TestRound115ManifestWritersLeaveTheInstalledModelWhole(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	installed := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"sha256:aa","size":1},"layers":[]}` + "\n"
	put := func(n model.Name) string {
		fp, err := manifest.PathForName(n)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(fp), 0o755)
		if err := os.WriteFile(fp, []byte(installed), 0o644); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	dst, src := model.ParseName("example.com/test/dst"), model.ParseName("example.com/test/src")
	dstPath := put(dst)
	put(src)

	limitFileSize(t, 64)
	if err := manifest.WriteManifest(dst, manifest.Layer{MediaType: "application/vnd.docker.container.image.v1+json", Digest: "sha256:bb", Size: 2}, nil); err == nil {
		t.Fatal("premise: WriteManifest under a 64-byte limit should fail")
	}
	if b, _ := os.ReadFile(dstPath); string(b) != installed {
		t.Errorf("a failed create left the installed manifest as %q", strings.TrimSpace(string(b)))
	}
	if err := CopyModel(src, dst); err == nil {
		t.Fatal("premise: CopyModel under a 64-byte limit should fail")
	}
	if b, _ := os.ReadFile(dstPath); string(b) != installed {
		t.Errorf("a failed copy left the installed manifest as %q", strings.TrimSpace(string(b)))
	}
	root, _ := manifest.Path()
	if left, _ := filepath.Glob(filepath.Join(root, ".tmp", "*")); len(left) > 0 {
		t.Errorf("failed writes left temp files behind: %v", left)
	}
}
