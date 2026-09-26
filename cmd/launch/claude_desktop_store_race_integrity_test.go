package launch

// claude_desktop_store_race_integrity_test.go — six writers publish Claude
// Desktop's own JSON documents, each reading the whole document, setting the
// keys it owns and writing the whole document back, with no lock anywhere:
// writeClaudeDesktopDeploymentMode, writeClaudeDesktopMeta,
// writeClaudeDesktopGatewayProfile, restoreClaudeDesktopMeta and
// restoreClaudeDesktopOllamaProfile over …/Claude*/claude_desktop_config.json
// and …/Claude-3p/configLibrary/{_meta.json,<profile id>.json}, plus
// writeClaudeDesktopRestoreState over oaica's own
// ~/.ollama/launch/claude-desktop-restore.json.
//
// Two overlapping `oaica launch claude-desktop` commands therefore each publish
// a snapshot taken before the other's keys landed, and the publish that renames
// last decides the file while both report success. For the restore state that
// is worse than a lost key: the state is a before-snapshot of a credential oaica
// is about to overwrite, so the record a lost publish deletes is the only copy
// of the user's own key — --restore then leaves oaica's secret on disk or
// deletes the user's (2026-09-26 audit, thirteenth round).
//
// The Claude Desktop documents are not oaica's, so their locks are keyed under
// ~/.oaica/locks (foreignStoreLockBase); the restore state is oaica's own, so its
// lock goes beside the file. Every function that touches one document takes the
// same lock for it (that is what makes the write and restore paths order against
// each other), and the two entry points take the restore-state lock first and the
// document locks inside it — the same order in both, so no deadlock.
//
// The Claude Desktop profile layout these cases pin is macOS's: it is entirely
// HOME-derived, which is what makes it reproducible on the Linux CI host, and it
// is resolved by the production code with claudeDesktopGOOS pinned to darwin —
// the same way claude_desktop_test.go exercises it.
//
// The machinery lives in agent_config_store_race_test.go.

import (
	"errors"
	"path/filepath"
	"testing"
)

// claudeDesktopRaceThirdPartyPaths is the document set the production code
// resolves for Claude Desktop's third-party profile root with macOS pinned: the
// child writer below derives its paths from HOME through exactly this call, and
// the cases derive their store from it too, so the two can never drift.
func claudeDesktopRaceThirdPartyPaths() (claudeDesktopThirdPartyPaths, error) {
	old := claudeDesktopGOOS
	claudeDesktopGOOS = "darwin"
	defer func() { claudeDesktopGOOS = old }()

	targets, err := claudeDesktopTargetPaths()
	if err != nil {
		return claudeDesktopThirdPartyPaths{}, err
	}
	if len(targets.thirdPartyProfiles) == 0 {
		return claudeDesktopThirdPartyPaths{}, errors.New("no Claude Desktop third-party profile root")
	}
	return targets.thirdPartyProfiles[0], nil
}

// claudeDesktopRaceStoreRel is the store path a case declares — as the harness
// wants it, relative to the hermetic HOME — for one document of that set.
func claudeDesktopRaceStoreRel(t *testing.T, pick func(claudeDesktopThirdPartyPaths) string) string {
	t.Helper()
	home := t.TempDir()
	old := claudeDesktopUserHome
	claudeDesktopUserHome = func() (string, error) { return home, nil }
	defer func() { claudeDesktopUserHome = old }()

	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, pick(target))
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func runClaudeDesktopMetaWriter(id string) error {
	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		return err
	}
	return writeClaudeDesktopMeta(target.meta, id, claudeDesktopProfileName)
}

func runClaudeDesktopRestoreMetaWriter() error {
	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		return err
	}
	return restoreClaudeDesktopMeta(target.meta)
}

func runClaudeDesktopProfileWriter() error {
	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		return err
	}
	return writeClaudeDesktopGatewayProfile(target.profile, "sk-race-key", true)
}

func runClaudeDesktopRestoreProfileWriter() error {
	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		return err
	}
	return restoreClaudeDesktopOllamaProfile(target.profile, claudeDesktopProfileRestoreState{}, false)
}

func runClaudeDesktopDeploymentModeWriter() error {
	target, err := claudeDesktopRaceThirdPartyPaths()
	if err != nil {
		return err
	}
	return writeClaudeDesktopDeploymentMode(target.desktopConfig, "3p")
}

// runClaudeDesktopRestoreStateWriter writes through the shared locked
// load-mutate-save both Claude Desktop entry points run (ConfigureAutodiscovery
// and Restore), with the record ConfigureAutodiscovery adds: a profile entry
// keyed by the profile path it is about to overwrite
// (recordClaudeDesktopGatewayKeyInjection). The key is this child's id, so the
// case can name it.
func runClaudeDesktopRestoreStateWriter(id string) error {
	return updateClaudeDesktopRestoreState(func(state *claudeDesktopRestoreState) error {
		if state.Profiles == nil {
			state.Profiles = make(map[string]claudeDesktopProfileRestoreState)
		}
		state.Profiles[id] = claudeDesktopProfileRestoreState{HadKey: true}
		return nil
	})
}

