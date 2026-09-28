package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file://" + p
}

func TestCatalogSync_AdoptsAPassingPayload(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	rep, err := CatalogSync(writeCatalogFile(t, modelsDevFixture))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Refused || rep.Providers != 3 {
		t.Fatalf("report = %+v", rep)
	}
	path, err := catalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
		t.Fatalf("cache not written: %v", err)
	}
}

// The refusal path is the whole safety story for Plan A: a payload whose shape
// we cannot trust must not replace the last good copy.
func TestCatalogSync_ContractFailureKeepsLastGood(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if _, err := CatalogSync(writeCatalogFile(t, modelsDevFixture)); err != nil {
		t.Fatal(err)
	}
	path, _ := catalogCachePath()
	good, _ := os.ReadFile(path)

	bad := `{"p":{"id":"p","models":{"m":{"id":"m","limit":{"context":"163840"}}}}}`
	rep, err := CatalogSync(writeCatalogFile(t, bad))
	if err == nil {
		t.Fatalf("a contract failure must be an error, got %+v", rep)
	}
	if !rep.Refused {
		t.Fatalf("report must say refused: %+v", rep)
	}
	if !strings.Contains(err.Error(), "providers.p.models.m.limit.context") {
		t.Fatalf("error must name the field: %v", err)
	}
	now, _ := os.ReadFile(path)
	if string(now) != string(good) {
		t.Fatal("a refused payload overwrote the last good catalog")
	}
}

func TestCatalogSync_UnparseablePayloadIsRefusedAndNotAdopted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	rep, err := CatalogSync(writeCatalogFile(t, "not json at all"))
	if err == nil || !rep.Refused {
		t.Fatalf("unparseable payload: rep=%+v err=%v", rep, err)
	}
	if _, statErr := os.Stat(mustCatalogPath(t)); !os.IsNotExist(statErr) {
		t.Fatal("an unparseable payload must not be cached")
	}
}

func TestCatalogSync_IdenticalResyncIsAnUnchangedNoop(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	src := writeCatalogFile(t, modelsDevFixture)
	if _, err := CatalogSync(src); err != nil {
		t.Fatal(err)
	}
	rep, err := CatalogSync(src)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Unchanged {
		t.Fatalf("identical bytes must report unchanged: %+v", rep)
	}
}

func TestLoadModelsDevCatalog_AbsentIsNotAnError(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if _, ok := loadModelsDevCatalog(); ok {
		t.Fatal("no catalog on disk must report ok=false, not a fabricated one")
	}
}

func mustCatalogPath(t *testing.T) string {
	t.Helper()
	p, err := catalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
