package launch

// remote_wire_flip_tool_format_test.go — `--wire` flip and the row's tool_format
// (2026-09-26 audit, ninth round, auditor B — raised PLAUSIBLE, verified here as
// INTENDED, and pinned so the next reader does not have to re-derive it).
//
// The concern: `remote add x --wire anthropic` preserves an existing
// tool_format, so a row can end up Wire=anthropic with ToolFormat=tool_calls —
// the OLD wire's inferred default.
//
// Two facts decide it:
//
//   - The preserve rule is the documented one (RemoteAddOptions.ToolFormatSet):
//     tool_format's default is INFERRED from the wire, so clearing it on an
//     edit that never mentioned the flag would revert a deliberately pinned
//     protocol ("freeform"/"none" on a model that cannot hold a tool_use loop)
//     back to tool_calls — the exact pairing the capability gate exists to
//     refuse. The reset is typeable (`--tool-format tool_calls`), so it has to
//     be meant.
//   - anthropic + tool_calls is not a contradiction: it is what this codebase
//     hardcodes for its own native-Anthropic leg (tier_routing.go's
//     "native-anthropic", ToolFormat "tool_calls", ToolReliable true). On that
//     wire tool_calls means "tool use is reliable", which for a native
//     passthrough leg it is.
//
// A typeless flip is not affected at all: a row that never named a tool_format
// stores an empty one, and an empty one infers from the NEW wire.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A flip that never named a tool_format leaves the inference to the new wire.
func TestAWireFlipWithoutAToolFormatInfersFromTheNewWire(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "flip", BaseURL: "https://api.example.com", Wire: "openai", WireSet: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "flip", BaseURL: "https://api.example.com", Wire: "anthropic", WireSet: true}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	r, err := RemoteShow("flip")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(r.ToolFormat) != "" {
		t.Fatalf("the flip stored tool_format %q without the flag ever being passed", r.ToolFormat)
	}
	if d := r.Descriptor(); d.Wire != "anthropic" || d.ToolFormat != "xml" {
		t.Errorf("after the flip the row resolves to wire=%q tool_format=%q, want anthropic/xml — a tool_format nobody typed must follow the new wire's inference", d.Wire, d.ToolFormat)
	}
	raw, err := os.ReadFile(remotesPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Remotes []struct {
			Name string `json:"name"`
			Wire string `json:"wire"`
		} `json:"remotes"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("the file is not JSON: %v\n%s", err, raw)
	}
	if len(onDisk.Remotes) != 1 || onDisk.Remotes[0].Wire != "anthropic" {
		t.Errorf("the flip did not reach the file:\n%s", raw)
	}
}

// The documented preserve rule, pinned: an explicitly typed tool_format
// survives an edit that says nothing about it — including a wire flip — and the
// reset is available by typing it.
func TestATypedToolFormatSurvivesAWireFlip(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "pin", BaseURL: "https://api.example.com",
		Wire: "openai", WireSet: true,
		ToolFormat: "tool_calls", ToolFormatSet: true,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "pin", BaseURL: "https://api.example.com", Wire: "anthropic", WireSet: true}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	r, err := RemoteShow("pin")
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolFormat != "tool_calls" {
		t.Errorf("tool_format = %q after a flip that did not mention it, want the typed value tool_calls — clearing it would revert a deliberate protocol to the wire's inference", r.ToolFormat)
	}
	// The reset has to be typeable.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "pin", BaseURL: "https://api.example.com", ToolFormat: "none", ToolFormatSet: true}); err != nil {
		t.Fatalf("typed reset: %v", err)
	}
	if r, err = RemoteShow("pin"); err != nil {
		t.Fatal(err)
	}
	if r.ToolFormat != "none" {
		t.Errorf("tool_format = %q after `--tool-format none`, want none — the reset must be available to whoever meant it", r.ToolFormat)
	}
}
