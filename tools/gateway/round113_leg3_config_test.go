package main

// round113_leg3_config_test.go — leg 3, round 113 (2026-09-29 audit), F113-L3-2 and
// F113-L3-3.
//
// loadConfig decoded with json.Unmarshal and no strictness, so a typo in a security
// or limit key was silently ignored: `max_concurent` meant no per-key concurrency cap,
// `request_timeout_secs` meant the default timeout, `larg_context_token_threshold`
// meant no admission control, and the gateway started or reloaded and logged success.
// There was also no way to validate a config before deploying it: the only checks were
// to start a process (which opens the ledger and binds the port) or to send SIGHUP and
// read a log line. Unknown keys are now WARNED about on load and reload (not refused:
// a live config may carry a key an older or newer version reads, and refusing on
// reload would take the gateway's last good config away for a comment), and
// `--check` validates a config, lists the unknown keys as failures and the resolved
// limits as a summary, opens nothing and exits 0 or 1 for a deploy script to gate on.

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func r113Config(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	body = strings.ReplaceAll(body, "LEDGER", filepath.Join(dir, "ledger.jsonl"))
	body = strings.ReplaceAll(body, "ERRLOG", filepath.Join(dir, "errors.jsonl"))
	p := filepath.Join(dir, "gw.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func r113Valid(extra string) string {
	return `{"upstream_addr":"http://127.0.0.1:1","ledger_path":"LEDGER","upstream_error_log_path":"ERRLOG",` + extra +
		`"api_keys":[{"label":"k","sha256":"` + keyHash("sk") + `","max_concurrent":3}],` +
		`"models":[{"id":"m","pricing":{"prompt":"0.1","completion":"0.2"}}]}`
}

func TestMine113UnknownConfigKeysAreFoundAtEveryLevel(t *testing.T) {
	raw := `{"upstream_addr":"http://x","request_timeout_secs":5,"larg_context_token_threshold":1,` +
		`"api_keys":[{"label":"k","sha256":"` + keyHash("sk") + `","max_concurent":3}],` +
		`"models":[{"id":"m","max_completion_token":9,"pricing":{"prompt":"1","complettion":"2"}}]}`
	got := strings.Join(unknownConfigKeys([]byte(raw)), ",")
	for _, want := range []string{"request_timeout_secs", "larg_context_token_threshold", "api_keys[0].max_concurent", "models[0].max_completion_token", "models[0].pricing.complettion"} {
		if !strings.Contains(got, want) {
			t.Errorf("the unknown key %q was not found (got %q) — a typo in a limit key must not be silent (2026-09-29 audit, round 113, F113-L3-2)", want, got)
		}
	}
	// Known keys, in any case the decoder accepts, are not reported.
	if k := unknownConfigKeys([]byte(r113Valid(`"request_timeout_sec":30,`))); len(k) != 0 {
		t.Errorf("a valid config reported unknown keys %v (2026-09-29 audit, round 113, F113-L3-2)", k)
	}
	if k := unknownConfigKeys([]byte(`{"Upstream_Addr":"http://x"}`)); len(k) != 0 {
		t.Errorf("a key that differs only in case, which json.Unmarshal accepts, was reported unknown: %v (2026-09-29 audit, round 113, F113-L3-2)", k)
	}
}

func TestMine113ALoadAndReloadWarnAboutUnknownKeys(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	path := r113Config(t, r113Valid(`"request_timeout_secs":5,`))
	g := &gateway{}
	g.reload(path)
	if out := buf.String(); !strings.Contains(out, "request_timeout_secs") || !strings.Contains(strings.ToLower(out), "unknown") {
		t.Errorf("a reload with a typo'd key logged %q — it must say the key is unknown and ignored (2026-09-29 audit, round 113, F113-L3-2)", out)
	}
	if len(g.cfg.Models) == 0 {
		t.Errorf("the reload was refused for an unknown key — it is a warning, the config still applies (2026-09-29 audit, round 113, F113-L3-2)")
	}
}

func TestMine113CheckModeGatesADeploy(t *testing.T) {
	for name, c := range map[string]struct {
		body     string
		want     int
		contains string
	}{
		"valid":            {r113Valid(""), 0, "max_concurrent"},
		"unknown key":      {r113Valid(`"request_timeout_secs":5,`), 1, "request_timeout_secs"},
		"invalid config":   {`{"upstream_addr":"http://x","api_keys":[],"models":[]}`, 1, "api_keys"},
		"NaN price":        {strings.Replace(r113Valid(""), `"prompt":"0.1"`, `"prompt":"NaN"`, 1), 1, "pricing"},
		"unparseable file": {`{not json`, 1, "parse"},
	} {
		var out bytes.Buffer
		if got := runCheck(r113Config(t, c.body), &out); got != c.want {
			t.Errorf("%s: --check exited %d, want %d\n%s (2026-09-29 audit, round 113, F113-L3-3)", name, got, c.want, out.String())
		}
		if !strings.Contains(out.String(), c.contains) {
			t.Errorf("%s: --check output %q does not mention %q (2026-09-29 audit, round 113, F113-L3-3)", name, out.String(), c.contains)
		}
		if strings.Contains(out.String(), keyHash("sk")) {
			t.Errorf("%s: --check printed a key digest (2026-09-29 audit, round 113, F113-L3-3)", name)
		}
	}
	// --check opens nothing: no ledger file appears beside the config.
	cfg := r113Config(t, r113Valid(""))
	var out bytes.Buffer
	runCheck(cfg, &out)
	for _, f := range []string{"ledger.jsonl", "errors.jsonl"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), f)); err == nil {
			t.Errorf("--check created %s (2026-09-29 audit, round 113, F113-L3-3)", f)
		}
	}
}
