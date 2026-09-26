package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/internal/modelref"
	"github.com/ollama/ollama/readline"
	"github.com/ollama/ollama/types/model"
)

type MultilineState int

const (
	MultilineNone MultilineState = iota
	MultilinePrompt
	MultilineSystem
)

func generateInteractive(cmd *cobra.Command, opts runOptions) error {
	usage := func() {
		fmt.Fprintln(os.Stderr, "Available Commands:")
		fmt.Fprintln(os.Stderr, "  /set            Set session variables")
		fmt.Fprintln(os.Stderr, "  /show           Show model information")
		fmt.Fprintln(os.Stderr, "  /load <model>   Switch the active model (same as /model)")
		fmt.Fprintln(os.Stderr, "  /model <name>   Switch active model (OAICA API — see /model list)")
		fmt.Fprintln(os.Stderr, "  /clear          Clear session context")
		fmt.Fprintln(os.Stderr, "  /bye            Exit")
		fmt.Fprintln(os.Stderr, "  /?, /help       Help for a command")
		fmt.Fprintln(os.Stderr, "  /? shortcuts    Help for keyboard shortcuts")

		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Use \"\"\" to begin a multi-line message.")

		if opts.MultiModal {
			fmt.Fprintf(os.Stderr, "Use %s to include .jpg, .png, .webp images, or .wav audio files.\n", filepath.FromSlash("/path/to/file"))
		}

		fmt.Fprintln(os.Stderr, "")
	}

	// usageSet lists the options this session can actually honour. It used to
	// list /set parameter, /set format json, /set noformat, /set think and
	// /set nothink as well, and every one of those arms answers that the
	// setting is not sent in this session: the help advertised five promises
	// the REPL refuses (2026-09-26 audit, tenth round). They stay accepted and
	// keep explaining why — see the /set arm — but the help only offers what
	// is honoured, the invariant usage() above already keeps for the command
	// it no longer runs.
	usageSet := func() {
		fmt.Fprintln(os.Stderr, "Available Commands:")
		fmt.Fprintln(os.Stderr, "  /set system <string>   Set system message")
		fmt.Fprintln(os.Stderr, "  /set history           Enable history")
		fmt.Fprintln(os.Stderr, "  /set nohistory         Disable history")
		fmt.Fprintln(os.Stderr, "  /set wordwrap          Enable wordwrap")
		fmt.Fprintln(os.Stderr, "  /set nowordwrap        Disable wordwrap")
		fmt.Fprintln(os.Stderr, "  /set verbose           Show LLM stats")
		fmt.Fprintln(os.Stderr, "  /set quiet             Disable LLM stats")
		fmt.Fprintln(os.Stderr, "")
	}

	usageShortcuts := func() {
		fmt.Fprintln(os.Stderr, "Available keyboard shortcuts:")
		fmt.Fprintln(os.Stderr, "  Ctrl + a            Move to the beginning of the line (Home)")
		fmt.Fprintln(os.Stderr, "  Ctrl + e            Move to the end of the line (End)")
		fmt.Fprintln(os.Stderr, "   Alt + b            Move back (left) one word")
		fmt.Fprintln(os.Stderr, "   Alt + f            Move forward (right) one word")
		fmt.Fprintln(os.Stderr, "  Ctrl + k            Delete the sentence after the cursor")
		fmt.Fprintln(os.Stderr, "  Ctrl + u            Delete the sentence before the cursor")
		fmt.Fprintln(os.Stderr, "  Ctrl + w            Delete the word before the cursor")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  Ctrl + l            Clear the screen")
		fmt.Fprintln(os.Stderr, "  Ctrl + g            Open default editor to compose a prompt")
		fmt.Fprintln(os.Stderr, "  Ctrl + c            Stop the model from responding")
		fmt.Fprintln(os.Stderr, "  Ctrl + d            Exit oaica (/bye)")
		fmt.Fprintln(os.Stderr, "")
	}

	usageShow := func() {
		fmt.Fprintln(os.Stderr, "Available Commands:")
		fmt.Fprintln(os.Stderr, "  /show info         Show what the OAICA router publishes about this model")
		fmt.Fprintln(os.Stderr, "  /show system       Show the session's system message")
		fmt.Fprintln(os.Stderr, "  /show parameters   Show what a session sends — none: the router takes the model and the conversation")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  /show license, modelfile, template: not available in this session — the")
		fmt.Fprintln(os.Stderr, "  model is served by the OAICA API, which publishes no Modelfile for it.")
		fmt.Fprintln(os.Stderr, "")
	}

	// usageShow lists what /show answers in a session served by the OAICA API.
	// The Ollama subcommands it used to list (license, modelfile, template)
	// asked a local daemon for a Modelfile, which this fork has no equivalent
	// of — the router publishes a model's id, description and rating, and
	// nothing else. They stay accepted and say so; see the /show arm.
	scanner, err := readline.New(readline.Prompt{
		Prompt:         ">>> ",
		AltPrompt:      "... ",
		Placeholder:    "Send a message (/? for help)",
		AltPlaceholder: "Press Enter to send",
	})
	if err != nil {
		return err
	}

	if envconfig.NoHistory() {
		scanner.HistoryDisable()
	}

	fmt.Print(readline.StartBracketedPaste)
	defer fmt.Printf(readline.EndBracketedPaste)

	var sb strings.Builder
	var multiline MultilineState
	// oaicaActiveModel: when set (via /model, or pre-seeded from `oaica run
	// <model>`'s opts.Model), subsequent user turns route through the OAICA
	// OpenAI-compatible router (oaica_client.go) instead of Ollama's native
	// client.Generate()/chat() path. Empty = fall through to the original
	// Ollama-native behavior (untouched) — only happens if RunHandler was
	// invoked without a model name, which cobra's arg validation prevents.
	oaicaActiveModel := opts.Model
	var oaicaHistory []oaicaChatMessage

	for {
		line, err := scanner.Readline()
		switch {
		case errors.Is(err, io.EOF):
			fmt.Println()
			return nil
		case errors.Is(err, readline.ErrInterrupt):
			if line == "" {
				fmt.Println("\nUse Ctrl + d or /bye to exit.")
			}

			scanner.Prompt.UseAlt = false
			sb.Reset()

			continue
		case errors.Is(err, readline.ErrEditPrompt):
			sb.Reset()
			content, err := editInExternalEditor(line)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			scanner.Prefill = content
			continue
		case err != nil:
			return err
		}

		switch {
		case multiline != MultilineNone:
			// check if there's a multiline terminating string
			before, ok := strings.CutSuffix(line, `"""`)
			sb.WriteString(before)
			if !ok {
				fmt.Fprintln(&sb)
				scanner.Prompt.UseAlt = true
				continue
			}

			switch multiline {
			case MultilineSystem:
				opts.System = sb.String()
				opts.Messages = append(opts.Messages, api.Message{Role: "system", Content: opts.System})
				fmt.Println("Set system message.")
				sb.Reset()
			}

			multiline = MultilineNone
			scanner.Prompt.UseAlt = false
		case strings.HasPrefix(line, `"""`):
			line := strings.TrimPrefix(line, `"""`)
			line, ok := strings.CutSuffix(line, `"""`)
			sb.WriteString(line)
			if !ok {
				// no multiline terminating string; need more input
				fmt.Fprintln(&sb)
				multiline = MultilinePrompt
				scanner.Prompt.UseAlt = true
			}
		case scanner.Pasting:
			fmt.Fprintln(&sb, line)
			continue
		case isSlashCommand(line, "/list"):
			args := strings.Fields(line)
			if err := ListHandler(cmd, args[1:]); err != nil {
				return err
			}
		case isSlashCommand(line, "/load"):
			args := strings.Fields(line)
			if len(args) != 2 {
				fmt.Println("Usage:\n  /load <modelname>")
				continue
			}
			// Same act as /model <name> in this fork —
			// see oaicaSwitchActiveModel. The daemon path this used to take
			// (client.Show → applyShowResponseToRunOptions →
			// loadOrUnloadModel) changed opts.Model, which the OAICA gate below
			// never reads, so the session kept answering from the model that
			// was already active and every failure that was not "not found"
			// ended the session with "error: couldn't connect to ollama
			// server" (2026-09-26 audit).
			if _, err := oaicaSwitchActiveModel(args[1], &oaicaActiveModel, &oaicaHistory); err != nil {
				fmt.Printf("error: %v\n", err)
			}
			continue
		case isSlashCommand(line, "/save"):
			args := strings.Fields(line)
			if len(args) != 2 {
				fmt.Println("Usage:\n  /save <modelname>")
				continue
			}
			// Refused, not attempted. /save built a LOCAL Ollama model from the
			// session's run options (NewCreateRequest → client.Create), and a
			// session here is served by the OAICA API: the model it created
			// was real but had nothing to do with the turns it claimed to
			// save, and reaching a daemon that need not be running both cost a
			// round trip and ended the session on failure (2026-09-26 audit).
			// Saying so, and staying in the session, is the honest version of
			// the promise the help used to make.
			fmt.Println("save: unavailable — this session is served by the OAICA API, so there is no local model to write. Use `oaica model add` outside the session to register one.")
			continue
		case isSlashCommand(line, "/clear"):
			oaicaHistory = resetConversation(&opts, oaicaHistory)
			fmt.Println("Cleared session context")
			continue
		case isSlashCommand(line, "/set"):
			args := strings.Fields(line)
			if len(args) > 1 {
				switch args[1] {
				case "history":
					scanner.HistoryEnable()
				case "nohistory":
					scanner.HistoryDisable()
				case "wordwrap":
					opts.WordWrap = true
					fmt.Println("Set 'wordwrap' mode.")
				case "nowordwrap":
					opts.WordWrap = false
					fmt.Println("Set 'nowordwrap' mode.")
				case "verbose":
					if err := cmd.Flags().Set("verbose", "true"); err != nil {
						return err
					}
					fmt.Println("Set 'verbose' mode.")
				case "quiet":
					if err := cmd.Flags().Set("verbose", "false"); err != nil {
						return err
					}
					fmt.Println("Set 'quiet' mode.")
				case "think":
					oaicaSettingNotSent("think", "Thinking is off for every request this fork sends — oaicaChatComplete pins enable_thinking=false, and any reasoning a backend returns anyway is stripped before you see it.")
				case "nothink":
					oaicaSettingNotSent("nothink", "Thinking is already off for every request this fork sends.")
				case "format":
					if len(args) < 3 || args[2] != "json" {
						fmt.Println("Invalid or missing format. For 'json' mode use '/set format json'")
						break
					}
					oaicaSettingNotSent("format json", "The router's JSON mode is not wired up.")
				case "noformat":
					oaicaSettingNotSent("noformat", "Nothing is in format mode to begin with.")
				case "parameter":
					oaicaSettingNotSent("parameter", "Ollama generation parameters (num_ctx, temperature, …) are not sent to the router.")
				case "system":
					if len(args) < 3 {
						usageSet()
						continue
					}

					multiline = MultilineSystem

					line := strings.Join(args[2:], " ")
					line, ok := strings.CutPrefix(line, `"""`)
					if !ok {
						multiline = MultilineNone
					} else {
						// only cut suffix if the line is multiline
						line, ok = strings.CutSuffix(line, `"""`)
						if ok {
							multiline = MultilineNone
						}
					}

					sb.WriteString(line)
					if multiline != MultilineNone {
						scanner.Prompt.UseAlt = true
						continue
					}

					opts.System = sb.String() // for display in modelfile
					newMessage := api.Message{Role: "system", Content: sb.String()}
					// Check if the slice is not empty and the last message is from 'system'
					if len(opts.Messages) > 0 && opts.Messages[len(opts.Messages)-1].Role == "system" {
						// Replace the last message
						opts.Messages[len(opts.Messages)-1] = newMessage
					} else {
						opts.Messages = append(opts.Messages, newMessage)
					}
					fmt.Println("Set system message.")
					sb.Reset()
					continue
				default:
					fmt.Printf("Unknown command '/set %s'. Type /? for help\n", args[1])
				}
			} else {
				usageSet()
			}
		case isSlashCommand(line, "/show"):
			args := strings.Fields(line)
			if len(args) > 1 {
				switch args[1] {
				case "info":
					if err := oaicaShowInfo(oaicaActiveModel); err != nil {
						fmt.Printf("error: %v\n", err)
					}
				case "system":
					if strings.TrimSpace(opts.System) != "" {
						fmt.Println(opts.System + "\n")
					} else {
						fmt.Println("No system message was specified for this session. Set one with /set system <text>.")
					}
				case "parameters":
					fmt.Println("This session sends no generation parameters — the router takes the model and the conversation. See /? /set.")
					fmt.Println()
				case "license", "modelfile", "template":
					fmt.Printf("/show %s: not available in this session — %s is served by the OAICA API, which publishes no Modelfile for it.\n", args[1], oaicaActiveModel)
				default:
					fmt.Printf("Unknown command '/show %s'. Type /? for help\n", args[1])
				}
			} else {
				usageShow()
			}
		case isSlashCommand(line, "/help") || isSlashCommand(line, "/?"):
			args := strings.Fields(line)
			if len(args) > 1 {
				switch args[1] {
				case "set", "/set":
					usageSet()
				case "show", "/show":
					usageShow()
				case "shortcut", "shortcuts":
					usageShortcuts()
				}
			} else {
				usage()
			}
		case isSlashCommand(line, "/model"):
			args := strings.Fields(line)
			if len(args) < 2 {
				fmt.Println("Usage:\n  /model <name>\n  /model list")
				if oaicaActiveModel != "" {
					fmt.Printf("Active OAICA model: %s\n", oaicaActiveModel)
				}
				continue
			}
			if args[1] == "list" {
				entries, err := oaicaListModelsDetailed()
				if err != nil {
					fmt.Printf("error: %v\n", err)
					continue
				}
				oaicaPrintModelList(entries)
				continue
			}
			if _, err := oaicaSwitchActiveModel(args[1], &oaicaActiveModel, &oaicaHistory); err != nil {
				fmt.Printf("error: %v\n", err)
			}
			continue
		case isSlashCommand(line, "/lora"):
			args := strings.Fields(line)
			if len(args) < 2 {
				fmt.Println("Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list\n  /lora use <name> [name2 ...]\n  /lora stack <name>\n  /lora off")
				continue
			}
			switch args[1] {
			case "use", "stack":
				if len(args) < 3 {
					fmt.Printf("Usage: /lora %s <name> [name2 ...]\n", args[1])
					continue
				}
				loras, err := oaicaListLoras()
				if err != nil {
					fmt.Printf("error: %v\n", err)
					continue
				}
				byName := map[string]oaicaLoraListEntry{}
				for _, l := range loras {
					byName[l.Name] = l
				}
				entries := []oaicaLoraRequestEntry{}
				models := map[string]bool{}
				if args[1] == "stack" {
					entries = append(entries, activeLocalLoras...)
				}
				var addedNames []string
				unknown := false
				for _, name := range args[2:] {
					found, ok := byName[name]
					if !ok {
						fmt.Printf("Unknown LoRA '%s'. Configured: ", name)
						names := make([]string, len(loras))
						for i, l := range loras {
							// Quoted like /lora list's rows: these names are the
							// router's, and this line is one line.
							names[i] = launch.PrintableCell(l.Name)
						}
						fmt.Println(strings.Join(names, ", "))
						unknown = true
						break
					}
					entries = append(entries, oaicaLoraRequestEntry{ID: found.ID, Scale: 1})
					models[found.Model] = true
					addedNames = append(addedNames, name)
				}
				if unknown {
					continue
				}
				if len(models) > 1 {
					fmt.Println("Stacked LoRAs must all belong to the same backend model (they load together into one llama-server) — mixed models given.")
					continue
				}
				activeLocalLoras = entries
				verb := "Using"
				if args[1] == "stack" {
					verb = "Stacked"
				}
				fmt.Printf("%s LoRA(s) [%s] for this session only (per-request — doesn't affect other users)\n", verb, strings.Join(addedNames, ", "))
			case "off":
				activeLocalLoras = nil
				fmt.Println("Per-request LoRA disabled for this session.")
			case "list":
				loras, err := oaicaListLoras()
				if err != nil {
					fmt.Printf("error: %v\n", err)
					continue
				}
				if len(loras) == 0 {
					fmt.Println("No LoRA adapters configured.")
					continue
				}
				oaicaPrintLoraList(loras)
			case "add":
				if len(args) < 3 {
					fmt.Println("Usage: /lora add <name>")
					continue
				}
				model, err := oaicaLoraAdd(args[2])
				if err != nil {
					fmt.Printf("error: %v\n", err)
					continue
				}
				fmt.Printf("LoRA '%s' activated on model '%s'\n", args[2], model)
			case "remove":
				if len(args) < 3 {
					fmt.Println("Usage: /lora remove <name>")
					continue
				}
				model, err := oaicaLoraRemove(args[2])
				if err != nil {
					fmt.Printf("error: %v\n", err)
					continue
				}
				fmt.Printf("LoRA '%s' deactivated on model '%s'\n", args[2], model)
			default:
				fmt.Println("Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list\n  /lora use <name>\n  /lora off")
			}
			continue
		case isSlashCommand(line, "/agent"):
			args := strings.SplitN(line, " ", 2)
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				fmt.Println("Usage:\n  /agent <task>\n\nRuns a tool-using ReAct agent (NeMo Agent Toolkit) instead of a plain\nchat turn — use for tasks that need a real tool call (e.g. current time),\nnot for ordinary conversation.")
				continue
			}
			result, err := oaicaAgentRun(strings.TrimSpace(args[1]))
			if err != nil {
				fmt.Printf("error: %v\n", err)
				continue
			}
			fmt.Println(result)
			continue
		case isSlashCommand(line, "/exit"), isSlashCommand(line, "/bye"):
			return nil
		case strings.HasPrefix(line, "/"):
			args := strings.Fields(line)
			isFile := false

			if opts.MultiModal {
				for _, f := range extractFileNames(line) {
					if strings.HasPrefix(f, args[0]) {
						isFile = true
						break
					}
				}
			}

			if !isFile {
				if slashLineDisposition(line) == slashUnknownCommand {
					fmt.Printf("Unknown command '%s'. Type /? for help\n", args[0])
					continue
				}
			}

			sb.WriteString(line)
		default:
			sb.WriteString(line)
		}

		if sb.Len() > 0 && multiline == MultilineNone && oaicaActiveModel != "" {
			// OAICA thin-client path: bypass Ollama's native chat() entirely,
			// speak OpenAI-shaped /v1/chat/completions to api.oaica.com.
			newHistory, reply, err := oaicaTurn(oaicaActiveModel, oaicaHistory, sb.String(), opts.System, oaicaVerboseRequested(cmd))
			oaicaHistory = newHistory
			if err != nil {
				fmt.Printf("error: %v\n", err)
				sb.Reset()
				continue
			}
			fmt.Println(reply)
			sb.Reset()
			continue
		}

		if sb.Len() > 0 && multiline == MultilineNone {
			newMessage := api.Message{Role: "user", Content: sb.String()}

			if opts.MultiModal {
				msg, images, err := extractFileData(sb.String())
				if err != nil {
					return err
				}

				newMessage.Content = msg
				newMessage.Images = images
			}

			opts.Messages = append(opts.Messages, newMessage)

			assistant, err := chat(cmd, opts)
			if err != nil {
				if strings.Contains(err.Error(), "does not support thinking") ||
					strings.Contains(err.Error(), "invalid think value") {
					fmt.Printf("error: %v\n", err)
					sb.Reset()
					continue
				}
				return err
			}
			if assistant != nil {
				opts.Messages = append(opts.Messages, *assistant)
			}

			sb.Reset()
		}
	}
}

