package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storePathEnvVars are the OAICA_*_FILE overrides every store resolves BEFORE
// it falls back to HOME. Each one has to be masked here: a developer or CI box
// that exports one makes the whole suite run against that file — and the store
// tests write, so the suite destroys it. The three missing from this list until
// 2026-09-26 (OAICA_MODELS_FILE, OAICA_PLANS_FILE, OAICA_ALIASES_FILE) were
// exactly the stores whose tests write most: an exported OAICA_MODELS_FILE had
// `go test ./cmd/launch/` overwrite the file with test content and fail ten
// tests, and an exported OAICA_PLANS_FILE wedged the run.
var storePathEnvVars = []string{
	"OAICA_REMOTES_FILE",
	"OAICA_AUTH_FILE",
	"OAICA_MODELS_FILE",
	"OAICA_PLANS_FILE",
	"OAICA_ALIASES_FILE",
	"OPENCODE_AUTH_FILE",
}

// TestEveryStoreOverrideIsMaskedForTests is the guard on the list above: while
// a test binary is running, every one of these must be SET (to a path inside
// the hermetic directory, or to the empty string, which the stores read as
// "not set" and resolve through the hermetic HOME instead) — a developer or CI
// box that exports one has the whole suite read and WRITE that file, and the
// store tests save, so an exported path is a config file destroyed by
// `go test`.
func TestEveryStoreOverrideIsMaskedForTests(t *testing.T) {
	hermetic := filepath.Join(os.TempDir(), "oaica-launch-tests")
	for _, env := range storePathEnvVars {
		v, set := os.LookupEnv(env)
		if !set {
			t.Errorf("%s is not masked for tests: a developer or CI box that exports it has the whole suite read and WRITE that file — the store tests save, so an exported path is a config file destroyed by `go test`", env)
			continue
		}
		if v != "" && !strings.HasPrefix(v, hermetic) {
			t.Errorf("%s = %q, which is outside the hermetic test directory %s — every store path the suite can write must live there, or be empty so the store resolves through the hermetic HOME", env, v, hermetic)
		}
	}
}

// hermeticTestEnv is called from TestMain before any test runs. It keeps the
// whole test binary off the developer's own configuration and off the real
// network:
//
//   - OAICA_REMOTES_FILE → a path that does not exist. Without it, every test
//     that resolves a bare model name (codex/copilot/kimi/opencode helpers,
//     showOrPull, ResolveAgentModel, ...) without setting the var reads the
//     real ~/.oaica/remotes.json and sweeps each configured box's /v1/models —
//     real traffic to real hosts from a unit test, and up to fetchRemoteModels'
//     6s timeout per unreachable one.
//   - Z_AI_API_KEY / OPENROUTER_API_KEY → empty, so builtinRemotes() is empty.
//   - OAICA_HOST → a loopback port nothing listens on. showOrPull and the
//     inventory call the router's /v1/models (oaicaFetchCloudModelEntries);
//     with the var unset that is api.oaica.com, reached with the developer's
//     OAICA_API_KEY. A dead port keeps the live code path (it fails as "router
//     unreachable", the same fail-open branch an offline CI box exercises)
//     without leaving the machine.
//   - OAICA_API_KEY → empty, so no test's output depends on a shell key.
//   - OAICA_AUTH_FILE → a path that does not exist, so a credential stored by
//     `oaica auth login` on the developer's box cannot make a
//     gate-on-no-credential assertion pass for the wrong reason.
//   - OPENCODE_AUTH_FILE → the same, for the external-store reuse chain
//     (auth_external.go): without it, a developer who has ever run `opencode
//     auth login` would see the auth_via catalog rows appear in every picker
//     test, and a gate assertion would pass for a reason the test never set
//     up. Tests that exercise reuse point it at their own fixture.
//
// Tests that need any of these still opt in explicitly (writeRemotes,
// writeDescriptorRemotesFile, t.Setenv("Z_AI_API_KEY", ...), t.Setenv(
// "OAICA_HOST", srv.URL) or "" for local-server discovery); t.Setenv restores
// to these masked values afterwards, so the guard holds across the run.
func hermeticTestEnv() {
	os.Setenv("OAICA_REMOTES_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-remotes.json"))
	// Every provider catalog entry (anthropic, openai, deepseek, zai,
	// zai-coding-plan, ...) keys off its own env var — a dev box exporting a
	// dozen of them would otherwise leak catalog remotes into every picker
	// test. Data-driven (provider_catalog.go), so this loop covers new
	// providers/plans automatically — nothing to add here when one ships.
	//
	// Each NAME is masked, not the row's api_key_env string: a row may name
	// more than one variable ("OPENCODE_API_KEY, OPENCODE_GO_API_KEY" — see
	// keyEnvNames), and setting the comma-joined string as a single variable
	// left both real ones untouched. A box that exports one of them then
	// leaked a live credential into any test that walks the credential list
	// (reportSecrets), which is how the leak was found.
	for _, p := range providerCatalog() {
		for _, env := range (userRemote{APIKeyEnv: p.APIKeyEnv}).keyEnvNames() {
			os.Setenv(env, "")
		}
	}
	os.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	os.Setenv("OAICA_API_KEY", "")
	os.Setenv("OAICA_AUTH_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-auth.json"))
	os.Setenv("OPENCODE_AUTH_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-opencode-auth.json"))
	// The three stores whose overrides were NOT masked, which is the same hole
	// stated three times: each resolves its env var before HOME, so a box that
	// exported one had the suite run against that file — and these are the
	// stores the suite WRITES. An exported OAICA_MODELS_FILE made `go test
	// ./cmd/launch/` replace the developer's models.json with test content and
	// fail ten tests; an exported OAICA_PLANS_FILE overwrote their plans and
	// hung the run.
	//
	// Masked to the EMPTY string, not to a path: the stores read an empty value
	// as "not set" and resolve through HOME, which TestMain has already
	// repointed at the hermetic directory. A path here would be a single store
	// shared by every test in the binary — and worse, it would follow the
	// process rather than the test, so a fixture that isolates itself by moving
	// HOME (withTempOaicaHome) would still write to the shared file. Empty
	// keeps every test's isolation exactly as it was, while a var exported by
	// the developer's shell can no longer reach any of them.
	for _, env := range []string{"OAICA_MODELS_FILE", "OAICA_PLANS_FILE", "OAICA_ALIASES_FILE"} {
		os.Setenv(env, "")
	}
	// The HuggingFace token is a credential this client transmits (`oaica pull`
	// attaches it) and one the support report therefore scans for — so it is
	// part of the ambient state a test must not inherit either.
	os.Setenv("HF_TOKEN", "")
}
