package agent

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

type agentOptions struct {
	model    string
	system   string
	maxTurns int
	yes      bool
	cwd      string
}

// AgentCmd returns the "oaica agent" cobra command. checkServerHeartbeat is
// injected (the cmd package's oaicaEnsureSignedIn) to avoid an import cycle:
// cmd imports cmd/agent to register the command, so cmd/agent cannot import
// cmd. This mirrors launch.LaunchCmd's dependency-injection pattern
// (cmd/launch/launch.go).
func AgentCmd(checkServerHeartbeat func(cmd *cobra.Command, args []string) error) *cobra.Command {
	opts := &agentOptions{}
	cmd := &cobra.Command{
		Use:     "agent [PROMPT]",
		Short:   "Run a streaming coding agent",
		Args:    cobra.ArbitraryArgs,
		PreRunE: checkServerHeartbeat,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(cmd.Context(), opts, strings.Join(args, " "))
		},
	}
	cmd.Flags().StringVar(&opts.model, "model", "", "model to use (interactive picker if unset)")
	cmd.Flags().StringVar(&opts.system, "system", "", "system prompt for the agent")
	cmd.Flags().IntVar(&opts.maxTurns, "max-turns", 0, "maximum consecutive tool rounds (0 = model-specific default)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "auto-approve all tool calls without prompting")
	cmd.Flags().StringVar(&opts.cwd, "cwd", "", "agent working directory (default: current directory)")
	return cmd
}

func runAgent(ctx context.Context, opts *agentOptions, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf(`no prompt: provide a prompt argument, e.g. oaica agent "what is the capital of France?"`)
	}

	model := opts.model
	if model == "" {
		var err error
		model, err = launch.ResolveRunModel(ctx, launch.RunModelRequest{})
		if err != nil {
			return fmt.Errorf("resolve model: %w", err)
		}
	}

	baseURL, token, upstreamModel, meta, err := launch.ResolveAgentModel(ctx, model)
	if err != nil {
		return err
	}

	cwd, err := agentWorkingDir(opts.cwd)
	if err != nil {
		return err
	}

	client := newShimClient(baseURL, token, upstreamModel, meta)

	var approval agent.ApprovalPrompter = newTerminalApprovalPrompter(os.Stdin, os.Stdout)
	if opts.yes {
		approval = autoApprovePrompter{}
	}

	skills, err := agent.LoadDefaultSkills(cwd)
	if err != nil {
		return fmt.Errorf("load agent skills: %w", err)
	}

	sess := &agent.Session{
		Client:           client,
		EventSinks:       []agent.EventSink{newStdoutSink()},
		Tools:            agentRegistry(skills, meta),
		Skills:           skills,
		ApprovalPrompter: approval,
		WorkingDir:       cwd,
		Compactor: &agent.SimpleCompactor{
			Client:  client,
			Options: agent.CompactionOptions{ContextWindowTokens: meta.ContextLength},
		},
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var messages []api.Message
	if strings.TrimSpace(prompt) != "" {
		messages = append(messages, api.Message{Role: "user", Content: prompt})
	}

	_, err = sess.Run(ctx, agent.RunOptions{
		Model:         upstreamModel,
		SystemPrompt:  opts.system,
		Messages:      messages,
		MaxToolRounds: opts.maxTurns,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil // interrupted — the engine already finalized the aborted turn
		}
		return err
	}
	return nil
}

// agentWorkingDir resolves the run's working directory, defaulting to the
// process's current directory.
func agentWorkingDir(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return os.Getwd()
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return cwd, nil
}
