// Package fileutil provides small shared helpers for reading JSON files
// and writing config files with backup-on-overwrite semantics.
package fileutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Keep a bounded number of backups per file so config backups do not grow
// without limit. We keep the 5 most recent backups and do not pin the oldest.
const maxBackupsPerFile = 5

// ReadJSON reads a JSON object file into a generic map.
func ReadJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// WriteFileAtomic writes data to path so that no reader ever sees a partial
// or absent file: a UNIQUE temp file in the same directory, fsynced, then
// renamed over the target. Use this — not os.WriteFile — for any state file a
// different process reads (2026-09-26 audit).
//
// Two properties, each of which was a live bug:
//
//   - The rename. os.WriteFile truncates the live path (O_TRUNC), so a crash,
//     a power loss, or a concurrent writer inside that window leaves a
//     zero-byte or half-written file. For ~/.oaica/remotes.json that is not
//     "unset", it is a parse error that kills the remote sweep — every user
//     remote and any inline api_key gone, with no backup copy.
//
//   - The UNIQUE temp name. A fixed "<path>.tmp" is a shared write buffer:
//     two processes open it with O_TRUNC, the first rename publishes whatever
//     that path holds at that instant (including the other writer's
//     half-written bytes) and the second rename fails ENOENT. Reached in
//     practice by two terminals — `oaica plan set` while a launch wizard
//     saves a plan, or `oaica auth login` for two providers at once.
//
// perm is applied to the temp before the rename AND re-asserted on the target
// afterwards: a pre-existing looser file (observed 0664 live, plaintext key
// inside) is replaced by a new inode, but the target is what callers stat.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Chmod(path, perm)
}

// BackupDir returns the shared backup root used before overwriting files.
func BackupDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".ollama", "backup")
	}
	return filepath.Join(os.TempDir(), "ollama-backup")
}

func writeBackupCopy(srcPath string, integration string) (string, error) {
	dir := BackupDir()
	name := filepath.Base(srcPath)
	if integration != "" {
		dir = filepath.Join(dir, integration)
	}

	// 0o700 dir / 0o600 file (2026-09-01 security audit M3): these backups
	// hold integration configs with plaintext API keys; 0o755 + the source
	// file's mode left them world-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	backupPath := filepath.Join(dir, fmt.Sprintf("%s.%d", name, time.Now().Unix()))
	if err := copyFile(srcPath, backupPath); err != nil {
		return "", err
	}
	_ = os.Chmod(backupPath, 0o600)
	pruneOldBackups(dir, name, maxBackupsPerFile)
	return backupPath, nil
}

// WriteWithBackup writes data to path via temp file + rename, backing up any
// existing file first. Callers may optionally pass one integration name to
// store backups under BackupDir()/.../<integration>/.
func WriteWithBackup(path string, data []byte, integration ...string) error {
	backupIntegration := ""
	if len(integration) > 0 {
		backupIntegration = integration[0]
	}

	var backupPath string
	// backup must be created before any writes to the target file
	if existingContent, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existingContent, data) {
			return nil
		}
		backupPath, err = writeBackupCopy(path, backupIntegration)
		if err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing file: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp failed: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close failed: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		if backupPath != "" {
			_ = copyFile(backupPath, path)
		}
		return fmt.Errorf("rename failed: %w", err)
	}

	return nil
}

func pruneOldBackups(dir, name string, keep int) {
	if keep < 1 {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type backupEntry struct {
		name      string
		timestamp int64
	}

	prefix := name + "."
	backups := make([]backupEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		timestamp, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), prefix), 10, 64)
		if err != nil {
			continue
		}

		backups = append(backups, backupEntry{
			name:      entry.Name(),
			timestamp: timestamp,
		})
	}

	if len(backups) <= keep {
		return
	}

	sort.Slice(backups, func(i, j int) bool {
		if backups[i].timestamp != backups[j].timestamp {
			return backups[i].timestamp > backups[j].timestamp
		}
		return backups[i].name > backups[j].name
	})

	for _, backup := range backups[keep:] {
		_ = os.Remove(filepath.Join(dir, backup.name))
	}
}
