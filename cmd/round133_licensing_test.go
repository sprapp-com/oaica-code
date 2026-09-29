package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round 133: the MIT notices travel with every copy, and the licensor is named where the licence says it is.
func TestRound133LicenceNoticesShipWithEveryArchive(t *testing.T) {
	root := filepath.Join("..")
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return string(b)
	}
	lic := read("LICENSE")
	if !strings.Contains(lic, "Copyright (c) Ollama") || !strings.Contains(lic, "BizTransit Sdn Bhd (891234-X)") {
		t.Error("LICENSE must keep the upstream notice and add BizTransit's")
	}
	if n := read("NOTICE"); !strings.Contains(n, "Ollama") || !strings.Contains(n, "BizTransit Sdn Bhd") {
		t.Error("NOTICE must name Ollama and BizTransit")
	}
	script := read("scripts/build_oaica.sh")
	for _, want := range []string{"LICENSE", "NOTICE", "LICENSING.md", "THIRD_PARTY_MODULES.txt"} {
		if strings.Count(script, want) < 4 { // cp + tar + tgz + zip
			t.Errorf("scripts/build_oaica.sh must put %s in every archive kind", want)
		}
	}
	if !strings.Contains(read("docs/LICENSING.md"), "891234-X") {
		t.Error("docs/LICENSING.md must name the licensor's company number")
	}
}
