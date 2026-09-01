package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDbgBackupFail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	path := filepath.Join(tmpDir, "config.json")
	os.WriteFile(path, []byte(`{}`), 0o644)
	backupDir := BackupDir()
	t.Logf("backupDir=%s", backupDir)
	os.MkdirAll(backupDir, 0o755)
	os.Chmod(backupDir, 0o444)
	fi, _ := os.Stat(backupDir)
	t.Logf("mode=%v perm&077=%o", fi.Mode(), fi.Mode().Perm()&0o077)
	f, err := os.Create(filepath.Join(backupDir, "probe"))
	t.Logf("create err=%v", err)
	if f != nil {
		f.Close()
	}
}
