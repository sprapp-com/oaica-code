package sitebuilder

import (
	"os"
	"path/filepath"
	"testing"
)

// A site directory from a clone/backup whose .oaica-site is a real directory but whose sections/ is a
// symlink: checkStateDir checks only .oaica-site, so Save's stale-fragment cleanup deletes the files of
// whatever directory sections/ names, and writes the fragments there.
func TestRound127SectionsSymlinkCannotSteerSave(t *testing.T) {
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, "thesis.docx"), []byte("years of work"), 0o644)
	os.WriteFile(filepath.Join(victim, "id_ed25519"), []byte("key"), 0o600)
	site := t.TempDir()
	os.MkdirAll(filepath.Join(site, StateDir), 0o755)
	if err := os.Symlink(victim, filepath.Join(site, StateDir, "sections")); err != nil {
		t.Fatal(err)
	}
	s := &Site{Spec: Spec{Title: "t", Sections: []Section{{ID: "hero"}}}, Fragments: map[string]string{"hero": "<section id=\"hero\">hi</section>"}}
	err := s.Save(site)
	ents, _ := os.ReadDir(victim)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	t.Logf("Save err=%v; victim dir now holds %q", err, names)
	if _, e := os.Stat(filepath.Join(victim, "thesis.docx")); e != nil {
		t.Errorf("Save deleted a file outside the site tree through a symlinked sections/ directory")
	}
}

func TestRound127LoadRefusesASymlinkedSections(t *testing.T) {
	site := t.TempDir()
	os.MkdirAll(filepath.Join(site, StateDir), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(site, StateDir, "sections"))
	if _, err := Load(site); err == nil {
		t.Errorf("Load read a site whose sections/ is a symlink")
	}
}
