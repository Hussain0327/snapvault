package repo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Hussain0327/snapvault/go/internal/store"
)

// Upgrade migrates the repository from format 1 to format 2 by rewriting
// the format file; it never touches stored objects, since a legacy-form
// object is valid forever in a format 2 repository. It reports whether it
// actually changed anything, so callers can tell an upgrade apart from the
// idempotent no-op of re-running it on an already-format-2 repository.
func (r *Repository) Upgrade() (upgraded bool, err error) {
	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return false, err
	}
	defer lock.close()

	if r.version == maxFormatVersion {
		return false, nil
	}
	if err := writeFormatFileAtomically(r.metadata, maxFormatVersion); err != nil {
		return false, err
	}
	r.version = maxFormatVersion
	r.store.SetFormat(store.Format(maxFormatVersion))
	return true, nil
}

// writeFormatFileAtomically replaces ".snapvault/format" with a temp file,
// fsync, and rename, so a crash mid-upgrade never leaves a half-written
// format file behind.
func writeFormatFileAtomically(metadata string, version int) error {
	path := filepath.Join(metadata, "format")
	tmp, err := os.CreateTemp(metadata, ".format-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if _, err := fmt.Fprintf(tmp, "snapvault %d\n", version); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}