// resetConversation starts a new conversation and returns the (empty) OAICA
// history for it. The interactive session holds the conversation in two
// places: opts.Messages, which the native path sends, and oaicaHistory, which
// the OAICA thin-client path posts to the router. Every "this is a new
// conversation now" site has to reset both — /clear did not, so a user who
// cleared the context and then kept talking through the thin-client path kept
// sending the conversation they had just cleared (2026-09-26 audit).
func resetConversation(opts *runOptions, history []oaicaChatMessage) []oaicaChatMessage {
	opts.Messages = []api.Message{}
	if opts.System != "" {
		opts.Messages = append(opts.Messages, api.Message{Role: "system", Content: opts.System})
	}
	return nil
}

// oaicaShowInfo prints what the OAICA router publishes about a model — its id,
// its "recommended for" line and its rating. Those three are the router's whole
// record, so this is the honest /show info for a session here; the Ollama
// version asked a local daemon for a Modelfile.
//
// A model the router no longer lists is not an error: a session can outlive a
// rename, and /show must not be the command that ends it.
// oaicaPrintModelList writes /model list's rows from the router's answer.
//
// Both columns are ROUTER-supplied strings — the id and the "recommended for"
// description are set once via the router's admin API — so they are no more
// trustworthy than the catalog ids `model sync` prints, and a newline in either
// forged a row in this picker: the user reads a list of models, one of which
// the router never published. launch.PrintableCell is the rule the rest of the
// toolkit uses for exactly this; see store_cell_forgery_integrity_test.go for
// the same call at the other listings (2026-09-26 audit, round 13).
func oaicaPrintModelList(entries []oaicaModelListEntry) {
	fmt.Println("Available models:")
	for _, m := range entries {
		fmt.Printf("  %-28s %s\n", launch.PrintableCell(m.ID), starString(m.Stars))
		if m.Description != "" {
			fmt.Printf("  %-28s %s\n", "", launch.PrintableCell(m.Description))
		}
	}
}

