package cmd

// site.go — `oaica site`: the optional static-site builder. All logic lives
// in internal/sitebuilder; this file only adapts the router chat endpoint
// to sitebuilder.LLM and exposes the cobra subcommands. Nothing in the
// core launch/run/serve path imports or depends on it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ollama/ollama/cmd/launch"
	"github.com/ollama/ollama/internal/sitebuilder"
)

const siteDefaultModel = "kat-awq"

// routerLLM sends sitebuilder requests to the OAICA router (or OAICA_HOST).
type routerLLM struct {
	model string
}

type siteChatRequest struct {
	Model              string             `json:"model"`
	Messages           []oaicaChatMessage `json:"messages"`
	MaxTokens          int                `json:"max_tokens,omitempty"`
	Temperature        float64            `json:"temperature"`
	Stop               []string           `json:"stop,omitempty"`
	Stream             bool               `json:"stream"`
	ChatTemplateKwargs map[string]bool    `json:"chat_template_kwargs,omitempty"`
}

func (r routerLLM) Complete(ctx context.Context, req sitebuilder.Request) (string, error) {
	body := siteChatRequest{
		Model: r.model,
		Messages: []oaicaChatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		MaxTokens:          req.MaxTokens,
		Temperature:        req.Temperature,
		Stop:               req.Stop,
		ChatTemplateKwargs: map[string]bool{"enable_thinking": false},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, oaicaHost()+"/v1/chat/completions", bytes.NewReader(buf))
		if err != nil {
			return "", err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		oaicaAuthorize(httpReq)
		resp, err := (&http.Client{Timeout: 180 * time.Second}).Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
			if ctx.Err() != nil {
				return "", lastErr
			}
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		var out oaicaChatResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, truncateForError(raw))
		}
		if out.Error != nil {
			return "", fmt.Errorf("%s (HTTP %d)", out.Error.Message, resp.StatusCode)
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("empty response (HTTP %d): %s", resp.StatusCode, truncateForError(raw))
		}
		msg := out.Choices[0].Message
		if msg.Content == "" && msg.ReasoningContent != "" {
			return stripThinkTags(msg.ReasoningContent), nil
		}
		return msg.Content, nil
	}
	return "", lastErr
}

func truncateForError(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// siteLLMForModel validates the model against the router's live list so a
// typo fails before the (slow) plan call, listing what is available.
func siteLLMForModel(model string) (sitebuilder.LLM, error) {
	ok, names, err := oaicaModelExists(model)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach OAICA API: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown model %q; available: %s", model, strings.Join(names, ", "))
	}
	return routerLLM{model: model}, nil
}

func siteProgress() sitebuilder.Progress {
	return func(msg string) { fmt.Fprintln(os.Stderr, msg) }
}

func siteCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "site",
		Short: "Build, preview and publish a static website from a brief (optional add-on)",
		Long: `Generate a single-page static website from a one-line brief, section by
section, then preview it locally or publish it to Cloudflare Pages.

  oaica site new ./mysite --prompt "landing page for a JB dental clinic"
  oaica site edit ./mysite --prompt "make the hero mention same-day appointments"
  oaica site preview ./mysite
  oaica site deploy ./mysite --project mysite

State lives in <dir>/.oaica-site so edits regenerate one section at a time.`,
	}

	var model, prompt string
	var overwrite bool
	newCmd := &cobra.Command{
		Use:     "new DIR",
		Short:   "Generate a new site into DIR",
		Args:    cobra.ExactArgs(1),
		PreRunE: oaicaEnsureSignedIn,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if strings.TrimSpace(prompt) == "" {
				return errors.New("--prompt is required (what is the site for?)")
			}
			if _, err := os.Stat(filepath.Join(dir, sitebuilder.IndexFile)); err == nil && !overwrite {
				return fmt.Errorf("%s already has an index.html; use `oaica site edit` or pass --overwrite", dir)
			}
			llm, err := siteLLMForModel(model)
			if err != nil {
				return err
			}
			t0 := time.Now()
			site, err := sitebuilder.Build(cmd.Context(), llm, prompt, model, siteProgress())
			if err != nil {
				return err
			}
			if err := site.Save(dir); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%d sections, %.0fs)\n", filepath.Join(dir, sitebuilder.IndexFile), len(site.Spec.Sections), time.Since(t0).Seconds())
			fmt.Fprintf(os.Stderr, "next: oaica site preview %s   |   oaica site deploy %s --project <name>\n", dir, dir)
			return nil
		},
	}
	newCmd.Flags().StringVar(&prompt, "prompt", "", "what the site is for (business, audience, offer)")
	newCmd.Flags().StringVar(&model, "model", siteDefaultModel, "router model to generate with")
	newCmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing site in DIR")

	var editPrompt, editModel, section string
	editCmd := &cobra.Command{
		Use:     "edit DIR",
		Short:   "Regenerate one section with an instruction",
		Args:    cobra.ExactArgs(1),
		PreRunE: oaicaEnsureSignedIn,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if strings.TrimSpace(editPrompt) == "" {
				return errors.New("--prompt is required (what should change?)")
			}
			site, err := sitebuilder.Load(dir)
			if err != nil {
				return err
			}
			m := editModel
			if m == "" {
				m = site.Model
			}
			llm, err := siteLLMForModel(m)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if section == "" {
				section, err = sitebuilder.ChooseSection(ctx, llm, site.Spec, editPrompt)
				if err != nil {
					return fmt.Errorf("%w — pass --section explicitly (see `oaica site sections %s`)", err, dir)
				}
				fmt.Fprintf(os.Stderr, "targeting section %s\n", section)
			}
			if err := site.Edit(ctx, llm, section, editPrompt, siteProgress()); err != nil {
				return err
			}
			if err := site.Save(dir); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "updated %s\n", filepath.Join(dir, sitebuilder.IndexFile))
			return nil
		},
	}
	editCmd.Flags().StringVar(&editPrompt, "prompt", "", "the change to make")
	editCmd.Flags().StringVar(&section, "section", "", "section id to regenerate (default: let the model pick)")
	editCmd.Flags().StringVar(&editModel, "model", "", "router model (default: the one the site was built with)")

	sectionsCmd := &cobra.Command{
		Use:   "sections DIR",
		Short: "List the sections of a site",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			site, err := sitebuilder.Load(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s — %s (model %s)\n", site.Spec.Title, site.Spec.Tagline, site.Model)
			for _, s := range site.Spec.Sections {
				fmt.Printf("  %-16s %-12s %s\n", s.ID, s.Kind, s.Title)
			}
			return nil
		},
	}

	var port int
	var noOpen bool
	previewCmd := &cobra.Command{
		Use:   "preview DIR",
		Short: "Serve DIR locally in a sandboxed preview (Ctrl-C to stop)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if _, err := os.Stat(filepath.Join(dir, sitebuilder.IndexFile)); err != nil {
				return fmt.Errorf("%s has no index.html", dir)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			url, err := sitebuilder.Preview(ctx, dir, port)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "preview: %s  (Ctrl-C to stop)\n", url)
			if !noOpen {
				launch.OpenBrowser(url)
			}
			<-ctx.Done()
			return nil
		},
	}
	previewCmd.Flags().IntVar(&port, "port", 4173, "port to listen on (0 = random)")
	previewCmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open a browser")

	var project string
	deployCmd := &cobra.Command{
		Use:   "deploy DIR",
		Short: "Publish DIR to Cloudflare Pages via wrangler",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := sitebuilder.Deploy(cmd.Context(), args[0], project, os.Stderr)
			if err != nil {
				return err
			}
			fmt.Println(url)
			return nil
		},
	}
	deployCmd.Flags().StringVar(&project, "project", "", "Cloudflare Pages project name (created if missing)")
	_ = deployCmd.MarkFlagRequired("project")

	root.AddCommand(newCmd, editCmd, sectionsCmd, previewCmd, deployCmd)
	return root
}
