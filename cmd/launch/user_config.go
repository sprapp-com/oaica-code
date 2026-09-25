package launch

// user_config.go — the user's standing launch preferences, ~/.oaica/config.json.
// Deliberately tiny: two keys (sonnet_model, haiku_model), read at launch as
// the DEFAULT tier split so the user can define their own tiers once and have
// every subsequent `oaica launch claude` honor it without flags.
//
// Precedence for the sonnet tier (highest wins):
//
//	1. --sonnet-model flag        (explicit, per-launch)
//	2. --plan <name>'s SonnetModel (explicit, saved plan)
//	3. config.json's sonnet_model  (standing user preference)   <- here
//	4. wizard / same-as-primary    (interactive default)
//
// The haiku tier follows the same ladder with --haiku-model / HaikuModel /
// haiku_model. It is the tier worth setting: Claude Code sends its background
// work there (conversation titles, topic detection, subagent spawning), so a
// haiku tier left on the primary bills invisible traffic at primary prices —
// see tier_routing.go's buildTierPlan and NativeTiers for the mechanics.
//
// Same atomic-write convention as plans.json / remotes.json.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UserConfig is the user's standing launch preference file.
type UserConfig struct {
	// SonnetModel is the default sonnet/subagent-tier model for
	// `oaica launch claude`. Empty = no standing preference (flag/plan/
	// wizard decide, as before this file existed).
	SonnetModel string `json:"sonnet_model,omitempty"`
	// HaikuModel is the default haiku-tier model — the cheap leg for Claude
	// Code's background traffic. Empty = no standing preference.
	HaikuModel string `json:"haiku_model,omitempty"`
}

func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "config.json"), nil
}

// UserConfigLoad returns the user's config; a missing file is the zero
// config, not an error (first-run state).
func UserConfigLoad() (UserConfig, error) {
	path, err := userConfigPath()
	if err != nil {
		return UserConfig{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return UserConfig{}, nil
	}
	if err != nil {
		return UserConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c UserConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return UserConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// UserConfigPath is the exported config path, for `oaica config show`.
func UserConfigPath() (string, error) { return userConfigPath() }

// UserConfigSetSonnetModel persists the standing sonnet tier; empty string
// clears it.
func UserConfigSetSonnetModel(model string) error {
	return userConfigSet(func(c *UserConfig) { c.SonnetModel = strings.TrimSpace(model) })
}

// UserConfigSetHaikuModel persists the standing haiku tier; empty string
// clears it.
func UserConfigSetHaikuModel(model string) error {
	return userConfigSet(func(c *UserConfig) { c.HaikuModel = strings.TrimSpace(model) })
}

// userConfigSet applies one change and writes the file atomically, leaving
// every other key untouched — the two setters above differ only in which
// field they assign.
func userConfigSet(mutate func(*UserConfig)) error {
	c, err := UserConfigLoad()
	if err != nil {
		return err
	}
	mutate(&c)
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Chmod after the fact too: the 0o600 mode arg only applies at creation,
	// and a pre-existing looser file (audit 2026-09-01) stayed world-readable.
	return os.Chmod(path, 0o600)
}

// UserConfigSonnetModel returns the standing sonnet tier ("" when unset) —
// the launch path reads THIS, never the file directly.
func UserConfigSonnetModel() string {
	c, err := UserConfigLoad()
	if err != nil {
		return "" // unreadable config must not block a launch
	}
	return c.SonnetModel
}

// UserConfigHaikuModel returns the standing haiku tier ("" when unset).
func UserConfigHaikuModel() string {
	c, err := UserConfigLoad()
	if err != nil {
		return "" // unreadable config must not block a launch
	}
	return c.HaikuModel
}
