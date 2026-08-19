package launch

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ollama/ollama/api"
)

// AgentModelMeta carries the model metadata the agent command needs to
// configure its shim and tool gating. Zero values mean "unknown"; consumers
// fall back to their own defaults.
type AgentModelMeta struct {
	ToolCapable     bool
	ContextLength   int
	MaxOutputTokens int
	// Protocol descriptor for a user-remote model (see RemoteDescriptor).
	// Zero values for local/cloud entries — those route through the daemon.
	Wire         string
	ToolFormat   string
	ToolReliable bool
}

// Agent shim defaults when the launch inventory has no metadata for a model.
const (
	defaultAgentContextLength = 128000
	defaultAgentMaxTokens     = 4096
)

// ToolWire is the tool-loop wire an integration drives: OpenAI tool_calls or
// Anthropic tool_use. Each integration declares which it wants so the shared
// capability gate can decide whether a user-remote model's tool format can
// satisfy its loop (see toolGateDecision).
type ToolWire int

const (
	// toolWireOpenAI is the tool loop of every OpenAI-compatible integration
	// (opencode, codex, hermes, kimi, omp, cline, droid, qwen, copilot,
	// poolside, pi): the client issues tool_calls and the model replies with
	// structured tool_calls it consumes.
	toolWireOpenAI ToolWire = iota
	// toolWireAnthropic is the tool loop of Claude Code / cmd/agent: the
	// client issues tool_use blocks and the model replies with tool_use. For
	// an OpenAI remote this goes through the Anthropic↔OpenAI translation
	// proxy, which needs the model to emit real tool_calls.
	toolWireAnthropic
)

func (w ToolWire) String() string {
	switch w {
	case toolWireOpenAI:
		return "OpenAI tool_calls"
	case toolWireAnthropic:
		return "Anthropic tool_use"
	default:
		return "unknown"
	}
}

// toolGateDecision is the single shared capability-gate predicate. A remote
// model is gate-able when its tool format cannot drive the requested wire's
// loop (ToolReliable false — i.e. anything other than real tool_calls, unless
// the user explicitly overrode it). Anthropic-native remotes are always OK on
// the Anthropic wire (handled natively in Phase 3, no OpenAI translation).
func toolGateDecision(wants ToolWire, ep RemoteEndpoint) (ok bool, reason string) {
	if wants == toolWireAnthropic && ep.Wire == "anthropic" {
		return true, ""
	}
	if ep.ToolReliable {
		return true, ""
	}
	format := ep.ToolFormat
	if format == "" {
		format = "unknown"
	}
	return false, fmt.Sprintf(
		"model %q on remote %q emits tool calls as %q (tool_reliable=false); %s requires reliable %s tool calls — set \"tool_format\":\"tool_calls\" for remote %q in remotes.json, or pass --force-tools to proceed anyway",
		ep.UpstreamModel, ep.Name, format, wants, wants, ep.Name)
}

// ResolveOpts controls remote-model resolution for the agent path.
type ResolveOpts struct {
	// ForceTools downgrades the tool-capability gate from refuse to a
	// stderr warning, letting a user-launch a model whose tool format the
	// integration's loop cannot reliably drive.
	ForceTools bool
}

// gateRemoteToolsEndpoint applies the capability gate to a resolved remote
// endpoint, warning-and-proceeding under --force-tools.
func gateRemoteToolsEndpoint(ep RemoteEndpoint, wants ToolWire, force bool) error {
	ok, reason := toolGateDecision(wants, ep)
	if ok {
		return nil
	}
	if force {
		fmt.Fprintf(os.Stderr, "%sWarning: %s%s\n", ansiYellow, reason, ansiReset)
		return nil
	}
	return fmt.Errorf("%s", reason)
}

// gateUserRemoteTools applies the capability gate to a userRemote with the
// bare upstream model id. Used by the Anthropic path (claude.go,
// agent_routing.go) before starting the translation proxy.
func gateUserRemoteTools(remote userRemote, bare string, wants ToolWire, force bool) error {
	d := remote.Descriptor()
	ep := RemoteEndpoint{
		Name:          remote.Name,
		BaseURL:       remote.openAIBase(),
		Token:         remote.key(),
		UpstreamModel: bare,
		Wire:          d.Wire,
		ToolFormat:    d.ToolFormat,
		ToolReliable:  d.ToolReliable,
	}
	return gateRemoteToolsEndpoint(ep, wants, force)
}

// gateOpenAITools applies the capability gate to an OpenAI-wire integration
// whose primary is a user-remote picker name. Local/cloud primaries resolve to
// no endpoint and pass through unchecked.
func gateOpenAITools(model string, force bool) error {
	ep, ok := resolveRemoteEndpoint(model)
	if !ok {
		return nil
	}
	return gateRemoteToolsEndpoint(ep, toolWireOpenAI, force)
}

// extractForceTools pulls a launcher-level --force-tools flag out of the
// passthrough args so integrations can downgrade the tool gate refuse→warn
// without forwarding the flag to the child binary.
func extractForceTools(args []string) (force bool, rest []string) {
	for _, a := range args {
		if a == "--force-tools" || a == "--force-tools=true" {
			force = true
			continue
		}
		rest = append(rest, a)
	}
	return force, rest
}

// extractSonnetModel pulls a launcher-level "--sonnet-model <id>" (or
// "--sonnet-model=<id>") out of the passthrough args, for claude.go's
// opusplan-style tier split: the picker-selected model stays pinned to
// Opus/Haiku, this gives a second model (same remote) for Sonnet-tier
// requests. Accepts either a bare upstream id ("muse-spark-1.2") or the
// namespaced picker form ("opencode-go/muse-spark-1.2") — claude.go resolves
// the latter and verifies it's on the same remote as the primary. Not
// forwarded to the child claude binary — it only controls which
// ANTHROPIC_DEFAULT_SONNET_MODEL/CLAUDE_CODE_SUBAGENT_MODEL env vars we set.
func extractSonnetModel(args []string) (sonnetModel string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--sonnet-model" && i+1 < len(args):
			sonnetModel = args[i+1]
			i++
		case strings.HasPrefix(a, "--sonnet-model="):
			sonnetModel = strings.TrimPrefix(a, "--sonnet-model=")
		default:
			rest = append(rest, a)
		}
	}
	return sonnetModel, rest
}

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
	return ResolveAgentModelWithOpts(ctx, model, ResolveOpts{})
}

// ResolveAgentModelWithOpts is ResolveAgentModel with the capability gate
// applied to user-remote models (refuse unless opts.ForceTools). ResolveAgentModel
// is a ForceTools:false wrapper so existing callers stay back-compatible.
func ResolveAgentModelWithOpts(ctx context.Context, model string, opts ResolveOpts) (baseURL, token, upstreamModel string, meta AgentModelMeta, err error) {
	upstreamModel, _ = oaicaStripLocalTag(model)

	if remote, bare, ok := findUserRemoteForModel(model); ok {
		if err := gateUserRemoteTools(remote, bare, toolWireAnthropic, opts.ForceTools); err != nil {
			return "", "", "", AgentModelMeta{}, err
		}
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
	if lm.Remote {
		meta.Wire = lm.Wire
		meta.ToolFormat = lm.ToolFormat
		meta.ToolReliable = lm.ToolReliable
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
