package launch

// pi_context_window_survives_reconfigure_integrity_test.go — Pi.Edit decides
// whether a _launch entry it is asked to register again can be kept as-is or
// must be rebuilt, and the question it asks is hasContextWindow
// (cmd/launch/pi.go). Rebuilding is the destructive arm: createConfig writes
// the handful of members oaica models, so an entry that carried the window the
// router reported (or any key Pi understands and oaica does not) comes back
// without them.
//
// The document hasContextWindow inspects is read by readPiJSONDocument, which
// decodes with dec.UseNumber() (pi.go:606, landed 2026-09-26 in 64640c6c). A
// JSON number then arrives as json.Number — never as float64/int/int64 — so
// every entry read back from disk answered "no window", and a cloud entry that
// had one was rebuilt on every launch: its contextWindow replaced by whatever
// the catalog says, and every unmodeled member deleted.
//
// The window asserted here is deliberately NOT the catalog's number for the
// alias: the package prefers a live number over the catalog (see
// router_window_integrity_test.go), so a kept entry must come back with the
// window the file states, verbatim.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPiEditKeepsAContextWindowedLaunchEntry(t *testing.T) {
	// createConfig asks the daemon about capabilities; nothing here needs one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprintf(w, `{"capabilities":[],"model_info":{}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	home := t.TempDir()
	setLaunchTestHome(t, home)

	configDir := filepath.Join(home, ".pi", "agent")
	configPath := filepath.Join(configDir, "models.json")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The alias is one lookupCloudModelLimit knows a catalog window for, which
	// is exactly the entry the rebuild arm targets. The file's own window
	// differs from that catalog entry, and two members are ones oaica does not
	// model at all, so "was this entry kept?" is answerable from the output.
	const (
		alias         = "kimi-k2.6:cloud"
		fileWindow    = "200000"
		seedPiOnlyKey = "piOnlyField"
		seedPiOnlyVal = "keep-me"
		seedOutputKey = "maxOutputTokens"
		seedOutputVal = "8192"
	)
	seed := `{
  "providers": {
    "ollama": {
      "baseUrl": "http://127.0.0.1:11434/v1",
      "api": "openai-completions",
      "apiKey": "ollama",
      "models": [
        {
          "id": "` + alias + `",
          "_launch": true,
          "input": ["text"],
          "contextWindow": ` + fileWindow + `,
          "` + seedOutputKey + `": ` + seedOutputVal + `,
          "` + seedPiOnlyKey + `": "` + seedPiOnlyVal + `"
        }
      ]
    }
  }
}`
	if err := os.WriteFile(configPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Pi{}).Edit(launchModelsFromNames([]string{alias})); err != nil {
		t.Fatalf("Edit(%q) error = %v, want nil", alias, err)
	}

	// Read the published document with UseNumber too, so the assertion is on
	// the number the file holds rather than on a rounded float64.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading %s back: %v", configPath, err)
	}
	doc := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s does not parse after Edit: %v\n%s", configPath, err, data)
	}

	providers, _ := doc["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	list, _ := ollama["models"].([]any)

	var entry map[string]any
	matches := 0
	for _, m := range list {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := obj["id"].(string); id == alias {
			entry = obj
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("Edit wrote %d entries with id %q, want exactly one — the entry was dropped and rebuilt:\n%s", matches, alias, data)
	}

	// The window the file carried, not the catalog's (262144 for this alias):
	// hasContextWindow's answer is the "already refreshed" marker, so a
	// positive window means keep this entry — the rebuild is only for entries
	// written before oaica set one.
	if got := fmt.Sprint(entry["contextWindow"]); got != fileWindow {
		t.Errorf("contextWindow = %v, want %s — the entry was rebuilt from scratch, so the window the file stated was replaced by the catalog's", entry["contextWindow"], fileWindow)
	}
	if got := fmt.Sprint(entry[seedOutputKey]); got != seedOutputVal {
		t.Errorf("%s = %v, want %s — the entry was rebuilt from scratch and every member oaica does not model was deleted", seedOutputKey, entry[seedOutputKey], seedOutputVal)
	}
	if got, _ := entry[seedPiOnlyKey].(string); got != seedPiOnlyVal {
		t.Errorf("%s = %q, want %q — the entry was rebuilt from scratch and every member oaica does not model was deleted", seedPiOnlyKey, got, seedPiOnlyVal)
	}
}

// TestPiHasContextWindowReadsTheDecodedForm pins the helper directly: the form
// this file's documents actually arrive in is json.Number, and the concrete
// Go types are the ones another decoder would hand it. A false from the
// json.Number case is the defect, so this test names it without an Edit.
func TestPiHasContextWindowReadsTheDecodedForm(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"json.Number, as readPiJSONDocument decodes it", json.Number("262144"), true},
		{"json.Number past 2^53", json.Number("9007199254740993"), true},
		{"json.Number zero states nothing", json.Number("0"), false},
		{"json.Number negative is no window", json.Number("-1"), false},
		{"json.Number unparseable is no window", json.Number("not-a-number"), false},
		{"float64, from a plain decode", float64(262144), true},
		{"int, from a typed decode", int(262144), true},
		{"int64, from a typed decode", int64(262144), true},
		{"float64 zero", float64(0), false},
		{"absent means no window", nil, false},
		{"a non-numeric type is no window", "262144", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]any{}
			if tc.val != nil {
				cfg["contextWindow"] = tc.val
			}
			if got := hasContextWindow(cfg); got != tc.want {
				t.Errorf("hasContextWindow(contextWindow=%#v) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
