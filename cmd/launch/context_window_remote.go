package launch

// Remote context-window discovery: an upstream's /v1/models metadata tells
// us the context the model is actually RUNNING with (vLLM `max_model_len`,
// OpenAI-style routers `context_length`). Claude Code assumes 200k for any
// model id it does not recognize and warns; passing the real window as
// CLAUDE_CODE_MAX_CONTEXT_TOKENS makes auto-compact honor it. Failure to
// probe is not fatal — we simply leave the env unset (previous behavior).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// remoteContextWindowFn is swappable so tests can stub the network probe.
var remoteContextWindowFn = defaultRemoteContextWindow

// probeTimeout is intentionally short: this runs on every launch, before the
// first token. A slow /models must not add seconds of startup latency.
const probeTimeout = 2 * time.Second

type remoteModelMeta struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	MaxModelLen   int    `json:"max_model_len"`
}

type remoteModelsResponse struct {
	Data []remoteModelMeta `json:"data"`
}

// defaultRemoteContextWindow asks route.BaseURL (…/v1) for its model list
// and returns the context window of the upstream model, 0 if unknown.
func defaultRemoteContextWindow(route proxyRoute) int {
	url := strings.TrimRight(route.BaseURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	if route.Key != "" {
		req.Header.Set("Authorization", "Bearer "+route.Key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var parsed remoteModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0
	}
	var meta *remoteModelMeta
	for i := range parsed.Data {
		if parsed.Data[i].ID == route.UpstreamModel {
			meta = &parsed.Data[i]
			break
		}
	}
	if meta == nil {
		if len(parsed.Data) == 1 {
			meta = &parsed.Data[0]
		} else {
			return 0
		}
	}
	// Routers report context_length; a bare vLLM reports max_model_len. A
	// router in front of vLLM may return both (identical). Prefer the
	// larger — never advertise less than the upstream says it serves.
	if meta.ContextLength >= meta.MaxModelLen {
		return meta.ContextLength
	}
	return meta.MaxModelLen
}

// withContextWindows probes each distinct upstream route for its real
// context window and records it on the plan. Never fails: an unreachable or
// non-enumerating upstream just leaves the fields at 0.
func (p *tierPlan) withContextWindows() *tierPlan {
	p.PrimaryContext = remoteContextWindowFn(p.Routes.Default)
	if p.SecondaryName != p.PrimaryName {
		if r, _ := p.Routes.resolve(p.SecondaryName); r.BaseURL != "" &&
			r.BaseURL != p.Routes.Default.BaseURL {
			p.SecondaryContext = remoteContextWindowFn(r)
		}
	}
	return p
}

// contextEnvVars is what Claude Code reads for the real window;
// AUTO_COMPACT_WINDOW is the older knob our cloud-alias path already used —
// keep both in sync rather than sending contradictory hints.
func (p tierPlan) contextEnvVars() []string {
	if p.PrimaryContext <= 0 {
		return nil
	}
	v := strconv.Itoa(p.PrimaryContext)
	return []string{
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS=" + v,
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + v,
	}
}