// oaicaPrintLoraList writes /lora list's rows. Same argument as
// oaicaPrintModelList: the name and the backend model come over HTTP from a
// router the user may not own.
func oaicaPrintLoraList(loras []oaicaLoraListEntry) {
	fmt.Println("Configured LoRA adapters:")
	for _, l := range loras {
		fmt.Printf("  %s  (model: %s, slot: %d)\n", launch.PrintableCell(l.Name), launch.PrintableCell(l.Model), l.ID)
	}
}

func oaicaShowInfo(model string) error {
	entries, err := oaicaListModelsDetailed()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID == model || strings.HasPrefix(model, e.ID+"+") {
			fmt.Printf("model:  %s\n", model)
			if e.Description != "" {
				// Free text the ROUTER controls; a newline in it emitted
				// arbitrary lines inside this panel (2026-09-26 audit,
				// fifteenth round).
				fmt.Printf("best for: %s\n", launch.PrintableCell(e.Description))
			}
			if e.Stars > 0 {
				fmt.Printf("rating: %s\n", starString(e.Stars))
			}
			if e.Description == "" && e.Stars == 0 {
				fmt.Println("the router publishes no description or rating for this model")
			}
			return nil
		}
	}
	fmt.Printf("model:  %s\n", model)
	fmt.Println("the router does not list this model — it may have been renamed or removed since this session started")
	return nil
}

