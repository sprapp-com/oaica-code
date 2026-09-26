package fileutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// publish.go — publishing several documents as one set (2026-09-27 audit,
// round 27, F5).
//
// Some integrations describe ONE selection in TWO documents, and those
// documents are only ever allowed to describe the same selection: Cline's
// providers.json holds the model id beside the base URL it belongs to and its
// globalState.json holds the same pair for the legacy picker, so a pair split
// across two selections — one half from the launch that is running, the other
// from the launch that was interrupted — is a configuration nobody chose.
//
// Writing them one after the other does not give that: each writer stages and
// renames its own file, so the second one's staging failure (a read-only
// directory, a full disk) lands after the first has already been published,
// which is the split the pair must never have. PublishAll stages every document
// first and renames them only once all of them are on disk.

// PublishFile is one document of a set that is published together.
type PublishFile struct {
	Path string
	Data []byte
	// Integration optionally files the pre-publish backup of Path under
	// BackupDir()/<Integration>/, as WriteWithBackup does.
	Integration string
}

// stagedFile is a PublishFile with the state its publish has reached.
type stagedFile struct {
	publish    PublishFile
	path       string
	backupPath string
	tmpPath    string
	// renamed records that this document is already at its destination, which
	// is what a later failure has to undo.
	renamed bool
	// noop records a document whose bytes on disk are already Data: it is not
	// backed up, not staged and not renamed (same rule as WriteWithBackup).
	noop bool
}

// PublishAll publishes files as one set. Every document is backed up and
// written to a temp file in its own directory before ANY of them is renamed
// into place, so a document that cannot be staged fails the publish before the
// others are visible — the pair is then what it was, not half of the new
// selection. If a rename fails after earlier documents were published, those
// are restored from the backups this call took.
//
// A document whose bytes on disk are already its Data is left alone, as
// WriteWithBackup leaves it: a publish that changes nothing is not a write.
func PublishAll(files ...PublishFile) error {
	staged := make([]*stagedFile, 0, len(files))
	for _, f := range files {
		path := writtenTarget(f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return discard(staged, fmt.Errorf("%s: %w", path, err))
		}
		sf := &stagedFile{publish: f, path: path}
		staged = append(staged, sf)

		existing, err := os.ReadFile(path)
		switch {
		case err == nil && bytes.Equal(existing, f.Data):
			sf.noop = true
			continue
		case err == nil:
			sf.backupPath, err = writeBackupCopy(path, f.Integration)
			if err != nil {
				return discard(staged, fmt.Errorf("%s: backup failed: %w", path, err))
			}
		case !os.IsNotExist(err):
			return discard(staged, fmt.Errorf("%s: read existing file: %w", path, err))
		}

		tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
		if err != nil {
			return discard(staged, fmt.Errorf("%s: create temp failed: %w", path, err))
		}
		sf.tmpPath = tmp.Name()
		if _, err := tmp.Write(f.Data); err != nil {
			_ = tmp.Close()
			return discard(staged, fmt.Errorf("%s: write failed: %w", path, err))
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return discard(staged, fmt.Errorf("%s: sync failed: %w", path, err))
		}
		if err := tmp.Close(); err != nil {
			return discard(staged, fmt.Errorf("%s: close failed: %w", path, err))
		}
	}

	// Every document is on disk and only now does either one become visible.
	for _, sf := range staged {
		if sf.noop || sf.tmpPath == "" {
			continue
		}
		if err := os.Rename(sf.tmpPath, sf.path); err != nil {
			return unpublish(staged, sf, fmt.Errorf("%s: rename failed: %w", sf.path, err))
		}
		sf.renamed = true
	}
	return nil
}

// discard removes the temp files of a set that never became visible. Every
// failure between the first staged document and the first rename leaves through
// here — a document that failed while an earlier one was already staged used to
// return directly, and the staged file's temp was left in the user's config
// directory holding the config bytes it had just written. The read-only
// directory that makes a staging failure likely is also the one the user may
// not be able to clean (2026-09-27 audit, round 28, F3).
func discard(staged []*stagedFile, err error) error {
	for _, sf := range staged {
		if sf.tmpPath != "" {
			_ = os.Remove(sf.tmpPath)
		}
	}
	return err
}

// unpublish undoes the renames of a set whose later member failed: every
// document already published goes back to its pre-publish content, so the set
// is the one that was there before this call rather than a mixture of both.
func unpublish(staged []*stagedFile, failed *stagedFile, err error) error {
	_ = os.Remove(failed.tmpPath)
	for _, sf := range staged {
		if sf == failed {
			continue
		}
		if sf.tmpPath != "" && !sf.renamed {
			_ = os.Remove(sf.tmpPath)
			continue
		}
		if !sf.renamed {
			continue
		}
		if sf.backupPath != "" {
			if rerr := copyFile(sf.backupPath, sf.path); rerr != nil {
				return fmt.Errorf("%w (and %s could not be restored: %v — its pre-publish content is at %s)", err, sf.path, rerr, sf.backupPath)
			}
			continue
		}
		// Published but nothing was there before: the document is new, so
		// undoing means removing it.
		if rerr := os.Remove(sf.path); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("%w (and the newly created %s could not be removed: %v)", err, sf.path, rerr)
		}
	}
	return err
}
