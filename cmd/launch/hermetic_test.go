package launch

import (
	"os"
	"path/filepath"
)

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
	for _, p := range providerCatalog() {
		if p.APIKeyEnv == "" {
			continue
		}
		os.Setenv(p.APIKeyEnv, "")
	}
	os.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	os.Setenv("OAICA_API_KEY", "")
	os.Setenv("OAICA_AUTH_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-auth.json"))
	os.Setenv("OPENCODE_AUTH_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-opencode-auth.json"))
}