// oaicaSettingNotSent answers a `/set` option the router is never sent with.
//
// generateInteractive's OAICA gate takes every turn, so the native branch that
// reads opts.Think, opts.Format and opts.Options is unreachable here: setting
// them changed nothing a turn could observe and printed a success anyway — the
// same inert promise /load and /save made (2026-09-26 audit). Saying so is the
// honest version. The option names stay accepted, because a session scripted
// against them must keep running; only the answer changes.
func oaicaSettingNotSent(name, why string) {
	fmt.Printf("Not set: '%s' is not sent in this session — every turn is served by the OAICA API, which takes the model and the conversation. %s\n", name, why)
}

// oaicaSwitchActiveModel points an OAICA session at name, clears the
// conversation that belonged to the previous model, and reports whether it
// switched. /model and /load both go through here, because in this fork they
// are the same act: every turn is served by the OAICA API (the oaicaActiveModel
// gate in generateInteractive), so there is no daemon-side "load" to do.
// /load's own daemon work — client.Show, loadOrUnloadModel — put a model into a
// process this session never talks to while the turns kept going to the model
// already active, and printed a success about nothing the user could observe
// (2026-09-26 audit).
//
// An unknown name is not an error: the session stays where it was and the user
// is shown what exists. Returning an error here for a typo is what made /load
// end the session, and it is the shape this function exists to not have.
// Only an unreachable router is an error.
func oaicaSwitchActiveModel(name string, activeModel *string, history *[]oaicaChatMessage) (bool, error) {
	ok, names, err := oaicaModelExists(name)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Printf("Unknown model '%s'. Available models:\n", name)
		for _, n := range names {
			fmt.Printf("  %s\n", launch.PrintableCell(n))
		}
		return false, nil
	}
	*activeModel = name
	*history = nil
	fmt.Printf("Switched to model '%s'\n", name)
	return true, nil
}