func TestClaudeDesktopMetaTakesConfigLibraryLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "writeClaudeDesktopMeta",
		kind:    agentStoreClaudeDesktopMeta,
		id:      claudeDesktopProfileID,
		store:   claudeDesktopRaceStoreRel(t, func(p claudeDesktopThirdPartyPaths) string { return p.meta }),
		foreign: true,
		seed: `{"appliedId": "another-profile", "entries": [{"id": "another-profile", "name": "Other"}], ` +
			`"window": {"zoom": 1.5}}`,
		other: `{"appliedId": "another-profile", "entries": [{"id": "another-profile", "name": "Other"}], ` +
			`"window": {"zoom": 1.5}, "concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"appliedId": "` + claudeDesktopProfileID + `"`,
			`"name": "` + claudeDesktopProfileName + `"`,
			// The profile the user already had is not this writer's to drop.
			`"another-profile"`,
			`"zoom": 1.5`,
		},
		gone: []string{`"appliedId": "another-profile"`},
	})
}

func TestClaudeDesktopRestoreMetaTakesTheSameConfigLibraryLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "restoreClaudeDesktopMeta",
		kind:    agentStoreClaudeDesktopRestMeta,
		id:      claudeDesktopProfileID,
		store:   claudeDesktopRaceStoreRel(t, func(p claudeDesktopThirdPartyPaths) string { return p.meta }),
		foreign: true,
		seed: `{"appliedId": "` + claudeDesktopProfileID + `", ` +
			`"entries": [{"id": "` + claudeDesktopProfileID + `", "name": "` + claudeDesktopProfileName + `"}], ` +
			`"window": {"zoom": 1.5}}`,
		other: `{"appliedId": "` + claudeDesktopProfileID + `", ` +
			`"entries": [{"id": "` + claudeDesktopProfileID + `", "name": "` + claudeDesktopProfileName + `"}], ` +
			`"window": {"zoom": 1.5}, "concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"zoom": 1.5`,
		},
		gone: []string{`"appliedId"`},
	})
}

func TestClaudeDesktopGatewayProfileTakesProfileLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "writeClaudeDesktopGatewayProfile",
		kind:    agentStoreClaudeDesktopProfile,
		id:      claudeDesktopProfileID,
		store:   claudeDesktopRaceStoreRel(t, func(p claudeDesktopThirdPartyPaths) string { return p.profile }),
		foreign: true,
		seed:    `{"inferenceProvider": "firstParty", "window": {"zoom": 1.5}}`,
		other: `{"inferenceProvider": "firstParty", "window": {"zoom": 1.5}, ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"inferenceProvider": "gateway"`,
			`"inferenceGatewayApiKey": "sk-race-key"`,
			`"zoom": 1.5`,
		},
		gone: []string{`"firstParty"`},
	})
}

func TestClaudeDesktopRestoreProfileTakesTheSameProfileLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "restoreClaudeDesktopOllamaProfile",
		kind:    agentStoreClaudeDesktopRestProf,
		id:      claudeDesktopProfileID,
		store:   claudeDesktopRaceStoreRel(t, func(p claudeDesktopThirdPartyPaths) string { return p.profile }),
		foreign: true,
		seed: `{"inferenceProvider": "gateway", "inferenceGatewayBaseUrl": "https://ollama.com", ` +
			`"inferenceGatewayApiKey": "sk-race-key", "window": {"zoom": 1.5}}`,
		other: `{"inferenceProvider": "gateway", "inferenceGatewayBaseUrl": "https://ollama.com", ` +
			`"inferenceGatewayApiKey": "sk-race-key", "window": {"zoom": 1.5}, ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			// No record at all (recorded=false) means oaica never wrote a key
			// here, so the key stays where it is.
			`"inferenceGatewayApiKey": "sk-race-key"`,
			`"zoom": 1.5`,
		},
		gone: []string{`"inferenceProvider"`, `"inferenceGatewayBaseUrl"`},
	})
}

func TestClaudeDesktopDeploymentModeTakesDesktopConfigLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name:    "writeClaudeDesktopDeploymentMode",
		kind:    agentStoreClaudeDesktopMode,
		id:      claudeDesktopProfileID,
		store:   claudeDesktopRaceStoreRel(t, func(p claudeDesktopThirdPartyPaths) string { return p.desktopConfig }),
		foreign: true,
		seed:    `{"deploymentMode": "1p", "window": {"zoom": 1.5}}`,
		other: `{"deploymentMode": "1p", "window": {"zoom": 1.5}, ` +
			`"concurrentWriter": "kept"}`,
		want: []string{
			`"concurrentWriter": "kept"`,
			`"deploymentMode": "3p"`,
			`"zoom": 1.5`,
		},
		gone: []string{`"1p"`},
	})
}

func TestClaudeDesktopRestoreStateTakesItsOwnStoreLock(t *testing.T) {
	runAgentStoreExclusion(t, agentStoreCase{
		name: "updateClaudeDesktopRestoreState",
		kind: agentStoreClaudeDesktopRestState,
		id:   "/race/profile.json",
		// oaica's own bookkeeping file, inside the launcher's directory tree.
		store:   filepath.Join(".ollama", "launch", "claude-desktop-restore.json"),
		foreign: false,
		seed:    `{"profiles": {"/seed/profile.json": {"had_key": true}}}`,
		other: `{"profiles": {"/seed/profile.json": {"had_key": true}, ` +
			`"/concurrent/other.json": {"had_key": true}}}`,
		want: []string{
			// The record another command published while this one held the lock.
			`"/concurrent/other.json"`,
			// The record only this writer adds.
			`"/race/profile.json"`,
		},
	})
}
