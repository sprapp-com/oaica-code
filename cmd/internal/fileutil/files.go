// Package fileutil provides small shared helpers for reading JSON files
// and writing config files with backup-on-overwrite semantics.
package fileutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
//
// A document that is `null` comes back as an EMPTY map, not a nil one.
// Unmarshalling `null` into a map succeeds with a nil result, so this used to
// return
// (nil, nil): a caller could not tell a document that failed to load from an
// empty one, and the first write to it would panic — assigning to an entry of a
// nil map is a runtime error (2026-09-27 audit, round 21, F15). Every caller
// today only reads, which a nil map survives, so this closes the class rather
// than a live crash.
func ReadJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result == nil {
		result = map[string]any{}
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
// writtenTarget resolves the path the data must actually land at. A path that
// is a symlink is FOLLOWED, up to a sane chain length, because rename(2)
// replaces the link rather than writing through it: the write would succeed,
// the client would name the configured path, and the file the user keeps (a
// git-managed or backed-up directory they linked the store into) would keep its
// old content forever (2026-09-26 audit). A link whose target does not exist
// yet is still the user's chosen destination, so it is followed too.
func writtenTarget(path string) string {
	for i := 0; i < 8; i++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path
		}
		dest, err := os.Readlink(path)
		if err != nil || dest == "" {
			return path
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	// A chain this long is not something to follow further; writing at the last
	// link's own path is the safe answer.
	return path
}

func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	path = writtenTarget(path)
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

// timeNow is the clock the backup name is stamped from. It is a variable so a
// test can hold it still: the collision this stamp had at second granularity
// only shows up when two backups land in the same second, and whether they do
// otherwise depends on how fast the machine is.
var timeNow = time.Now

// backupName renders the name a backup of base gets when it is the first (seq 1)
// or a later backup claimed within the same wall-clock second.
func backupName(base string, stamp int64, seq int) string {
	if seq <= 1 {
		return fmt.Sprintf("%s.%d", base, stamp)
	}
	return fmt.Sprintf("%s.%d-%d", base, stamp, seq)
}

// writeNewFileExclusive creates dst and writes data, failing with os.ErrExist
// rather than truncating when the name is already taken. The claim on the name
// is the creation itself, so two writers racing for it cannot both win.
func writeNewFileExclusive(dst string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(dst)
		return err
	}
	return f.Close()
}

// nextBackupSeq returns the seq the next backup of name for stamp must use to
// stay newer than every backup already claimed for that stamp.
//
// The names are what pruneOldBackups orders by, so a name freed by pruning must
// not be handed out again: it would sort as the OLDEST name for that second
// while holding the NEWEST bytes, and the bounded "keep the 5 most recent"
// policy would then keep stale copies and delete the fresh one (observed with
// eight writes inside one second, where the plain "<name>.<stamp>" was pruned
// and then immediately re-claimed).
func nextBackupSeq(dir, name string, stamp int64) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	first := fmt.Sprintf("%s.%d", name, stamp)
	next := 1
	for _, entry := range entries {
		entryName := entry.Name()
		if entryName != first && !strings.HasPrefix(entryName, first+"-") {
			continue
		}
		seq := 1
		if entryName != first {
			parsed, err := strconv.Atoi(strings.TrimPrefix(entryName, first+"-"))
			if err != nil {
				continue
			}
			seq = parsed
		}
		if seq+1 > next {
			next = seq + 1
		}
	}
	return next
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

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}

	// Claim a FREE name, don't trust the timestamp to be one. At second
	// granularity two backups of the same file collide, and the backup is
	// what O_TRUNC used to eat: the second call overwrote the first — the
	// copy holding the user's ORIGINAL pre-oaica content — with oaica's own
	// intermediate write, so the file the user is promised a backup of was
	// gone and nothing said so (2026-09-26 audit, round 11). One
	// `oaica launch openclaw` reaches it: Openclaw.Edit writes
	// ~/.openclaw/openclaw.json and configureOllamaWebSearch, called from the
	// same Run, writes it again with no subprocess in between on any launch
	// that is not the first. Same claim-a-free-name stance as
	// quarantineAuthStore (cmd/launch/auth_store.go).
	stamp := timeNow().Unix()
	backupPath := ""
	for seq := nextBackupSeq(dir, name, stamp); ; seq++ {
		candidate := filepath.Join(dir, backupName(name, stamp, seq))
		// No iteration cap: the starting seq is already past every name on
		// disk, so this only loops again when another writer claimed the same
		// name in between, and each iteration moves to a strictly higher seq.
		if err := writeNewFileExclusive(candidate, data, 0o600); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		backupPath = candidate
		break
	}

	pruneOldBackups(dir, name, maxBackupsPerFile)
	return backupPath, nil
}

// WriteWithBackup writes data to path via temp file + rename, backing up any
// existing file first. Callers may optionally pass one integration name to
// store backups under BackupDir()/.../<integration>/.
func WriteWithBackup(path string, data []byte, integration ...string) error {
	// Same reason as WriteFileAtomic: a symlinked destination is FOLLOWED, or
	// the rename replaces the link and the file the user keeps (a
	// git-managed ~/.openclaw, a backed-up config directory) keeps its old
	// content while this reports success (2026-09-26 audit).
	path = writtenTarget(path)
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
		// seq is the "-N" a second backup claimed in the same second, so
		// backups taken inside one second still order by when they were taken
		// rather than by how their name happens to sort.
		seq int64
	}

	prefix := name + "."
	backups := make([]backupEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		stem := strings.TrimPrefix(entry.Name(), prefix)
		seq := int64(1)
		if dash := strings.IndexByte(stem, '-'); dash >= 0 {
			parsed, err := strconv.ParseInt(stem[dash+1:], 10, 64)
			if err != nil {
				continue
			}
			seq = parsed
			stem = stem[:dash]
		}
		timestamp, err := strconv.ParseInt(stem, 10, 64)
		if err != nil {
			continue
		}

		backups = append(backups, backupEntry{
			name:      entry.Name(),
			timestamp: timestamp,
			seq:       seq,
		})
	}

	if len(backups) <= keep {
		return
	}

	sort.Slice(backups, func(i, j int) bool {
		if backups[i].timestamp != backups[j].timestamp {
			return backups[i].timestamp > backups[j].timestamp
		}
		if backups[i].seq != backups[j].seq {
			return backups[i].seq > backups[j].seq
		}
		return backups[i].name > backups[j].name
	})

	for _, backup := range backups[keep:] {
		_ = os.Remove(filepath.Join(dir, backup.name))
	}
}
