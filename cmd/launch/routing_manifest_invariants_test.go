package launch

// routing_manifest_invariants_test.go — regression pins for the model
// manifest and the routing decisions built from it: `oaica model sync`'s
// add/prune identity, the `--oversize` leg's context window as it reaches
// Fallbacks, and the window a same-remote split's legs are probed with
// (model_sync.go, model_manifest.go, tier_routing.go's buildTierPlan,
// context_window_remote.go's window stamping).
//
// Each test was written as a failing reproduction of one defect during the
// 2026-09-26 audit and now pins the fixed behaviour.
//
//	rtk proxy go test ./cmd/launch/ -run 'SyncPrune|OversizeFailover' -count=1
//
// Every test points HOME at a throwaway dir (setLaunchTestHome /
// withTempOaicaHome) and stubs the network seams the package already
// provides; the real ~/.oaica is never touched.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAuditR3BSyncPruneDeletesTheRowTheSameRunJustAdded
//
// Finding: `oaica model sync --prune` deletes the very entry the same run just
// added when the catalog's map key carries stray whitespace, so the row can
// never be installed and the report claims it both Added and Pruned that id.
//
// model_sync.go:150 trims the catalog key before storing (`id := TrimSpace(listed)`,
// round 2's fix for "a stray space in a hand-authored catalog produced a second
// entry for one model"), but the prune's membership test at model_sync.go:225
// still looks the UNTRIMMED key up in the catalog map:
//
//	if _, inCatalog := catalog.Models[id]; inCatalog {   // id is trimmed
//
// so "m-a " is present in the document, absent under the trimmed key, and the
// entry is removed the moment it was written.
func TestAuditR3BSyncPruneDeletesTheRowTheSameRunJustAdded(t *testing.T) {
	withTempOaicaHome(t)

	catalog := filepath.Join(t.TempDir(), "catalog.json")
	// One catalog row, keyed exactly as a hand-authored catalog with a stray
	// trailing space would be. The entry inside carries the trimmed id, i.e.
	// the document itself is unambiguous about which model it means.
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a ": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
	}})

	rep, err := ModelSync("file://"+catalog, true)
	if err != nil {
		t.Fatalf("ModelSync: %v", err)
	}

	// The claim: one sync must leave the catalog's row on disk.
	if _, ok := mustModelManifest(t).Models["m-a"]; !ok {
		t.Errorf("--prune deleted the entry the same ModelSync added: the add trims the catalog key, the prune looks the raw key up\n"+
			"  report.Added  = %v\n  report.Pruned = %v\n  (model_sync.go:150 vs model_sync.go:225)",
			rep.Added, rep.Pruned)
	}
	// And the report a user reads must not contradict itself.
	if containsStr(rep.Added, "m-a") && containsStr(rep.Pruned, "m-a") {
		t.Errorf("the same id is reported as both Added and Pruned by one run: added=%v pruned=%v", rep.Added, rep.Pruned)
	}

	// Worse shape of the same defect: a model the manifest ALREADY has (it was
	// installed from this catalog) is deleted by a catalog document that still
	// lists it — the only difference being that the document's key now carries
	// a stray space. Nothing was withdrawn; the model is running.
	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
	}})
	rep, err = ModelSync("file://"+catalog, false)
	if err != nil {
		t.Fatalf("ModelSync (install): %v", err)
	}
	if e, ok := mustModelManifest(t).Models["m-a"]; !ok || e.SourceURL != rep.URL {
		t.Fatalf("fixture: m-a was not installed from this catalog: %+v (err=%v)", e, err)
	}

	writeJSON(t, catalog, modelManifest{Version: 1, Models: map[string]ModelManifestEntry{
		"m-a ": {ID: "m-a", Engine: EngineVLLM, ContextWindow: 1048576},
	}})
	rep, err = ModelSync("file://"+catalog, true)
	if err != nil {
		t.Fatalf("ModelSync (prune): %v", err)
	}
	if _, ok := mustModelManifest(t).Models["m-a"]; !ok {
		t.Errorf("--prune deleted %q although the catalog still lists that model (the document's key is %q, trimmed to %q by the add loop): rep.Pruned=%v, rep.Updated=%v",
			"m-a", "m-a ", "m-a", rep.Pruned, rep.Updated)
	}
}

