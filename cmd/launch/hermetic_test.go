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
//
// Tests that need any of these still opt in explicitly (writeRemotes,
// writeDescriptorRemotesFile, t.Setenv(zaiEnvKey, ...), t.Setenv("OAICA_HOST",
// srv.URL) or "" for local-server discovery); t.Setenv restores to these masked
// values afterwards, so the guard holds across the run.
func hermeticTestEnv() {
	os.Setenv("OAICA_REMOTES_FILE", filepath.Join(os.TempDir(), "oaica-launch-tests", "no-remotes.json"))
	os.Setenv(zaiEnvKey, "")
	os.Setenv(openrouterEnvKey, "")
	os.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	os.Setenv("OAICA_API_KEY", "")
}
