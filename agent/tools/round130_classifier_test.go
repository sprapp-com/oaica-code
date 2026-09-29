package tools

import (
	"os"
	"path/filepath"
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

// F131-L1-1/2/4/5 (2026-09-29 audit, round 131).
func TestRound131ClassifierRefusesPartlyQuotedAndBackslashGlobs(t *testing.T) {
	for _, c := range []string{
		`cat ~/.ssh/"id_"*`, `cat ~/".ssh"/id_*`, `cat ~/.ssh/id_'rs'?`,
		`gc ~\.ssh\id_*`, `cat ~\.ssh\id_*`, `Get-Content $HOME\.ssh\id_rs?`, `type %USERPROFILE%\.ssh\id_*`,
		`cat ~/.ss\h/id_rsa`, `cat ~/.ollama/id_ed25519`, `cat ~/.oaica/api_key`, `cat ~/.oaica/remotes.json`,
	} {
		if rejectUnsafeShellCommand(c) == nil {
			t.Errorf("%q must be refused", c)
		}
	}
}

func TestRound131PublicKeysAreReadable(t *testing.T) {
	for _, c := range []string{"cat ~/.ssh/id_ed25519.pub", "cat ~/.ssh/id_rsa.pub | pbcopy", "grep -o '.*' file.txt"} {
		if err := rejectUnsafeShellCommand(c); err != nil {
			t.Errorf("%q must pass: %v", c, err)
		}
	}
	if err := refuseCredentialPath("/tmp", "/home/u/.ssh/id_ed25519.pub"); err != nil {
		t.Errorf("Read of a .pub refused: %v", err)
	}
	if err := refuseCredentialPath("/tmp", "/home/u/.ssh/id_ed25519"); err == nil {
		t.Error("Read of the private key must be refused")
	}
	if err := refuseCredentialPath("/tmp", "/home/u/.ssh/id_rsa.pub.bak"); err == nil {
		t.Error("a .pub.bak is not the public key")
	}
}

// F132-L1-1..4 (2026-09-29 audit, round 132).
func TestRound132ProjectFilesNamedLikeProductStoresPass(t *testing.T) {
	for _, c := range []string{
		"cat src/.oaica/api_key_docs.md", "cat project/.oaica/api_key.txt", "sed -n 1,5p .oaica/remotes.json.example",
		"cat ./.oaica/remotes.json", "cat src/api_key.go", "cat ~/.oaica/api_key_docs.md",
	} {
		if err := rejectUnsafeShellCommand(c); err != nil {
			t.Errorf("%q must pass: %v", c, err)
		}
	}
	for _, c := range []string{"cat ~/.oaica/api_key", "cat ~/.oaica/remotes.json.bak.1", "cat $HOME/.oaica/local_servers.json", "cat ${HOME}/.oaica/api_key"} {
		if rejectUnsafeShellCommand(c) == nil {
			t.Errorf("%q must be refused", c)
		}
	}
}

func TestRound132QuotingFormsAndCdCannotHideTheKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("k"), 0o600)
	for _, c := range []string{
		`cat ~/.ssh/$'id_rsa'`, `cat ~/.ssh/id_$'rsa'`, `cat ~/.ssh/$"id_rsa"`, `cat ~/.ssh/$'\x69d_rsa'`, `cat ~/.ss${x}h/id_rsa`,
	} {
		if rejectUnsafeShellCommand(c) == nil {
			t.Errorf("%q must be refused", c)
		}
	}
	for _, c := range []string{"cd ~/.ssh && cat id_rsa", "cd ~/.ssh; cat id_*", "cd " + filepath.Join(home, ".ssh") + " && cat id_rsa"} {
		if refuseCredentialWords(t.TempDir(), c) == nil {
			t.Errorf("%q must be refused after the cd", c)
		}
	}
	if err := refuseCredentialWords(t.TempDir(), "cd src && cat main.go"); err != nil {
		t.Errorf("ordinary cd refused: %v", err)
	}
}
