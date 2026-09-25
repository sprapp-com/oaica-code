package launch

// remote_key_prompt.go — interactive "enter your API key" fallback when a
// selected remote has no key on file yet. Fully generic: no per-provider
// list here. Any remote (builtin from provider_catalog.go, or hand-added
// to ~/.oaica/remotes.json with a base_url but no api_key/api_key_env yet)
// gets this treatment the moment it's selected and its resolved key is
// empty. Adding a new provider or subscription plan never touches this
// file — it inherits the prompt for free.
//
// Hooked once, in launchAfterConfiguration (launch.go), right after the
// picker has resolved a single model and right before Run() — never inside
// resolveRemoteEndpoint or the picker's concurrent model-listing sweep,
// both of which run without a guarantee of being on the interactive
// terminal's goroutine.

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// ensureRemoteAPIKeyForModel checks whether modelName resolves to a user
// remote with no key yet, and if so prompts for one (hidden input) and
// persists it to ~/.oaica/remotes.json via RemoteAdd so future launches
// don't ask again. modelName is the full picker name
// ("zai-coding-plan/glm-5.3"); anything else (local models, a remote that
// already has a key, an integration with no model argument) is a no-op.
func ensureRemoteAPIKeyForModel(modelName string) error {
	remote, _, ok := findUserRemoteForModel(modelName)
	if !ok || remote.key() != "" {
		return nil
	}
	// A remote with NO credential mechanism at all (neither an
	// api_key_env nor an api_key — e.g. a plain localhost vLLM/llama server
	// living behind a tunnel, like this box's own `oaica serve` backends)
	// is genuinely unauthenticated, not just missing its key: asking for
	// one would block what should work as-is.
	if remote.APIKeyEnv == "" && remote.APIKey == "" {
		return nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		hint := remote.APIKeyEnv
		if hint == "" {
			hint = "an api_key in ~/.oaica/remotes.json"
		} else {
			hint = "set " + hint
		}
		return fmt.Errorf("%s requires an API key: %s, or run `oaica launch` interactively once to enter it", remote.Name, hint)
	}

	key, err := promptRemoteAPIKey(remote)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("%s requires an API key", remote.Name)
	}

	if _, err := RemoteAdd(RemoteAddOptions{
		Name:    remote.Name,
		BaseURL: remote.BaseURL,
		APIKey:  key,
		Wire:    remote.Wire,
		Version: remote.Version,
	}); err != nil {
		return fmt.Errorf("saving %s API key: %w", remote.Name, err)
	}
	fmt.Fprintf(os.Stderr, "%sSaved %s API key to ~/.oaica/remotes.json%s\n", ansiGreen, remote.Name, ansiReset)
	return nil
}

func promptRemoteAPIKey(remote userRemote) (string, error) {
	fmt.Fprintf(os.Stderr, "%s has no API key on file.\n", remote.Name)
	if url := providerKeyURL(remote.Name); url != "" {
		fmt.Fprintf(os.Stderr, "Get one at %s\n", hyperlink(url, url))
	}
	envHint := remote.APIKeyEnv
	if envHint == "" {
		envHint = "an env var"
	}
	fmt.Fprintf(os.Stderr, "Enter %s API key (input hidden, or set %s and re-run): ", remote.Name, envHint)
	key, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}
