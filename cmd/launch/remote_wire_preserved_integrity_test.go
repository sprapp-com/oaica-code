package launch

// remote_wire_preserved_integrity_test.go — `oaica remote add` on an existing
// row silently reset its wire and its tool format (2026-09-26 audit).
//
// Both fields have flags, so they followed the usual "absent means cleared"
// rule — but neither default is neutral, which is the argument Version already
// won for itself:
//
//   - Wire's default (openai) MOVES THE ENDPOINT. A vendor that speaks
//     Anthropic natively answers <base>/chat/completions with a 404 (the
//     2026-09-25 z.ai failure this proxy's passthrough exists for), so
//     `oaica remote add zai --base-url <new>` — a repoint with nothing to say
//     about the wire — turned a working row into a broken one and the
//     confirmation line did not mention it.
//   - ToolFormat's default is INFERRED from the wire, so clearing it reverts a
//     deliberate setting: a model pinned to "freeform" or "none" because it
//     cannot hold a tool_use loop went back to "tool_calls", the pairing the
//     capability gate exists to refuse.
//
// The reset is still available — it just has to be typed.

import "testing"

func TestRemoteAddPreservesWireAndToolFormatWhenTheFlagsAreAbsent(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "zai", BaseURL: "https://api.z.ai/api/anthropic",
		Wire: "anthropic", WireSet: true,
		ToolFormat: "xml", ToolFormatSet: true,
	}); err != nil {
		t.Fatalf("RemoteAdd: %v", err)
	}

	// A repoint that says nothing about the wire or the tool format.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "zai", BaseURL: "https://api.z.ai/api/anthropic/v2"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	f, _, err := loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != 1 {
		t.Fatalf("re-add should replace, not append: %+v", f.Remotes)
	}
	got := f.Remotes[0]
	if got.BaseURL != "https://api.z.ai/api/anthropic/v2" {
		t.Fatalf("base_url not updated: %+v", got)
	}
	if got.Wire != "anthropic" {
		t.Errorf("wire = %q after an edit that never mentioned it, want anthropic — the default is not neutral, it moves every request to <base>/chat/completions, which an Anthropic-wire vendor 404s", got.Wire)
	}
	if got.ToolFormat != "xml" {
		t.Errorf("tool_format = %q after an edit that never mentioned it, want xml — clearing reverts to what the wire implies, which for a model pinned away from tool_calls is the loop the capability gate refuses", got.ToolFormat)
	}

	// ...and the reset is still typeable, without disturbing the other field.
	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "zai", BaseURL: "https://api.z.ai/api/anthropic/v2", Wire: "openai", WireSet: true,
	}); err != nil {
		t.Fatalf("re-add with --wire: %v", err)
	}
	f, _, err = loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	if f.Remotes[0].Wire != "openai" {
		t.Errorf("wire = %q after --wire openai, want openai — the reset must stay available, it just has to be meant", f.Remotes[0].Wire)
	}
	if f.Remotes[0].ToolFormat != "xml" {
		t.Errorf("tool_format = %q after an edit that passed only --wire, want xml untouched — one flag must not reset another", f.Remotes[0].ToolFormat)
	}
}
