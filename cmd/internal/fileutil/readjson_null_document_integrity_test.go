package fileutil

// readjson_null_document_integrity_test.go — ReadJSON returned (nil, nil) for a
// document that is `null` (2026-09-27 audit, round 21, F15).
//
// Unmarshalling `null` into a map succeeds and leaves the map nil, so the
// helper handed out a nil map with a nil error: indistinguishable from a
// document that failed to load, and a map the first write would panic on. The
// empty document is now an empty map.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadJSONTurnsANullDocumentIntoAnEmptyMap(t *testing.T) {
	for _, content := range []string{"null", " null ", "\nnull\n"} {
		t.Run(strings.TrimSpace(content), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := ReadJSON(path)
			if err != nil {
				t.Fatalf("ReadJSON(%q) returned error %v, want an empty map", content, err)
			}
			if got == nil {
				t.Fatalf("ReadJSON(%q) returned a nil map with a nil error: a caller cannot tell it from a failed load, and writing to it panics", content)
			}
			if len(got) != 0 {
				t.Errorf("ReadJSON(%q) = %v, want an empty map", content, got)
			}
			// The property that matters: the map is writable.
			got["k"] = "v"
		})
	}
}

// TestReadJSONStillReportsAMissingFile is the control: the empty map above is
// for an empty DOCUMENT, not for an unreadable path.
func TestReadJSONStillReportsAMissingFile(t *testing.T) {
	got, err := ReadJSON(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatalf("ReadJSON on a missing file returned %v, want an error", got)
	}
	if got != nil {
		t.Errorf("ReadJSON on a missing file returned %v alongside its error, want nil", got)
	}
}
