package launch

// vscode_null_prefs_integrity_test.go — a `chatModelPickerPreferences` value of
// the literal JSON `null` panicked the launch (2026-09-27 audit, round 21).
//
// ShowInModelPicker starts from `prefs := make(map[string]bool)` and unmarshals
// the stored value over it with the error discarded. A stored `null` decodes
// into a NIL map, so the first `prefs[id] = true` in the loop below it aborts
// the whole `oaica launch vscode` with "assignment to entry in nil map". Same
// class as the document-null readers round 19 fixed through decodeJSONObject
// (json_document.go); this one is a plain json.Unmarshal into a map, so it was
// missed.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVSCodeShowInModelPickerSurvivesNullPreferences(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	t.Setenv("XDG_CONFIG_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", tmpDir)
	}

	dbPath := testVSCodePath(t, tmpDir, filepath.Join("globalStorage", "state.vscdb"))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)"); err != nil {
		t.Fatal(err)
	}
	// The literal null, not a marshalled empty map.
	if _, err := db.Exec("INSERT INTO ItemTable (key, value) VALUES ('chatModelPickerPreferences', ?)", "null"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	v := &VSCode{}
	if err := v.ShowInModelPicker([]string{"llama3.2"}); err != nil {
		t.Fatalf("ShowInModelPicker: %v", err)
	}

	db, err = sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = 'chatModelPickerPreferences'").Scan(&raw); err != nil {
		t.Fatalf("read preferences back: %v", err)
	}
	prefs := make(map[string]bool)
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
		t.Fatalf("stored preferences are not an object: %v\n%s", err, raw)
	}
	if !prefs["ollama/Ollama/llama3.2"] {
		t.Errorf("the model was not shown: %s", raw)
	}
}
