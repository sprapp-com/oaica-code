package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/envconfig"
)

func TestRound115ListCacheStickyHydrateError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	home := t.TempDir()
	setTestHome(t, home)
	createListCacheModel(t, "sticky", map[string]any{"test.context_length": uint32(1024)}, "")

	models := envconfig.Models()
	_ = home
	_ = filepath.Join
	if err := os.Chmod(models, 0); err != nil {
		t.Fatal(err)
	}
	cache := newModelListCache()
	cache.Start(context.Background())
	werr := cache.Wait(context.Background())
	t.Logf("startup (models dir unreadable): Wait err = %v", werr)
	if err := os.Chmod(models, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("models dir readable again")

	for i := 0; i < 3; i++ {
		got, err := cache.List(context.Background())
		t.Logf("List #%d after recovery: %d models, err = %v", i+1, len(got), err)
	}
	if resp, err := GetModelInfo(api.ShowRequest{Model: "sticky"}); err == nil {
		t.Logf("same moment, /api/show door (GetModelInfo) serves the model: family=%q", resp.Details.Family)
	}
	if err := cache.syncManifests(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	t.Logf("syncManifests alone succeeds; cache holds %d entries", cache.Len())
	if _, err := cache.List(context.Background()); err != nil {
		t.Fatalf("RED: /api/tags stays %v after the directory recovered", err)
	}
}
