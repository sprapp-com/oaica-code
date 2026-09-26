package cmd

// remote_add_flag_preserve_integrity_test.go — the CLI half of the
// `oaica remote add` reset (2026-09-26 audit).
//
// launch.RemoteAdd only preserves a flag-bearing field when the caller says the
// flag was passed, so the wiring from cobra's Changed() is load-bearing: a
// missing WireSet/ToolFormatSet turns the preserve rule back into a silent
// reset, and a source-level assertion that the fields exist would not notice.
// This drives the real command twice through NewCLI and reads what the second
// one wrote.

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRemoteAddKeepsWireAndToolFormatThroughTheCLI(t *testing.T) {
	path := t.TempDir() + "/remotes.json"
	t.Setenv("OAICA_REMOTES_FILE", path)

	run := func(args ...string) {
		t.Helper()
		root := NewCLI()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("`oaica %s` failed: %v", strings.Join(args, " "), err)
		}
	}
	run("remote", "add", "zai", "--base-url", "https://api.z.ai/api/anthropic",
		"--wire", "anthropic", "--tool-format", "xml")

	// A repoint that says nothing about the wire: `oaica remote add zai
	// --base-url <new>`.
	run("remote", "add", "zai", "--base-url", "https://api.z.ai/api/anthropic/v2")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("premise: the command wrote no remotes.json: %v", err)
	}
	var f struct {
		Remotes []struct {
			Name       string `json:"name"`
			BaseURL    string `json:"base_url"`
			Wire       string `json:"wire"`
			ToolFormat string `json:"tool_format"`
		} `json:"remotes"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("remotes.json is not JSON: %v\n%s", err, b)
	}
	if len(f.Remotes) != 1 {
		t.Fatalf("re-add should replace, not append: %s", b)
	}
	got := f.Remotes[0]
	if got.BaseURL != "https://api.z.ai/api/anthropic/v2" {
		t.Fatalf("premise: the repoint did not take: %s", b)
	}
	if got.Wire != "anthropic" {
		t.Errorf("`oaica remote add zai --base-url <new>` left wire = %q, want anthropic — the flag was not passed, and its default moves every request to <base>/chat/completions, which this vendor 404s", got.Wire)
	}
	if got.ToolFormat != "xml" {
		t.Errorf("`oaica remote add zai --base-url <new>` left tool_format = %q, want xml — the flag was not passed, and clearing it reverts to what the wire implies", got.ToolFormat)
	}

	// The preserved value has to be REPLACEABLE, or "preserve unless asked"
	// degrades into "cannot be changed": this is the half cobra's Changed()
	// owns (see RemoteAddOptions.WireSet).
	run("remote", "add", "zai", "--base-url", "https://api.z.ai/api/anthropic/v2",
		"--wire", "openai", "--tool-format", "tool_calls")
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("remotes.json is not JSON: %v\n%s", err, b)
	}
	got = f.Remotes[0]
	if got.Wire != "openai" {
		t.Errorf("`--wire openai` left wire = %q, want openai — the flag was passed and ignored, which is a worse failure than the reset it replaced", got.Wire)
	}
	if got.ToolFormat != "tool_calls" {
		t.Errorf("`--tool-format tool_calls` left tool_format = %q, want tool_calls", got.ToolFormat)
	}
}
