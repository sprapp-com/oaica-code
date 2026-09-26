package launch

// omp_remote_model_row_integrity_test.go — a remote model was written into
// OMP's models.yml twice, the second copy stripped of everything oaica does
// not model (2026-09-26 audit, twelfth round).
//
// writeOMPModelsConfig merges the models it is asked for into whatever
// provider entry already exists, so a field oaica does not know about (one
// OMP added, or one the user set by hand) survives a reconfigure. Both halves
// of that merge are keyed on a map of the existing entries by their `id` —
// and the id an entry carries is the upstream id (`ompModelConfig` sets
// `id = ep.UpstreamModel` for a user remote), while the LOOKUP was done by
// the model's picker name (`big-model` vs `box/big-model`). The lookup never
// matched:
//
//   - the existing entry was not found, so nothing was preserved — the
//     reconfigure rebuilt the row from scratch and silently dropped every
//     field oaica does not model;
//   - and because the "carry over the entries we did not name" sweep also
//     compares against the picker name, the old row was not recognised as
//     ours, so it was appended after the new one: the provider ends up with
//     TWO rows both saying `id: big-model`, one of them stale.
//
// The row is what OMP shows its user and what it sends as the model id, so a
// duplicate is not cosmetic — it is the same model listed twice, differing in
// exactly the fields the merge lost.

import (
	"os"
	"strings"
	"testing"
)

// The shape an OMP models.yml has after one oaica launch of a remote row:
// the ollama provider holds the upstream id, plus a field oaica does not
// model (so the preservation half has something to lose) and a contextWindow
// the user may have raised.
const ompRemoteRowFixture = `providers:
  ollama:
    baseUrl: https://box.example/v1
    api: openai-completions
    auth: apiKey
    apiKey: sk-box-SECRET-1234567890
    models:
      - id: big-model
        name: big-model
        input:
          - text
        contextWindow: 200000
        customNote: keep-me
topLevelNote: also-keep
`

func TestOMPRemoteModelIsNotDuplicatedAndKeepsUnmodeledFields(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so the model would not be translated to an upstream id at all")
	}
	path := ompModelsFixturePath(t, home)
	if err := os.WriteFile(path, []byte(ompRemoteRowFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second launch of the same remote row, as `oaica launch omp` does.
	if err := writeOMPModelsConfig("box/big-model", testLaunchModels("box/big-model")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	if n := strings.Count(text, "id: big-model"); n != 1 {
		t.Errorf("the ollama provider holds %d row(s) with id big-model after one reconfigure, want 1 — the merge looks existing entries up by the model's PICKER name (box/big-model) while they are keyed by the upstream id, so the old row is neither found nor recognised as ours and is appended next to the new one:\n%s", n, text)
	}
	if !strings.Contains(text, "customNote: keep-me") {
		t.Errorf("customNote was dropped by a reconfigure: the same key mismatch meant the existing entry was never found, so the row was rebuilt from scratch and every field oaica does not model was silently lost:\n%s", text)
	}
	if !strings.Contains(text, "contextWindow: 200000") {
		t.Errorf("the existing contextWindow was dropped too — it is only rewritten when the model info carries one:\n%s", text)
	}
	// The control: things the fixture holds that are nobody's business of
	// ours must still be there.
	if !strings.Contains(text, "topLevelNote: also-keep") {
		t.Errorf("topLevelNote did not survive:\n%s", text)
	}
}

// Control: a LOCAL (daemon) model has no upstream translation — picker name
// and stored id are the same string — so it must keep working exactly as
// before, including preserving unmodeled fields.
func TestOMPLocalModelStillMergesByItsOwnName(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	home := t.TempDir()
	setOMPTestHome(t, home)
	path := ompModelsFixturePath(t, home)
	if err := os.WriteFile(path, []byte("providers:\n  ollama:\n    models:\n      - id: kat-awq\n        customNote: keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeOMPModelsConfig("kat-awq", testLaunchModels("kat-awq")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "id: kat-awq"); n != 1 {
		t.Errorf("%d rows with id kat-awq after one reconfigure, want 1:\n%s", n, got)
	}
	if !strings.Contains(string(got), "customNote: keep-me") {
		t.Errorf("customNote was dropped for a local model:\n%s", got)
	}
}