// TestAuditR3BOversizeFailoverCopyKeepsNoWindowSoTheClampIsSkipped
//
// Finding: when `--oversize <leg>` is wired in by Run (tier_routing.go:1388-1402),
// the leg's probed context window lands on plan.Routes.Oversize but NOT on the
// copy of that same leg that Run appends to plan.Routes.Fallbacks. That copy is
// the leg a request actually gets failed over to once the primary's breaker
// opens, and the proxy's context-fit clamp is gated on `route.ContextWindow > 0`
// (anthropic_openai_proxy.go:1243), so the failover request is forwarded
// unclamped to a leg whose own window cannot hold it — the exact class of
// already-doomed request the clamp exists to stop (2026-08-29 incident).
//
// This is the surviving instance of the defect round 2 fixed for the fallback
// copies buildTierPlan takes: context_window_remote.go:234-259 stamps
// p.Routes.Fallbacks entries matching the primary/secondary/haiku legs, but the
// --oversize copy is appended in Run AFTER buildTierPlan and before the probe
// call, and it matches none of those three legs, so nothing ever stamps it.
//
// The test builds the plan exactly the way Run does, then drives the real
// handler twice over the same table — once with the copy's window stamped (what
// the invariant demands) as a control, once as Run actually leaves it.
func TestAuditR3BOversizeFailoverCopyKeepsNoWindowSoTheClampIsSkipped(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})

	// The --oversize upstream: alive, enumerates its models (so the health
	// poll keeps its breaker closed) and records every completion it is asked
	// to serve.
	var hits, sawMaxTokens int
	oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"huge","context_length":262144}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var got struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(b, &got)
		hits++
		sawMaxTokens = got.MaxTokens
		// A vendor-neutral context-overflow shape (OpenAI's own
		// `context_length_exceeded`): NOT the vLLM wording
		// parseUpstreamContextOverflow matches, so the proxy passes it through
		// instead of re-emitting Anthropic's recoverable 400.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"too many tokens"}}`)
	}))
	defer oversize.Close()

	// The primary's box: down. It is the leg whose breaker opens.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	writeRemotes(t, fmt.Sprintf(`{"remotes":[
		{"name":"box","base_url":"%s/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"big","base_url":"%s/v1","api_key":"k","tool_format":"tool_calls"}]}`, deadURL, oversize.URL))

	origProbe := remoteContextWindowFn
	t.Cleanup(func() { remoteContextWindowFn = origProbe })
	remoteContextWindowFn = func(r proxyRoute) int {
		switch {
		case strings.HasPrefix(r.BaseURL, deadURL):
			return 32768
		case strings.HasPrefix(r.BaseURL, oversize.URL):
			return 262144
		}
		return 0
	}

	plan, err := buildTierPlan("box/small", "", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	auditR3BWireOversizeLikeRun(t, &plan, "big/huge")

	// Fixture: the --oversize leg's OWN route carries its probed window...
	if got := plan.Routes.Oversize.ContextWindow; got != 262144 {
		t.Fatalf("fixture: --oversize leg's route carries ContextWindow=%d, want its probed 262144", got)
	}
	// ...and the copy Run appended to Fallbacks is the leg a failover lands on.
	copyIdx := -1
	for i, f := range plan.Routes.Fallbacks {
		if f.BaseURL == plan.Routes.Oversize.BaseURL && f.UpstreamModel == plan.Routes.Oversize.UpstreamModel {
			copyIdx = i
		}
	}
	if copyIdx < 0 {
		t.Fatalf("fixture: Run's failover copy of the --oversize leg is not in plan.Routes.Fallbacks: %+v", plan.Routes.Fallbacks)
	}
	// The defect's own state: 0 at HEAD, the leg's window once the copy is
	// stamped. Not asserted either way — the two POST phases below decide,
	// and copyWindow is named in the failure so the diagnosis is immediate.
	copyWindow := plan.Routes.Fallbacks[copyIdx].ContextWindow

	// What Run does with the plan's route table (breakers are allocated per
	// launch in Run; the primary is the leg that is down).
	table := plan.Routes
	table.breakers = &routeBreakers{}
	for i := 0; i < breakerFailsToOpen; i++ {
		table.breakers.recordFail(table.Default.BaseURL)
	}
	if !table.breakers.open(table.Default.BaseURL) {
		t.Fatal("fixture: the primary's breaker did not open")
	}
	served, _, usedFallback := table.selectRoute("box/small")
	if !usedFallback || served.BaseURL != plan.Routes.Oversize.BaseURL {
		t.Fatalf("fixture: the request does not fail over onto the --oversize leg: %+v (fallback=%v)", served, usedFallback)
	}
	if served.ContextWindow != copyWindow {
		t.Fatalf("fixture: the served failover route carries ContextWindow=%d, but plan.Routes.Fallbacks[%d] carries %d", served.ContextWindow, copyIdx, copyWindow)
	}

	post := func(tbl proxyRouteTable) (int, string) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go RunAnthropicOpenAIProxyRoutes(ln, tbl)
		time.Sleep(50 * time.Millisecond)
		// ~920,580 chars => est ~230,145 tokens, margin 30% => ~299,188
		// against a 262,144 window: the request cannot fit that leg.
		body, _ := json.Marshal(map[string]any{
			"model":      "box/small",
			"max_tokens": 32000,
			"messages":   []map[string]any{{"role": "user", "content": strings.Repeat("x", 920580)}},
		})
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// Control — the same table with only the copy's window filled in, which is
	// what applyContextWindowsToRoutes' stamping loop does for every other
	// fallback copy. Now the clamp sees the leg's real window and rejects
	// client-side, never calling upstream.
	fixed := table
	fixed.Fallbacks = append([]proxyRoute(nil), table.Fallbacks...)
	fixed.Fallbacks[copyIdx].ContextWindow = 262144
	before := hits
	code, body := post(fixed)
	if hits != before {
		t.Fatalf("control: a request the leg's window cannot hold was forwarded upstream anyway even with the window known (status %d): %s", code, body)
	}
	if code != http.StatusBadRequest || !strings.Contains(body, "prompt is too long: ") {
		t.Fatalf("control: want a client-side 400 with Anthropic's recoverable wording, got %d: %s", code, body)
	}

	// As-built — Run's actual table. The same invariant must hold: the serving
	// leg's window is known (via its own route or via the copy), so a request
	// the leg cannot hold is rejected client-side. With the --oversize copy
	// left at ContextWindow 0 (copyWindow == 0, HEAD's state) the handler's
	// `route.ContextWindow > 0` gate is false, nothing clamps, and the doomed
	// request goes out.
	before = hits
	code, body = post(table)
	if hits > before {
		t.Errorf("the failover leg was forwarded a request its own probed window (262144) cannot hold: est ~230145 input + 32000 output is over the ceiling, so this is the exact already-doomed request the clamp exists to prevent (2026-08-29 incident). It went out because plan.Routes.Fallbacks[%d].ContextWindow == %d for the --oversize leg Run appended (tier_routing.go:1401, before the probe at tier_routing.go:1419), and the handler's clamp is gated on route.ContextWindow > 0 (anthropic_openai_proxy.go:1243). max_tokens reached the upstream unclamped (%d); the client got status %d: %s",
			copyIdx, copyWindow, sawMaxTokens, code, body)
	} else if code != http.StatusBadRequest || !strings.Contains(body, "prompt is too long: ") {
		t.Errorf("the request was not forwarded, but the client did not get the recoverable rejection either: %d %s", code, body)
	}
}

// TestAuditR3BSameRemoteSplitNeverProbesTheSiblingWindow
//
// Pinned alongside the test above: the same window question from the other
// side. Green since the same fix.
//
// The defect below was live at HEAD (214fc6d7): the documented same-remote tier
// split (`--model box/small --sonnet-model box/big`, one remote serving both)
// never had its sonnet leg's window probed, because tier_routing.go's probe was
// skipped whenever the leg shared the primary's base URL (the URL-only guard at
// context_window_remote.go:148-149). This test FAILED against HEAD for exactly
// that reason. While this audit ran, the main session fixed it in the working
// tree (sameRoute, "2026-09-26 audit"), so the test now passes and is kept as a
// regression pin for that fix. The HEAD failure was reproduced in a pristine
// export of HEAD (never by stashing the working tree, which holds another
// agent's uncommitted fixes):
//
//	mkdir /tmp/audit-r3b-head && git archive HEAD | tar -x -C /tmp/audit-r3b-head
//	cp cmd/launch/routing_manifest_invariants_test.go /tmp/audit-r3b-head/cmd/launch/
//	cd /tmp/audit-r3b-head && rtk proxy go test ./cmd/launch/ -run AuditR3B -count=1
//
// The remote answers the question — the probe is keyed per
// (BaseURL, ModelsURL, UpstreamModel) and this is a different MODEL — so the
// live value is knowable and was simply never asked for. The consequences were:
// SecondaryContext stayed 0, so the leg's window never reached Claude Code
// (envVars folds leg windows in), and ByModel[SecondaryName].ContextWindow
// stayed 0, so the proxy's context-fit clamp was skipped for every
// sonnet/subagent request (anthropic_openai_proxy.go:1243) — the 2026-08-29
// already-doomed request class, on the tier that carries subagent traffic.
func TestAuditR3BSameRemoteSplitNeverProbesTheSiblingWindow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})

	// One remote enumerating BOTH tiers with their real, different windows.
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"id":"small","context_length":32768},
			{"id":"big","context_length":262144}]}`)
	}))
	defer box.Close()
	writeRemotes(t, fmt.Sprintf(`{"remotes":[{"name":"box","base_url":"%s/v1","api_key":"k","tool_format":"tool_calls"}]}`, box.URL))

	plan, err := buildTierPlan("box/small", "box/big", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	if plan.Secondary.BaseURL != plan.Primary.BaseURL {
		t.Fatalf("fixture: this is not the same-remote split (secondary %q, primary %q)", plan.Secondary.BaseURL, plan.Primary.BaseURL)
	}

	// The real probe (not a stub): the remote is up and enumerates both ids.
	plan.withContextWindows().applyContextWindowsToRoutes()

	if plan.PrimaryContext != 32768 {
		t.Fatalf("fixture: the primary's own window was not probed: %d", plan.PrimaryContext)
	}
	if plan.SecondaryContext != 262144 {
		t.Errorf("the sonnet leg's window was never probed (SecondaryContext=%d, want the 262144 the SAME remote enumerates for %q): the probe is skipped whenever the leg's BaseURL equals the primary's (context_window_remote.go:148-149), so a documented same-remote split runs its sonnet/subagent tier with no window at all — no CLAUDE_CODE_MAX_CONTEXT_TOKENS for that tier and no clamp ceiling",
			plan.SecondaryContext, "big")
	}
	if got := plan.Routes.ByModel["box/big"].ContextWindow; got == 0 {
		t.Errorf("the sonnet tier's route has ContextWindow=0, so the context-fit clamp is skipped for every request that lands on it")
	}
}

// TestAuditR3BOversizeFailoverCopyHidesItsWindowFromEscalation
//
// Finding: same root cause as the test above, second consequence. The copy Run
// appends to plan.Routes.Fallbacks is deduped into escalationTarget's candidate
// set FIRST (it is in Fallbacks, which route_policy.go:407 adds before
// t.Oversize at :408, and `seen` is keyed by BaseURL), so the copy's 0 wins the
// slot and the leg's real probed window never enters the ranking. `auto`
// escalation ranks by ContextWindow, so the --oversize leg — the strongest leg
// in the plan — loses to a smaller alternate: the exact regression
// context_window_remote.go:242-245 documents as fixed.
func TestAuditR3BOversizeFailoverCopyHidesItsWindowFromEscalation(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box.invalid:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"mirror","base_url":"http://mirror.invalid:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"big","base_url":"http://big.invalid:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	origProbe := remoteContextWindowFn
	t.Cleanup(func() { remoteContextWindowFn = origProbe })
	remoteContextWindowFn = func(r proxyRoute) int {
		switch {
		case strings.HasPrefix(r.BaseURL, "http://box.invalid"):
			return 32768
		case strings.HasPrefix(r.BaseURL, "http://mirror.invalid"):
			return 65536
		case strings.HasPrefix(r.BaseURL, "http://big.invalid"):
			return 262144
		}
		return 0
	}

	// A split, so Fallbacks has a leg other than the primary: sonnet on the
	// mirror (a smaller window than the --oversize leg).
	plan, err := buildTierPlan("box/small", "mirror/mid", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	auditR3BWireOversizeLikeRun(t, &plan, "big/huge")

	if got := plan.Routes.Oversize.ContextWindow; got != 262144 {
		t.Fatalf("fixture: --oversize leg's route carries ContextWindow=%d, want its probed 262144", got)
	}

	got, ok := plan.Routes.escalationTarget(plan.Routes.Default)
	if !ok {
		t.Fatal("fixture: escalationTarget found no alternate leg")
	}
	if got.BaseURL != plan.Routes.Oversize.BaseURL {
		t.Errorf("auto escalation picked %s (ContextWindow=%d) over the --oversize leg %s (ContextWindow=%d): the failover copy Run appended at tier_routing.go:1401 carries ContextWindow 0 and is added to escalationTarget's candidates first (route_policy.go:407 before :408), so escalationTarget's `seen[BaseURL]` dedupe drops the real 262144 window that plan.Routes.Oversize carries — the ranking cannot see the strongest leg",
			got.BaseURL, got.ContextWindow, plan.Routes.Oversize.BaseURL, plan.Routes.Oversize.ContextWindow)
	}
}

// auditR3BWireOversizeLikeRun mirrors Run's --oversize wiring and probe call// (tier_routing.go:1365-1421) on a plan built by buildTierPlan: resolve the
// oversize endpoint, install it as Routes.Oversize, append that SAME value as a
// Fallback leg (a copy taken before any window is known), and only then probe
// and apply the windows.
func auditR3BWireOversizeLikeRun(t *testing.T, plan *tierPlan, oversizeModel string) {
	t.Helper()
	over, err := resolveLaunchEndpoint(oversizeModel)
	if err != nil {
		t.Fatalf("--oversize: %v", err)
	}
	if err := gateRemoteToolsEndpoint(over.RemoteEndpoint, toolWireAnthropic, false); err != nil {
		t.Fatalf("--oversize: %v", err)
	}
	plan.Routes.Oversize = routeFor(over)
	if over.Source != sourceNativeAnthropic {
		urlSeen := map[string]bool{}
		for _, f := range plan.Routes.Fallbacks {
			urlSeen[f.BaseURL] = true
		}
		if !urlSeen[plan.Routes.Oversize.BaseURL] {
			plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, plan.Routes.Oversize)
		}
	}
	plan.withContextWindows().applyContextWindowsToRoutes()
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
