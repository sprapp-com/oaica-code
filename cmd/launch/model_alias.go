package launch

// model_alias.go — user-defined shortcuts for launch model names
// (~/.oaica/aliases.json). Exists so a memorable short name ("glm") can
// point at a full, possibly-awkward id ("ollama/glm-5.3-flash:cloud",
// "openrouter/anthropic/claude-sonnet-4") without editing remotes.json or
// waiting on anyone else to fix/rename anything — the same "don't wait for
// intervention" motivation as `oaica model refresh`.
//
// Resolution is a single flat lookup, applied FIRST in resolveLaunchEndpoint
// (before remotes/router/daemon) — an alias always wins if defined, on the
// theory that a user who explicitly aliased a name wants exactly that
// target, not a different thing that happens to share the bare id. Aliases
// are NEVER auto-populated or auto-updated by any refresh/discovery path;
// they only change when the user runs `oaica model alias`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type modelAliases struct {
	Version int               `json:"version"`
	Aliases map[string]string `json:"aliases"`
}

const modelAliasesVersion = 1

func modelAliasesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "aliases.json"), nil
}

// ModelAliasesPath is the exported form, for cmd/cmd.go's CLI to print in
// "no aliases defined" messages.
func ModelAliasesPath() (string, error) {
	return modelAliasesPath()
}

func loadModelAliases() (*modelAliases, error) {
	path, err := modelAliasesPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &modelAliases{Version: modelAliasesVersion, Aliases: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var a modelAliases
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if a.Aliases == nil {
		a.Aliases = map[string]string{}
	}
	return &a, nil
}

func (a *modelAliases) save() error {
	path, err := modelAliasesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if a.Version == 0 {
		a.Version = modelAliasesVersion
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ModelAliasSet creates or replaces an alias. target is stored verbatim
// (not validated against any live source) — the whole point is working
// without waiting on discovery/refresh to catch up.
func ModelAliasSet(name, target string) error {
	name = strings.TrimSpace(name)
	target = strings.TrimSpace(target)
	if name == "" {
		return errors.New("alias name is required")
	}
	if target == "" {
		return errors.New("--target is required")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("alias name %q must not contain '/' — that would be ambiguous with <remote>/<id> resolution", name)
	}
	a, err := loadModelAliases()
	if err != nil {
		return err
	}
	a.Aliases[name] = target
	return a.save()
}

// ModelAliasRemove deletes an alias, reporting whether it existed.
func ModelAliasRemove(name string) (bool, error) {
	a, err := loadModelAliases()
	if err != nil {
		return false, err
	}
	if _, ok := a.Aliases[name]; !ok {
		return false, nil
	}
	delete(a.Aliases, name)
	return true, a.save()
}

// resolveModelAlias returns (target, true) if name is a defined alias.
// Package var so tests can stub it; production always goes through
// loadModelAliases.
var resolveModelAlias = defaultResolveModelAlias

func defaultResolveModelAlias(name string) (string, bool) {
	a, err := loadModelAliases()
	if err != nil {
		return "", false
	}
	target, ok := a.Aliases[name]
	return target, ok
}

// ModelAliasSortedNames returns alias names in stable alphabetical order.
func ModelAliasSortedNames() ([]string, error) {
	a, err := loadModelAliases()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(a.Aliases))
	for n := range a.Aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// ModelAliasGet resolves one alias, or an error naming the aliases file if
// not found.
func ModelAliasGet(name string) (string, error) {
	a, err := loadModelAliases()
	if err != nil {
		return "", err
	}
	target, ok := a.Aliases[name]
	if !ok {
		path, _ := modelAliasesPath()
		return "", fmt.Errorf("no alias named %q in %s — create one with `oaica model alias %s --target <id>`", name, path, name)
	}
	return target, nil
}
