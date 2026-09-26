package launch

// picker_fingerprint_cloud_limits_test.go — the picker's cache is voided when
// any input that decided its rows changes (pickerCacheInputPaths, hashed by
// pickerInputFingerprint). The synced cloud-limits catalog
// (~/.oaica/cache/cloud_limits/cloud_limits.json) was not among them, so
// `oaica model cloud-limits sync` — which changes exactly the windows a row is
// sized against, and the CLAUDE_CODE_* pair a launch exports — could not
// invalidate the cached menu (2026-09-26 audit, third round).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloudLimitsSyncInvalidatesThePickerCache(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	path, err := cloudLimitsCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	before := pickerInputFingerprint()[path]

	// A sync lands new numbers.
	if err := os.WriteFile(path, []byte(`{"version":9,"limits":{"kimi-k2.6":{"context":300000,"output":131072}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	after := pickerInputFingerprint()[path]
	if _, ok := pickerInputFingerprint()[path]; !ok {
		t.Errorf("the cloud-limits catalog at %s is not a picker cache input: %v — a sync changes the windows on the rows it paints, and the cache keeps the old menu until its TTL expires", path, pickerCacheInputPaths())
	}
	if before == after {
		t.Errorf("fingerprint for the cloud-limits catalog did not change after a sync (%q) — the picker cache cannot notice it", after)
	}
}
