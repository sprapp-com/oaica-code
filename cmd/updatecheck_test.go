package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.4.2", "0.4.3", true},
		{"0.4.3", "0.4.2", false},
		{"0.4.3", "0.4.3", false},
		{"0.4.9", "0.5.0", true},
		{"0.9.9", "1.0.0", true},
		{"1.0.0", "0.9.9", false},
		{"0.4", "0.4.1", true}, // short component treated as 0
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchLatestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("version=0.4.9\ncommit=deadbeef\n"))
	}))
	defer srv.Close()

	old := updateCheckURLForTest(srv.URL)
	defer old()

	got := fetchLatestVersion(context.Background())
	if got != "0.4.9" {
		t.Errorf("fetchLatestVersion() = %q, want %q", got, "0.4.9")
	}
}

func TestFetchLatestVersion_UnreachableReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // guaranteed connection refused

	old := updateCheckURLForTest(url)
	defer old()

	if got := fetchLatestVersion(context.Background()); got != "" {
		t.Errorf("expected empty string on unreachable server, got %q", got)
	}
}

// setUpdateCheckHome points os.UserHomeDir's effective result (via HOME) at
// a temp dir so cache read/write tests never touch the developer's real
// ~/.oaica/update_check.json.
func setUpdateCheckHome(t *testing.T) {
	t.Helper()
	old := os.Getenv("HOME")
	dir := t.TempDir()
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", old) })
}

func TestUpdateCheckCache_RoundTrips(t *testing.T) {
	setUpdateCheckHome(t)
	want := updateCheckCache{LastChecked: time.Now().Truncate(time.Second), LatestVersion: "0.4.9", Notified: "0.4.8"}
	saveUpdateCheckCache(want)
	got := loadUpdateCheckCache()
	if got.LatestVersion != want.LatestVersion || got.Notified != want.Notified {
		t.Errorf("cache round-trip = %+v, want %+v", got, want)
	}
}

func TestUpdateCheckCache_MissingFileReturnsZeroValue(t *testing.T) {
	setUpdateCheckHome(t)
	got := loadUpdateCheckCache()
	if got.LatestVersion != "" || !got.LastChecked.IsZero() {
		t.Errorf("expected zero-value cache when no file exists, got %+v", got)
	}
}

func TestUpdateCheckCachePath_UnderOaicaDir(t *testing.T) {
	setUpdateCheckHome(t)
	path, err := updateCheckCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(path)) != ".oaica" {
		t.Errorf("expected cache under ~/.oaica, got %q", path)
	}
	if filepath.Base(path) != "update_check.json" {
		t.Errorf("expected update_check.json, got %q", filepath.Base(path))
	}
}
