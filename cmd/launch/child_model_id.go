package launch

// child_model_id.go — the model id a child CLI is given (2026-09-27 audit,
// round 22).
//
// A launch target can be a picker spelling rather than a name the backend
// knows. `--model` reaches the writers VERBATIM (only picker selections go
// through launchNamesForPickerSelections), and readiness accepts all of them —
// a user alias (resolveModelAlias), and the source prefixes resolveLaunchEndpoint
// strips for its own lookup ("ollama/", "daemon/", "router/", "oaica/"). The
// child id functions resolved only the remote case, so
// `oaica launch pool --model ollama/llama3.2` handed the child "-m
// ollama/llama3.2" against the daemon, and an alias pointing at a cloud or
// remote model handed it the alias name: the launch reported success and the
// first inference 404'd (a remote alias additionally kept the daemon endpoint).
// Whatever this returns is also what gets saved, so the wrong spelling was
// reused by every later launch.

import "strings"

// resolveLaunchTargetEndpoint is resolveRemoteEndpoint for a LAUNCH TARGET: a
// `--model` spelling may be a user alias, and resolveLaunchEndpoint resolves
// aliases before it asks anything else, so every writer that asks "is this
// target a remote's?" must see the alias's own target. Without it an alias
// pointing at a remote kept the DAEMON's base URL and the daemon's (absent)
// credential while the model ran on the remote — the endpoint half of the same
// hole childModelIDFor closes (2026-09-27 audit, round 22).
func resolveLaunchTargetEndpoint(model string) (RemoteEndpoint, bool) {
	model = strings.TrimSpace(model)
	if target, ok := resolveModelAlias(model); ok {
		model = strings.TrimSpace(target)
	}
	return resolveRemoteEndpoint(model)
}

// childModelIDFor is the model id to hand a child CLI for a launch target:
// the user's alias resolved first, the remote's own upstream id when a
// configured remote serves it, and otherwise the target with its oaica-only
// source prefix removed — the same strip resolveLaunchEndpoint applies before
// it looks the row up. A name that is already a backend id is returned as-is.
func childModelIDFor(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if target, ok := resolveModelAlias(model); ok {
		model = strings.TrimSpace(target)
	}
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	if i := strings.Index(model, "/"); i > 0 && hasSourcePrefix(model) {
		return model[i+1:]
	}
	return model
}