// oaicaTurn appends the user's turn, asks the OAICA router for a reply, and
// returns the conversation as it stands afterwards: the user turn plus the
// reply on success, and NOTHING on failure.
//
// A failed turn must not be kept. The model never answered it, so leaving it
// in the history means every later request re-sends a prompt the user got no
// reply to — once more for each retry — until the router's context is full of
// turns the conversation never had (2026-09-26 audit).
//
// verbose is the `/set verbose` state, re-read per turn so toggling it
// mid-session takes effect on the next reply. The timings go to stderr — see
// oaicaChatTimed for why they are not on stdout like the native path's.
//
// system is the `/set system` message, sent ahead of the conversation on every
// turn. It is a per-turn argument rather than a member of the history because
// the two have different lifetimes: /clear resets the conversation and must
// leave the system message standing, and a history that carried it would
// re-send it once per turn's copy of itself. Before this, `/set system` wrote
// opts.System, which only the unreachable native branch reads, so the message
// the user typed was dropped in silence (2026-09-26 audit).
func oaicaTurn(model string, history []oaicaChatMessage, prompt, system string, verbose bool) ([]oaicaChatMessage, string, error) {
	next := append(history, oaicaChatMessage{Role: "user", Content: prompt})
	reply, err := oaicaChatTimed(os.Stderr, verbose, model, oaicaWithSystem(system, next))
	if err != nil {
		return history, "", err
	}
	return append(next, oaicaChatMessage{Role: "assistant", Content: reply}), reply, nil
}

