package sitebuilder

// lifecycle_integrity_test.go — the site builder's life-cycle around the page:
// what Load does with a fragment it cannot read, what Save does when it dies
// partway, what Export copies to a public host, and what happens when the site
// directory is a symlink. Five defects reproduced through this package and the
// real CLI (2026-09-26 audit, fourth round):
//
//   - Load treated ANY read error as "no fragment", and Save's stale-fragment
//     cleanup then deleted the file it could not read: one unreadable
//     hero.html turned the next `oaica site edit` into a page that had lost a
//     section, with site.json still listing it and the nav still linking to it;
//   - Save wrote plain files in an order that put index.html last, so a failed
//     final write left the state ahead of the deployed page — reported as
//     success, published as the previous site;
//   - Export walked a symlinked root as a FILE: nothing was copied, nil was
//     returned, and Deploy printed a *.pages.dev URL for an empty upload;
//   - a symlinked .oaica-site wrote state outside the site directory, and when
//     it pointed back inside the tree Export uploaded the brief and the plan;
//   - Export's docstring promised a whitelist; the regex was a narrow
//     blacklist, and secrets.txt / token.json / api-keys.txt / deploy.sh /
//     wrangler.toml were published.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lifecycleSite(t *testing.T) (*Site, string) {
	t.Helper()
	dir := t.TempDir()
	ids := []string{"hero", "services", "pricing"}
	secs := make([]Section, 0, len(ids))
	frags := map[string]string{}
	for _, id := range ids {
		secs = append(secs, Section{ID: id, Kind: id, Title: id})
		frags[id] = `<section id="` + id + `"><p>body of ` + id + `</p></section>`
	}
	s := &Site{Prompt: "landing page for a JB dental clinic with 30% off", Model: "m",
		Spec: Spec{Title: "T", Language: "en", Sections: secs}, Fragments: frags}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// L1: a fragment that exists but cannot be read is an ERROR, not an empty
// section. Silently dropping it is what let the next Save delete it.
func TestAnUnreadableFragmentIsLoudNotSilent(t *testing.T) {
	_, dir := lifecycleSite(t)
	path := filepath.Join(dir, StateDir, sectionsDir, "hero.html")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)

	got, err := Load(dir)
	if err == nil {
		t.Fatalf("Load returned %d fragments and err=nil for a fragment it could not read — the next Save's stale-fragment cleanup deletes that file, the published page loses the section while site.json still lists it, and the nav keeps an anchor with no target", len(got.Fragments))
	}
	if !strings.Contains(err.Error(), "hero.html") {
		t.Errorf("the error does not name the file that could not be read: %v", err)
	}

	// And the legitimate case — a section that has no fragment YET (a plan
	// that was not generated, or a section added since) — must stay
	// non-fatal, or the first `oaica site new` after a crash could never run.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err = Load(dir)
	if err != nil {
		t.Fatalf("a section with no fragment file yet must load fine (it is regenerated): %v", err)
	}
	if _, ok := got.Fragments["hero"]; ok {
		t.Error("a missing fragment file became a fragment")
	}
}

