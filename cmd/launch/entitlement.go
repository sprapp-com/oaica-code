package launch

// entitlement.go — a hook point for gating proxy requests on some future
// license/entitlement check, wired into RunAnthropicOpenAIProxyRoutes but
// OFF BY DEFAULT and a no-op until a real check is plugged in. This file
// makes ZERO business decisions (no pricing, no license terms, no key
// format) — it only answers "where would that check live and how would it
// deny a request" so the actual policy can be added later without another
// pass through the proxy's request-handling code.
//
// Why here, not somewhere else: every request to a self-hosted or user-remote
// model passes through POST /v1/messages in RunAnthropicOpenAIProxyRoutes,
// after routing is resolved (route, reqModel) but before the upstream call is
// made — so a denial never spends upstream GPU time, and the check sees
// exactly what a real entitlement decision would need: which model, which
// route/backend label. "Every" means every leg a request can LEAVE for a
// remote on, and each is judged by the label that SERVES it: the
// OpenAI-translated path, the Anthropic-wire remote in the NativePassthrough
// branch, and both oversize-crossover legs (route_policy.go's oversizeSwap,
// which re-points the request at a different backend late in the handler) —
// with the same exclusion on the crossover as on the primary path: a native
// claude/* leg is not gated there either, which the crossover got wrong until
// the 2026-09-26 audit's ninth round (an armed gate denied an oversize
// crossover onto the native tier that the primary path would have served).
// The native claude/* leg is deliberately NOT gated — it is
// api.anthropic.com under the user's own credential, so it is neither
// self-hosted nor user-remote, and the rule above is about the requests that
// spend an OAICA-side or self-hosted leg. Each of those three omissions was
// found by audit, in turn: the translated path alone left an Anthropic-wire
// remote — exactly the self-hosted class named here — ungated (2026-09-26
// audit: a deny-all gate armed let a zai-coding-plan request reach its
// upstream), and the crossover then served a leg the gate had judged under
// the PRE-swap label (same audit: a gate denying the oversize label still
// spent its upstream).
//
// Enable/disable: entitlementCheckEnabled defaults to false. Turning it on
// without also setting entitlementCheckFn to something real would deny
// every request (see denyAllEntitlementCheck) — that combination exists
// only so a future integration test can prove the gate actually blocks
// when armed, not as a usable default.

import (
	"net/http"
	"os"
)

// EntitlementDecision is what an entitlement check returns for one request.
type EntitlementDecision struct {
	Allowed bool
	// Reason is shown to the caller (as the Anthropic error message) when
	// Allowed is false. Keep it short and non-sensitive — it crosses the
	// wire to whatever's driving Claude Code.
	Reason string
}

// EntitlementCheckFn decides whether one request may proceed. route.Label
// and reqModel are exactly what tier_routing.go/proxyRouteTable already
// compute — no new data threaded through just for this. r's headers are
// available for a future bearer-token/license-key scheme; nothing reads
// them yet.
type EntitlementCheckFn func(r *http.Request, routeLabel, reqModel string) EntitlementDecision

// entitlementCheckEnabled gates whether RunAnthropicOpenAIProxyRoutes calls
// entitlementCheckFn at all. False by default — this repo ships with no
// license/entitlement product decision made, so the gate must be inert
// until one exists. Also settable via OAICA_ENTITLEMENT_CHECK=1 so a future
// deployment can flip it without a code change once entitlementCheckFn is
// replaced with something real.
var entitlementCheckEnabled = os.Getenv("OAICA_ENTITLEMENT_CHECK") == "1"

// entitlementCheckFn is the active check. allowAllEntitlementCheck is the
// package default (used whenever entitlementCheckEnabled somehow becomes
// true with nothing else configured) — it allows everything, so simply
// flipping the env var never breaks an existing deployment by accident.
// A real integration replaces this var (same pattern as
// remoteContextWindowFn/contextWindowFromManifest elsewhere in this
// package) rather than editing the proxy handler.
var entitlementCheckFn EntitlementCheckFn = allowAllEntitlementCheck

func allowAllEntitlementCheck(r *http.Request, routeLabel, reqModel string) EntitlementDecision {
	return EntitlementDecision{Allowed: true}
}

// denyAllEntitlementCheck exists for tests proving the gate actually blocks
// when armed with a real check — never assigned to entitlementCheckFn by
// production code.
func denyAllEntitlementCheck(r *http.Request, routeLabel, reqModel string) EntitlementDecision {
	return EntitlementDecision{Allowed: false, Reason: "entitlement check denied this request"}
}

// checkEntitlement is what RunAnthropicOpenAIProxyRoutes calls. Returns
// (true, "") when the gate is disabled or the active check allows the
// request; (false, reason) when it should be denied with reason as the
// error message.
func checkEntitlement(r *http.Request, routeLabel, reqModel string) (bool, string) {
	if !entitlementCheckEnabled {
		return true, ""
	}
	d := entitlementCheckFn(r, routeLabel, reqModel)
	return d.Allowed, d.Reason
}
