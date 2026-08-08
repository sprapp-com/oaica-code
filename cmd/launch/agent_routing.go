package launch

import (
	"context"
	"fmt"

	"github.com/ollama/ollama/api"
)

// AgentModelMeta carries the model metadata the agent command needs to
// configure its shim and tool gating. Zero values mean "unknown"; consumers
// fall back to their own defaults.
type AgentModelMeta struct {
	ToolCapable     bool
	ContextLength   int
	MaxOutputTokens int
}

// Agent shim defaults when the launch inventory has no metadata for a model.
const (
	defaultAgentContextLength = 128000
	defaultAgentMaxTokens     = 4096
)

// ResolveAgentModel resolves a picker model name to the Anthropic-native
// /v1/messages endpoint the agent shim should talk to, the bearer token to
// send, and the bare upstream model id (picker tag and remote prefix
// stripped). It mirrors claude.go's routing decision without launching a
// child process:
//
//   - user-defined remote ("<remote>/<model>") → loopback Anthropic↔OpenAI
//     translation proxy bound to the remote's own key. A bind failure is a
//     hard error — never silently fall back to the OAICA router (which would
//     send the remote's model name to the wrong endpoint with the wrong key).
//   - cloud / ":local" → loopback logging proxy in front of the resolved host
//     (best-effort; the host is used directly if the proxy cannot bind).
//
// Model metadata comes from the launch inventory (tool capability, context
// length, max output tokens) with sensible defaults when the model is unknown
// or the inventory is unreachable.
func ResolveAgentModel(ctx context.Context, model string) (baseURL, token, upstreamModel string, meta AgentModelMeta, err error) {
	upstreamModel, _ = oaicaStripLocalTag(model)

	if remote, bare, ok := findUserRemoteForModel(model); ok {
		upstreamModel = bare
		ln, port, lerr := ListenAnthropicOpenAIProxy(remote, bare)
		if lerr != nil {
			return "", "", "", AgentModelMeta{}, fmt.Errorf("failed to start translation proxy for remote %q: %w", remote.Name, lerr)
		}
		go func() { _ = RunAnthropicOpenAIProxy(ln, remote, bare) }()
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		token = remote.key()
	} else {
		realHost := oaicaResolveHostForModel(model)
		baseURL = realHost
		if ln, port, lerr := ListenLocalLoggingProxy(); lerr == nil {
			go func() { _ = RunLocalLoggingProxy(ln, realHost) }()
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		token = oaicaLaunchAPIKeyForEnv()
	}

	meta = agentModelMeta(ctx, model)
	return baseURL, token, upstreamModel, meta, nil
}

// applyAgentModelMeta fills ToolCapable and applies positive limit overrides
// from a matched inventory entry. Tool capability is only known for local
// entries (client.List carries real capability data); cloud/router and
// user-remote entries leave ToolCapable at its zero value even though the
// models are typically tool-capable, so they are treated as tool-capable.
func applyAgentModelMeta(meta AgentModelMeta, lm LaunchModel, found bool) AgentModelMeta {
	if !found {
		return meta
	}
	meta.ToolCapable = lm.ToolCapable || lm.Remote
	if lm.ContextLength > 0 {
		meta.ContextLength = lm.ContextLength
	}
	if lm.MaxOutputTokens > 0 {
		meta.MaxOutputTokens = lm.MaxOutputTokens
	}
	return meta
}

// agentModelMeta looks the model up in the launch inventory and applies
// defaults. A matched LOCAL inventory entry keeps its advertised tool
// capability (false really means no tools); remote/cloud entries are treated
// as tool-capable regardless of their advertised capability. An unknown or
// unreachable model is assumed tool-capable — an agent without tools is
// useless, and a model that cannot actually call tools fails visibly on the
// first call instead.
func agentModelMeta(ctx context.Context, model string) AgentModelMeta {
	meta := AgentModelMeta{
		ToolCapable:     true,
		ContextLength:   defaultAgentContextLength,
		MaxOutputTokens: defaultAgentMaxTokens,
	}
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return meta
	}
	models, err := newModelInventory(client).Load(ctx)
	if err != nil {
		return meta
	}
	if lm, ok := findLaunchModel(models, model); ok {
		return applyAgentModelMeta(meta, lm, true)
	}
	fb := fallbackLaunchModel(model)
	if fb.ContextLength > 0 {
		meta.ContextLength = fb.ContextLength
	}
	if fb.MaxOutputTokens > 0 {
		meta.MaxOutputTokens = fb.MaxOutputTokens
	}
	return meta
}