// oaicaWithSystem returns msgs with the session's system message at the head,
// where the router reads it. An empty (or whitespace-only) message is omitted
// rather than sent empty — an empty system turn is a request the user did not
// ask for.
func oaicaWithSystem(system string, msgs []oaicaChatMessage) []oaicaChatMessage {
	if strings.TrimSpace(system) == "" {
		return msgs
	}
	out := make([]oaicaChatMessage, 0, len(msgs)+1)
	out = append(out, oaicaChatMessage{Role: "system", Content: system})
	return append(out, msgs...)
}

// oaicaVerboseRequested reports whether --verbose (or `/set verbose`, which
// sets the same flag) is on. Unreadable state counts as off: a missing flag
// definition must not turn into an error on the reply path.
func oaicaVerboseRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("verbose")
	return err == nil && v
}

func NewCreateRequest(name string, opts runOptions) *api.CreateRequest {
	parentModel := opts.ParentModel

	modelName := model.ParseName(parentModel)
	if !modelName.IsValid() {
		parentModel = ""
	}

	// Preserve explicit cloud intent for sessions started with `:cloud`.
	// Cloud model metadata can return a source-less parent_model (for example
	// "qwen3.5"), which would otherwise make `/save` create a local derivative.
	if modelref.HasExplicitCloudSource(opts.Model) && !modelref.HasExplicitCloudSource(parentModel) {
		parentModel = ""
	}

	req := &api.CreateRequest{
		Model: name,
		From:  cmp.Or(parentModel, opts.Model),
	}

	if opts.System != "" {
		req.System = opts.System
	}

	if len(opts.Options) > 0 {
		req.Parameters = opts.Options
	}

	messages := slices.Clone(opts.LoadedMessages)
	messages = append(messages, opts.Messages...)
	if len(messages) > 0 {
		req.Messages = messages
	}

	return req
}

