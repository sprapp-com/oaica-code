package sitebuilder

// sitebuilder_integrity_test.go — three ways a site directory could leak or
// write something the user did not choose (2026-09-26 audit, third round):
//
//   - section ids became file names directly, so an id from site.json (or a
//     planner that echoed one) of "../../…" wrote and read outside the site;
//   - `oaica site deploy` exported EVERYTHING it found to a temporary
//     directory and uploaded it to a public host — dotfiles and credential
//     files included, and a symlink was followed and its target uploaded;
//   - the preview server refused the state directory by substring, which a
//     symlink named anything else walks straight past.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// S2: an id that is not a slug never becomes a path.
func TestSectionIDCannotEscapeTheSiteDir(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "escaped.html")

	s := &Site{Fragments: map[string]string{"../../" + filepath.Base(outside): "PWNED"}}
	if err := s.Save(dir); err == nil {
		t.Error("Save accepted a section id of \"../../…\" — it wrote outside the site directory")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("Save wrote %s, outside the site directory", outside)
	}
	_ = os.Remove(outside)
}

// The same for the direction that reads: a hand-edited site.json.
func TestLoadRefusesATraversingSectionID(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(filepath.Join(st, sectionsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"spec":{"title":"t","language":"en","sections":[{"id":"../../etc/passwd","kind":"hero","title":"H"}]}}`
	if err := os.WriteFile(filepath.Join(st, siteFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		// Refusing outright is a fine answer; the id must simply not be used
		// as a path either way.
		return
	}
	for _, sec := range got.Spec.Sections {
		if strings.ContainsAny(sec.ID, `/\`) || strings.Contains(sec.ID, "..") {
			t.Errorf("Load kept section id %q — the id is used as a file name, so it must be a plain slug", sec.ID)
		}
	}
}

// And the legitimate case still round-trips.
func TestSaveLoadRoundTripsNormalisedIDs(t *testing.T) {
	dir := t.TempDir()
	s := &Site{
		Spec:      Spec{Title: "T", Language: "en", Sections: []Section{{ID: "how-it-works", Kind: "features", Title: "How"}}},
		Fragments: map[string]string{"how-it-works": "<section>x</section>"},
	}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Fragments["how-it-works"] != "<section>x</section>" {
		t.Errorf("fragments = %+v, want the one that was saved", got.Fragments)
	}
}

// S7: Export uploads nothing private.
func TestExportSkipsPrivateFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY")

	writeFile(t, filepath.Join(dir, IndexFile), "<h1>site</h1>")
	writeFile(t, filepath.Join(dir, ".env"), "CLOUDFLARE_API_TOKEN=leak")
	writeFile(t, filepath.Join(dir, ".dev.vars"), "SECRET=leak")
	writeFile(t, filepath.Join(dir, "secrets.json"), `{"k":"leak"}`)
	writeFile(t, filepath.Join(dir, "tls.pem"), "CERT KEY")
	writeFile(t, filepath.Join(dir, ".wrangler", "state.json"), "state")
	writeFile(t, filepath.Join(dir, ".well-known", "security.txt"), "Contact: mailto:x@example.com")
	writeFile(t, filepath.Join(dir, "assets", "style.css"), "body{}")
	if err := os.Symlink(secret, filepath.Join(dir, "assets", "logo.css")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFile(t, filepath.Join(dir, StateDir, "site.json"), "{}")

	dst := t.TempDir()
	if err := Export(dir, dst); err != nil {
		t.Fatalf("Export: %v", err)
	}

	uploaded := map[string]bool{}
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		uploaded[rel] = true
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "leak") || strings.Contains(string(b), "PRIVATE KEY") {
			t.Errorf("%s was uploaded with its contents: the deploy target is a public host", rel)
		}
		return nil
	})

	for _, want := range []string{IndexFile, filepath.Join("assets", "style.css"), filepath.Join(".well-known", "security.txt")} {
		if !uploaded[want] {
			t.Errorf("%s was not exported, but it is publishable content (uploaded: %v)", want, uploaded)
		}
	}
	for _, bad := range []string{".env", ".dev.vars", "secrets.json", "tls.pem",
		filepath.Join(".wrangler", "state.json"), filepath.Join("assets", "logo.css"),
		filepath.Join(StateDir, siteFile)} {
		if uploaded[bad] {
			t.Errorf("%s was exported for upload — credentials, keys, the site state and symlinked targets must not leave the machine", bad)
		}
	}
}

// S8: the previewer refuses the state directory however it is addressed.
func TestPreviewRefusesASymlinkAliasToTheStateDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, IndexFile), "<h1>site</h1>")
	writeFile(t, filepath.Join(dir, StateDir, siteFile), `{"brief":"the user's private brief"}`)
	if err := os.Symlink(StateDir, filepath.Join(dir, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFile(t, filepath.Join(dir, "sub", "page.html"), "<p>ok</p>")
	if err := os.Symlink(filepath.Join(dir, StateDir), filepath.Join(dir, "sub", "deep")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := Preview(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	get := func(p string) (int, string, error) {
		resp, err := http.Get(url + p)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	for _, p := range []string{
		"site/" + StateDir + "/" + siteFile, // the plain path (already refused before)
		"site/alias/" + siteFile,            // a symlink at the top level
		"site/sub/deep/" + siteFile,         // a symlink one level down
		"site/sub/../alias/" + siteFile,     // and via a ".." hop
	} {
		code, body, err := get(p)
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusNotFound {
			t.Errorf("GET /%s = %d, want 404 — the state directory holds the brief and the plan and must never be served, however it is addressed\nbody: %s", p, code, body)
		}
		if strings.Contains(body, "private brief") {
			t.Errorf("GET /%s served the brief:\n%s", p, body)
		}
	}

	if code, body, err := get("site/sub/page.html"); err != nil || code != 200 || body != "<p>ok</p>" {
		t.Errorf("GET /site/sub/page.html = %d %q (%v), want the page — the rule must not refuse ordinary content", code, body, err)
	}
}

// A path that climbs out of the site directory entirely is refused too.
func TestPreviewRefusesEscapingTheSiteDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "site")
	writeFile(t, filepath.Join(dir, IndexFile), "<h1>site</h1>")
	writeFile(t, filepath.Join(root, "outside.txt"), "neighbour")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := Preview(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"site/../outside.txt", "site/%2e%2e/outside.txt"} {
		resp, err := http.Get(url + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || strings.Contains(string(b), "neighbour") {
			t.Errorf("GET /%s = %d %q — a path outside the site directory must not be served", p, resp.StatusCode, b)
		}
	}
}

// The preview still serves the wrapper and the site itself.
func TestPreviewStillServesTheSite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, IndexFile), "<h1>site</h1>")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := Preview(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(url + "site/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<h1>site</h1>") {
		t.Errorf("GET /site/ = %d %q, want the site", resp.StatusCode, b)
	}
}
