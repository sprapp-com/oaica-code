package sitebuilder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F130-L2-2 (2026-09-29 audit, round 130): a tab, LF or CR inside a URL is deleted by the browser, so
// `/<TAB>/evil.example` is a protocol-relative link.
func TestRound130SafeURLRefusesEmbeddedControlWhitespace(t *testing.T) {
	for _, u := range []string{"/\t/evil.example/x", "/\n/evil.example", "/\r/evil.example", "java\tscript:alert(1)"} {
		if got, ok := safeURL(u); ok {
			t.Errorf("safeURL(%q) = %q, want refused", u, got)
		}
	}
	if _, ok := safeURL("/about.html"); !ok {
		t.Error("an ordinary path must pass")
	}
}

// F130-L2-3: the preview neither serves dotfiles nor answers a foreign Host.
func TestRound130PreviewRefusesPrivateFilesAndForeignHosts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=x"), 0o600)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o700)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("url=x"), 0o600)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0o600)
	for _, p := range []string{"/site/.env", "/site/.git/config"} {
		if servableUnder(dir, p) {
			t.Errorf("%s must not be servable", p)
		}
	}
	if !servableUnder(dir, "/site/index.html") {
		t.Error("index.html must be servable")
	}
	h := previewHostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for host, want := range map[string]int{"rebind.attacker.example:4173": 403, "127.0.0.1:4173": 200, "localhost:4173": 200, "[::1]:4173": 200} {
		req := httptest.NewRequest("GET", "/site/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q -> %d, want %d", host, rec.Code, want)
		}
	}
}

func TestRound130PreviewServerAnswersOnlyThisMachine(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := Preview(ctx, dir, 0)
	base = strings.TrimSuffix(base, "/")
	if err != nil {
		t.Skip(err)
	}
	get := func(host string) int {
		req, _ := http.NewRequest("GET", base+"/site/index.html", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("rebind.attacker.example"); got != 403 {
		t.Errorf("foreign Host -> %d, want 403", got)
	}
	if got := get(strings.TrimPrefix(base, "http://")); got != 200 {
		t.Errorf("own Host -> %d, want 200", got)
	}
}