func normalizeFilePath(fp string) string {
	return strings.NewReplacer(
		"\\ ", " ", // Escaped space
		"\\(", "(", // Escaped left parenthesis
		"\\)", ")", // Escaped right parenthesis
		"\\[", "[", // Escaped left square bracket
		"\\]", "]", // Escaped right square bracket
		"\\{", "{", // Escaped left curly brace
		"\\}", "}", // Escaped right curly brace
		"\\$", "$", // Escaped dollar sign
		"\\&", "&", // Escaped ampersand
		"\\;", ";", // Escaped semicolon
		"\\'", "'", // Escaped single quote
		"\\\\", "\\", // Escaped backslash
		"\\*", "*", // Escaped asterisk
		"\\?", "?", // Escaped question mark
		"\\~", "~", // Escaped tilde
	).Replace(fp)
}

func extractFileNames(input string) []string {
	// Regex to match file paths starting with optional drive letter, / ./ \ or .\ and include escaped or unescaped spaces (\ or %20)
	// and followed by more characters and a file extension
	// This will capture non filename strings, but we'll check for file existence to remove mismatches
	regexPattern := `(?:[a-zA-Z]:)?(?:\./|/|\\)[\S\\ ]+?\.(?i:jpg|jpeg|png|webp|wav)\b`
	re := regexp.MustCompile(regexPattern)

	return re.FindAllString(input, -1)
}

