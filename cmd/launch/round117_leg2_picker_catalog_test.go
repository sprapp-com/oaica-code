package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound117CatalogSyncDoesNotVoidPickerCache(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	a := filepath.Join(home, "a.json")
	b := filepath.Join(home, "b.json")
	os.WriteFile(a, []byte(modelsDevFixture), 0o600)
	bb := strings.Replace(modelsDevFixture, `"limit": {"context": 163840, "output": 32768}`, `"limit": {"context": 8192, "output": 1024}`, 1)
	if bb == modelsDevFixture {
		t.Fatal("fixture edit missed")
	}
	os.WriteFile(b, []byte(bb), 0o600)

	if _, err := CatalogSync("file://" + a); err != nil {
		t.Fatal(err)
	}
	rowsA := catalogRowsFor("groq")
	fp := pickerInputFingerprint()
	savePickerCache([]LaunchModel{{Name: "groq/llama-x"}}, fp)

	rep, err := CatalogSync("file://" + b)
	t.Logf("sync B: %+v err=%v", rep, err)
	rowsB := catalogRowsFor("groq")
	t.Logf("groq rows before: %+v", rowsA)
	t.Logf("groq rows after:  %+v", rowsB)
	cp, _ := catalogCachePath()
	_, inFP := pickerInputFingerprint()[cp]
	_, _, ok := loadPickerCache()
	t.Logf("modelsdev.json fingerprinted=%v; picker cache still served after sync=%v", inFP, ok)
	if ok && rowsA[0].Context != rowsB[0].Context {
		t.Errorf("RED: the synced catalog changed the rows' windows (%d -> %d) and the picker cache is still served as valid", rowsA[0].Context, rowsB[0].Context)
	}
}
