package launch

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ollama/ollama/envconfig"
)

// Poolside implements Runner for Poolside's CLI.
type Poolside struct{}

var poolsideGOOS = runtime.GOOS

func (p *Poolside) String() string { return "Pool" }

func poolsideUnsupportedError() error {
	return fmt.Errorf("Warning: Poolside is not currently supported on Windows")
}

func (p *Poolside) args(model string, extra []string) []string {
	var args []string
	if model != "" {
		args = append(args, "-m", poolsideModelIDFor(model))
	}
	args = append(args, extra...)
	return args
}

// poolsideBaseURLFor is the standalone base URL Poolside should use: the
// remote's direct base for a user-remote model, otherwise the daemon's /v1.
func poolsideBaseURLFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return envconfig.ConnectableHost().String() + "/v1"
}

// poolsideModelIDFor is the model id Poolside should use: see childModelIDFor.
func poolsideModelIDFor(model string) string {
	return childModelIDFor(model)
}

// poolsideKeyFor is the API key Poolside should use: the remote's token for a
// user-remote model, "ollama" for the daemon.
func poolsideKeyFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return ep.Token
	}
	return "ollama"
}

func (p *Poolside) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	if err := refuseManagedModelFlag("pool", args); err != nil {
		return err
	}

	if poolsideGOOS == "windows" {
		return poolsideUnsupportedError()
	}

	bin, err := exec.LookPath("pool")
	if err != nil {
		return fmt.Errorf("pool is not installed")
	}

	cmd := exec.Command(bin, p.args(model, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Scrubbed of every other remote's credential, as the copilot and kimi doors are; only the
	// key this door chose is handed over (2026-09-29 audit, round 118, F118-L2-3).
	cmd.Env = directLaunchEnv(
		"POOLSIDE_STANDALONE_BASE_URL="+poolsideBaseURLFor(model),
		"POOLSIDE_API_KEY="+poolsideKeyFor(model),
	)
	return runChild(cmd)
}
