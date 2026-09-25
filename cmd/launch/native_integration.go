package launch

// native_integration.go — the table of "run the vendor's own CLI and let it
// spend its own credential" integrations.
//
// A native row exists for a tool whose paid access has NO usable API key:
// Claude Code on a subscription reads ~/.claude/.credentials.json, Codex on a
// ChatGPT plan reads ~/.codex/auth.json (auth_mode "chatgpt", a Responses-API
// backend behind an account header). There is no base_url oaica could point
// such a tool at and no bearer `oaica auth login` could store — the only
// honest integration is to exec the vendor binary with an untouched
// environment and let its own login pay. The same reasoning as the external
// credential stores (auth_external.go), one step further: those tools already
// solved their own auth, so oaica reuses it instead of re-developing it.
//
// Each entry below is DATA: the picker rows, the id prefixes that select it,
// how a picker id maps to a vendor model value, and the argv builder. Adding
// the next such tool (Gemini CLI, Copilot CLI, ...) is one entry plus its
// runNative method — not a new pair of isNative*/pickerModels* helpers and not
// another `if commandName == "..."` in the picker.
//
// The wrappers below (isNativeClaudeModel, nativeCodexModelID, ...) keep the
// names the rest of the package already calls; they are lookups over this
// table, so a new tool needs no edits at their call sites.

import "strings"

type nativeIntegration struct {
	// Tool is the integration name the launcher dispatches on
	// (selectSingleModelWithSelectorReady's commandName) — also what
	// nativePickerItemsFor matches.
	Tool string
	// Binary is the executable the integration runs, named in errors.
	Binary string
	// Prefixes are the picker-id prefixes that select this integration. More
	// than one where a provider alias helps search: claude accepts
	// "anthropic/<tier>" as well, because typing "anthropic" otherwise
	// surfaces only aggregator rows.
	Prefixes []string
	// Models are the picker's rows, in display order.
	Models []ModelItem
	// ModelValue maps a picker id to the value the vendor CLI wants. ok is
	// false when the id does not select this integration. A prefix with
	// nothing after it yields "" — the vendor's own config then decides,
	// which is what a bare "codex/native" must do.
	ModelValue func(name string) (string, bool)
	// Args builds the native argv from that value and the user's extra args.
	// It must never emit oaica's own profile/env flags, or the run would bill
	// the router instead of the user's plan (asserted in tests).
	Args func(value string, extra []string) []string
}

// nativeIntegrations is the registry. Order is display order.
func nativeIntegrations() []nativeIntegration {
	return []nativeIntegration{
		{
			Tool:     "claude",
			Binary:   "claude",
			Prefixes: []string{"claude/", "anthropic/"},
			Models:   nativeClaudePickerModels,
			ModelValue: func(name string) (string, bool) {
				for _, p := range []string{"claude/", "anthropic/"} {
					if rest, ok := strings.CutPrefix(name, p); ok {
						return rest, true
					}
				}
				return "", false
			},
			Args: func(tier string, extra []string) []string {
				if tier != "" && !hasClaudeModelFlag(extra) {
					return append([]string{"--model", tier}, extra...)
				}
				return extra
			},
		},
		{
			Tool:     "codex",
			Binary:   "codex",
			Prefixes: []string{codexNativePrefix},
			Models:   nativeCodexPickerModels,
			ModelValue: func(name string) (string, bool) {
				if name == codexNativePrefix {
					return "", true
				}
				rest, ok := strings.CutPrefix(name, codexNativePrefix+"/")
				if !ok {
					return "", false
				}
				return rest, true
			},
			Args: codexNativeArgs,
		},
	}
}

// nativeIntegrationForTool returns the integration a launcher command name
// selects ("" when that tool has no native path).
func nativeIntegrationForTool(tool string) (nativeIntegration, bool) {
	for _, n := range nativeIntegrations() {
		if n.Tool == tool {
			return n, true
		}
	}
	return nativeIntegration{}, false
}

// nativeIntegrationForModel returns the integration a picker id belongs to,
// plus the vendor-side model value that id stands for.
func nativeIntegrationForModel(name string) (nativeIntegration, string, bool) {
	for _, n := range nativeIntegrations() {
		if value, ok := n.ModelValue(name); ok {
			return n, value, true
		}
	}
	return nativeIntegration{}, "", false
}

// isNativeModel reports whether a picker id selects any native integration —
// the single gate the launcher's readiness checks use (there is no inventory
// row and no OAICA endpoint behind such an id, so nothing is prepared for it).
func isNativeModel(name string) bool {
	_, _, ok := nativeIntegrationForModel(name)
	return ok
}

// nativePickerItemsFor returns the picker rows belonging to a launcher command
// name, so the picker does not need a per-tool branch.
func nativePickerItemsFor(commandName string) []ModelItem {
	n, ok := nativeIntegrationForTool(commandName)
	if !ok {
		return nil
	}
	return n.Models
}

// isNativeClaudeModel reports whether name selects the native (non-OAICA)
// Claude Code path: "claude/<tier>" or its "anthropic/<tier>" alias.
func isNativeClaudeModel(model string) bool {
	n, _, ok := nativeIntegrationForModel(model)
	return ok && n.Tool == "claude"
}

// isNativeCodexModel reports whether name selects the native Codex path.
func isNativeCodexModel(model string) bool {
	n, _, ok := nativeIntegrationForModel(model)
	return ok && n.Tool == "codex"
}

// nativeCodexModelID is the model id to pass to `codex -m`, or "" to let
// Codex's own config choose. "codex/native" → "", "codex/native/gpt-6-sol" →
// "gpt-6-sol".
func nativeCodexModelID(name string) string {
	n, value, ok := nativeIntegrationForModel(name)
	if !ok || n.Tool != "codex" {
		return ""
	}
	return value
}

// nativeClaudeModelTier splits "claude/opus" (or "anthropic/opus") into the
// Claude Code --model alias ("opus"). ok is false for anything that is not a
// native Claude entry.
func nativeClaudeModelTier(model string) (string, bool) {
	n, value, ok := nativeIntegrationForModel(model)
	if !ok || n.Tool != "claude" {
		return "", false
	}
	return value, true
}

// nativeToolForModel names the integration a picker id belongs to ("" when it
// is not a native id) — used for messages that should name the vendor tool.
func nativeToolForModel(model string) string {
	n, _, ok := nativeIntegrationForModel(model)
	if !ok {
		return ""
	}
	return n.Tool
}

// hasNativePrefix reports whether any integration claims a prefix of name.
// Kept for callers that need the cheap check without extracting a value.
func hasNativePrefix(name string) bool {
	for _, n := range nativeIntegrations() {
		for _, p := range n.Prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
	}
	return false
}
