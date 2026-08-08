package agent

import (
	"os"

	"github.com/ollama/ollama/agent"
	agenttools "github.com/ollama/ollama/agent/tools"
	"github.com/ollama/ollama/cmd/launch"
)

// agentRegistry builds the tool registry for an agent run. Unlike the TUI's
// registry (cmd/agent_tui.go) there is no local server to probe for
// capabilities, so gating is driven by env vars (the user's explicit control)
// plus AgentModelMeta.ToolCapable from the launch inventory. A nil return
// tells the engine to run text-only.
func agentRegistry(skills *agent.SkillCatalog, meta launch.AgentModelMeta) *agent.Registry {
	if !meta.ToolCapable {
		return nil
	}
	registry := &agent.Registry{}
	if os.Getenv("OLLAMA_AGENT_DISABLE_SHELL") == "" {
		registry.Register(&agenttools.Bash{})
	}
	registry.Register(&agenttools.Read{})
	registry.Register(&agenttools.Edit{})
	if skills != nil && len(skills.List()) > 0 {
		registry.Register(&agenttools.Skill{Catalog: skills})
	}
	if os.Getenv("OLLAMA_AGENT_DISABLE_WEBSEARCH") == "" {
		registry.Register(&agenttools.WebSearch{})
		registry.Register(&agenttools.WebFetch{})
	}
	return registry
}