// L2: index.html is a derivation of the state, so a read repairs a stale one.
func TestAStaleIndexIsRedrawnFromTheState(t *testing.T) {
	s, dir := lifecycleSite(t)
	stale := "<!doctype html><title>previous site</title>"
	if err := os.WriteFile(filepath.Join(dir, IndexFile), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != Assemble(got) {
		t.Fatalf("index.html still diverges from the state it is derived from:\non disk = %q\nstate   = %q", string(b), Assemble(got))
	}
	if strings.Contains(string(b), "previous site") {
		t.Error("the stale page survived")
	}
	if Assemble(s) != Assemble(got) {
		t.Error("Load did not round-trip the state it saved")
	}
}

// L3: Save writes through a temp file and a rename, so it never follows a
// path that is not the file it means to write, and it replaces a file whose
// contents are not writable. Both are observable consequences of the rename —
// os.WriteFile has neither.
func TestSaveWritesThroughARename(t *testing.T) {
	_, dir := lifecycleSite(t)

	// 1. a fragment path that is a symlink: the LINK is replaced, the file it
	// pointed at is left alone. os.WriteFile followed the link and overwrote
	// the target — a state write reaching a file outside the site directory.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, StateDir, sectionsDir, "hero.html")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Save(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != "do not touch" {
		t.Errorf("Save wrote THROUGH a symlinked fragment path: the file outside the site now holds %q", string(b))
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the fragment path is still a symlink after Save (err=%v)", err)
	}

	// 2. no temp files survive a save
	var strays []string
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), ".tmp") {
			strays = append(strays, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(strays) > 0 {
		t.Errorf("Save left temp files behind: %v", strays)
	}
}

// L4: the nav links only to sections that are actually on the page.
func TestNavDoesNotLinkToASectionThatIsNotOnThePage(t *testing.T) {
	s := &Site{Prompt: "p", Spec: Spec{Title: "T", Language: "en", Sections: []Section{
		{ID: "hero", Kind: "hero", Title: "Hero"},
		{ID: "services", Kind: "services", Title: "Services"},
		{ID: "pricing", Kind: "pricing", Title: "Pricing"},
	}}, Fragments: map[string]string{
		"hero":     `<section id="hero">h</section>`,
		"services": `<section id="services">s</section>`,
		// pricing has no fragment: it is in the plan but not on the page
	}}
	page := Assemble(s)
	if strings.Contains(page, `href="#pricing"`) {
		t.Errorf("the header advertises an anchor with no target in the page:\n%s", page)
	}
	if !strings.Contains(page, `href="#services"`) {
		t.Error("the header dropped a section that IS on the page")
	}
}

// L5: a symlinked state directory is refused, in both directions.
func TestASymlinkedStateDirIsRefused(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	link := filepath.Join(dir, StateDir)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	s := &Site{Prompt: "the user's private brief", Model: "m",
		Spec:      Spec{Title: "T", Sections: []Section{{ID: "hero", Kind: "hero", Title: "H"}}},
		Fragments: map[string]string{"hero": `<section id="hero">c</section>`}}
	if err := s.Save(dir); err == nil {
		t.Error("Save wrote state through a symlinked .oaica-site, which can place site.json and section files anywhere on the filesystem")
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load read state through a symlinked .oaica-site")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Errorf("state was created outside the site directory: %v", entries)
	}

	// A state symlink pointing back INSIDE the tree is the same refusal — and
	// that is the shape that used to publish the brief, because Export no
	// longer recognised the resulting layout as the state dir.
	dir2 := t.TempDir()
	if err := os.Symlink(dir2, filepath.Join(dir2, StateDir)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(dir2); err == nil {
		t.Error("Save followed a self-referential state symlink")
	}
}

// L6: Export resolves a symlinked root instead of walking it as a file.
func TestExportResolvesASymlinkedRoot(t *testing.T) {
	_, dir := lifecycleSite(t)
	link := filepath.Join(t.TempDir(), "live")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Export(link, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, IndexFile)); err != nil {
		t.Errorf("Export(symlinked root) returned nil and copied nothing — Deploy then prints a *.pages.dev URL for an empty upload: %v", err)
	}
	// the trailing-slash spelling (what shell completion produces) agrees
	dst2 := t.TempDir()
	if err := Export(link+"/", dst2); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadDir(dst)
	b, _ := os.ReadDir(dst2)
	if len(a) != len(b) {
		t.Errorf("the same site exports differently with a trailing slash: %d vs %d entries", len(a), len(b))
	}
	// and a root that is not a directory is refused rather than silently empty
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Export(f, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Error("Export accepted a non-directory root")
	}
}

// L7: Deploy refuses to publish a tree with no index.html — through Deploy
// itself, with the wrangler CLI stubbed, so removing the check fails the test.
func TestAnUploadWithNoIndexIsRefused(t *testing.T) {
	restorePath, restoreRun := lookPath, runWrangler
	defer func() { lookPath, runWrangler = restorePath, restoreRun }()
	lookPath = func(string) (string, error) { return "/usr/bin/wrangler", nil }
	uploaded := 0
	runWrangler = func(context.Context, io.Writer, ...string) (string, error) {
		uploaded++
		return "✨ Deployed https://abc.pages.dev", nil
	}

	// A site whose index.html is missing: the export is not a website.
	_, dir := lifecycleSite(t)
	if err := os.Remove(filepath.Join(dir, IndexFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Deploy(context.Background(), dir, "proj", io.Discard); err == nil {
		t.Error("Deploy published a directory with no index.html and reported success — wrangler uploads an empty site and prints a URL")
	}
	if uploaded != 0 {
		t.Errorf("wrangler was invoked %d time(s) for an empty site", uploaded)
	}

	// The happy path still deploys and returns the URL.
	_, dir2 := lifecycleSite(t)
	url, err := Deploy(context.Background(), dir2, "proj", io.Discard)
	if err != nil {
		t.Fatalf("deploying a complete site failed: %v", err)
	}
	if url != "https://abc.pages.dev" {
		t.Errorf("url = %q", url)
	}
	if uploaded == 0 {
		t.Error("wrangler was never invoked for a deployable site")
	}
}

// L8: names that say "credential" are never published, however they are spelt.
func TestExportDoesNotPublishSecretShapedNames(t *testing.T) {
	_, dir := lifecycleSite(t)
	bad := []string{"secrets.txt", "token.json", "api-keys.txt", "api_keys.txt", "credentials.txt",
		"deploy.sh", "wrangler.toml", "keystore.keystore", "certificate.crt", "env.local",
		"auth.json", "site.json"}
	for _, n := range bad {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a legitimate asset still goes up
	for _, n := range []string{"robots.txt", "favicon.svg", "assets/style.css"} {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dst := t.TempDir()
	if err := Export(dir, dst); err != nil {
		t.Fatal(err)
	}
	var published []string
	for _, n := range bad {
		if _, err := os.Stat(filepath.Join(dst, n)); err == nil {
			published = append(published, n)
		}
	}
	if len(published) > 0 {
		t.Errorf("these were uploaded to a public host: %v — Export's contract is a whitelist-shaped decision, and a name that says it holds a credential is not publishable content", published)
	}
	for _, n := range []string{"robots.txt", "favicon.svg", "assets/style.css"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Errorf("a legitimate asset was not exported: %s (%v)", n, err)
		}
	}
	// and nothing from the state directory ever leaves
	if _, err := os.Stat(filepath.Join(dst, StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the state directory was exported")
	}
}
