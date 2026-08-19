package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ollama/ollama/envconfig"
)

// Copilot implements Runner for GitHub Copilot CLI integration.
type Copilot struct{}

func (c *Copilot) String() string { return "Copilot CLI" }

func (c *Copilot) args(model string, extra []string) []string {
	var args []string
	if model != "" {
		args = append(args, "--model", copilotModelIDFor(model))
	}
	args = append(args, extra...)
	return args
}

func (c *Copilot) findPath() (string, error) {
	if p, err := exec.LookPath("copilot"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := "copilot"
	if runtime.GOOS == "windows" {
		name = "copilot.exe"
	}
	fallback := filepath.Join(home, ".local", "bin", name)
	if _, err := os.Stat(fallback); err != nil {
		return "", err
	}
	return fallback, nil
}

func (c *Copilot) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	copilotPath, err := c.findPath()
	if err != nil {
		return fmt.Errorf("copilot is not installed, install from https://docs.github.com/en/copilot/how-tos/set-up/install-copilot-cli")
	}

	cmd := exec.Command(copilotPath, c.args(model, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Env = append(os.Environ(), c.envVars(model)...)

	return cmd.Run()
}

// copilotBaseURLFor is the provider base URL Copilot should use: the remote's
// direct base for a user-remote model, otherwise the daemon's /v1.
func copilotBaseURLFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return envconfig.Host().String() + "/v1"
}

// copilotModelIDFor is the model id Copilot should use: the bare upstream id
// for a user-remote model, otherwise the picker name.
func copilotModelIDFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	return model
}

// copilotKeyFor is the provider API key: the remote's token for a user-remote
// model, otherwise empty (the daemon is unauthenticated).
func copilotKeyFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.Token
	}
	return ""
}

// copilotWireFor picks Copilot's wire API: "chat" for a user-remote model (most
// remotes only speak /v1/chat/completions, not /v1/responses), "responses" for
// the daemon — unchanged from before.
func copilotWireFor(model string) string {
	if _, ok := resolveRemoteEndpoint(model); ok {
		return "chat"
	}
	return "responses"
}

// envVars returns the environment variables that configure Copilot CLI
// to use Ollama as its model provider.
func (c *Copilot) envVars(model string) []string {
	env := []string{
		"COPILOT_PROVIDER_BASE_URL=" + copilotBaseURLFor(model),
		"COPILOT_PROVIDER_API_KEY=" + copilotKeyFor(model),
		"COPILOT_PROVIDER_WIRE_API=" + copilotWireFor(model),
	}

	if model != "" {
		env = append(env, "COPILOT_MODEL="+copilotModelIDFor(model))
	}

	return env
}
