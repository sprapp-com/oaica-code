package tools

import (
	"testing"
)

// F130-L1-1 / L1-2 / L1-7 (2026-09-29 audit, round 130): round 129's classifier refused ordinary search and
// cleanup commands, and still passed spellings it meant to catch.
func TestRound130ClassifierPassesOrdinaryWork(t *testing.T) {
	for _, c := range []string{
		"grep -rn TODO *", "grep -n foo src/*", "head -n 3 *", "tail -n 50 logs/*", "cat *.* | wc -l",
		"grep -o '.*' file.txt", `awk '$1 ~ /.*/' f`, "find . -name '*' | grep -v node_modules", "git status --porcelain | grep foo; ls *",
		"find . -name '*.o' | xargs rm -rf", "find . -type d -name __pycache__ | xargs rm -rf", "cd ~/proj/build && ls | xargs rm -rf",
	} {
		if err := rejectUnsafeShellCommand(c); err != nil {
			t.Errorf("%q must pass: %v", c, err)
		}
	}
}

func TestRound130ClassifierStillCatchesTheSpellings(t *testing.T) {
	for _, c := range []string{
		`ca\t ~/.ssh/id_rsa`, `ls ~/.ssh; ca\t ~/.ssh/id_rsa`,
		"echo ~ | xargs -0 rm -rf", "echo ~ | xargs -I{} rm -rf {}", "echo / | xargs -r rm -rf", "echo ~ | xargs sudo rm -rf",
		"cat ~/.ssh/id_*", "cat ~/.ssh/id_rs?",
	} {
		if rejectUnsafeShellCommand(c) == nil {
			t.Errorf("%q must be refused", c)
		}
	}
}