func extractFileData(input string) (string, []api.ImageData, error) {
	filePaths := extractFileNames(input)
	var imgs []api.ImageData

	for _, fp := range filePaths {
		nfp := normalizeFilePath(fp)
		data, err := getImageData(nfp)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't process file: %q\n", err)
			return "", imgs, err
		}
		ext := strings.ToLower(filepath.Ext(nfp))
		switch ext {
		case ".wav":
			fmt.Fprintf(os.Stderr, "Added audio '%s'\n", nfp)
		default:
			fmt.Fprintf(os.Stderr, "Added image '%s'\n", nfp)
		}
		input = strings.ReplaceAll(input, "'"+nfp+"'", "")
		input = strings.ReplaceAll(input, "'"+fp+"'", "")
		input = strings.ReplaceAll(input, fp, "")
		imgs = append(imgs, data)
	}
	return strings.TrimSpace(input), imgs, nil
}

// resolveEditorCommand is the editor command line and its arguments, from
// OLLAMA_EDITOR, VISUAL, EDITOR, then the platform default.
//
// Each source is judged by what it SPLITS into, not by whether it is empty
// (2026-09-26 audit): a variable set to a space or a newline is not "", so the
// old chain passed it through, strings.Fields returned no words, and
// [0] panicked — Ctrl+G took the interactive session down instead of falling
// back to the default editor. The returned slice is never empty, which is what
// the caller indexes.
func resolveEditorCommand() []string {
	for _, candidate := range []string{envconfig.Editor(), os.Getenv("VISUAL"), os.Getenv("EDITOR"), defaultEditor} {
		if fields := strings.Fields(candidate); len(fields) > 0 {
			return fields
		}
	}
	return strings.Fields(defaultEditor)
}

func editInExternalEditor(content string) (string, error) {
	editor := resolveEditorCommand()

	// Check that the editor binary exists
	name := editor[0]
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("editor %q not found, set OLLAMA_EDITOR to the path of your preferred editor", name)
	}

	tmpFile, err := os.CreateTemp("", "ollama-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if content != "" {
		if _, err := tmpFile.WriteString(content); err != nil {
			tmpFile.Close()
			return "", fmt.Errorf("writing to temp file: %w", err)
		}
	}
	tmpFile.Close()

	args := append(editor, tmpFile.Name())
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %w", err)
	}

	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return "", fmt.Errorf("reading temp file: %w", err)
	}

	return strings.TrimRight(string(data), "\n"), nil
}

func getImageData(filePath string) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	buf := make([]byte, 512)
	_, err = file.Read(buf)
	if err != nil {
		return nil, err
	}

	contentType := http.DetectContentType(buf)
	allowedTypes := []string{"image/jpeg", "image/jpg", "image/png", "image/webp", "audio/wave"}
	if !slices.Contains(allowedTypes, contentType) {
		return nil, fmt.Errorf("invalid file type: %s", contentType)
	}

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	var maxSize int64 = 100 * 1024 * 1024 // 100MB
	if info.Size() > maxSize {
		return nil, errors.New("file size exceeds maximum limit (100MB)")
	}

	buf = make([]byte, info.Size())
	_, err = file.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	_, err = io.ReadFull(file, buf)
	if err != nil {
		return nil, err
	}

	return buf, nil
}
