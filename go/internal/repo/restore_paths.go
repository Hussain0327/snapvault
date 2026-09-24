package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// pathRestore is one validated RestorePaths request: where it lands in the
// work tree, and the snapshot entry to put there (nil when the path did not
// exist in the snapshot, so restoring it means deleting it).
type pathRestore struct {
	rel         string
	destination string
	entry       *object.TreeEntry
}

// RestorePaths returns each named path in the work tree to its state in
// revision, leaving every other path alone. A path that did not exist in
// revision is deleted. Paths are slash-separated and relative to the work
// tree root. Every path and every object a path needs is validated and
// integrity-checked before anything in the work tree changes.
func (r *Repository) RestorePaths(revision string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("no paths to restore")
	}
	commitID, err := r.ResolveCommit(revision)
	if err != nil {
		return err
	}
	commit, err := r.ReadCommit(commitID)
	if err != nil {
		return err
	}

	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return err
	}
	defer lock.close()

	var plan []pathRestore
	for _, raw := range paths {
		rel, err := cleanRelativePath(raw)
		if err != nil {
			return err
		}
		for _, part := range strings.Split(rel, "/") {
			if r.ignored(part) {
				return fmt.Errorf("refusing to restore %q: %q is ignored by this checkpoint store", raw, part)
			}
		}
		destination := filepath.Join(r.root, filepath.FromSlash(rel))
		if err := r.requireNoSymlinkAncestors(rel); err != nil {
			return err
		}
		entry, err := r.lookupEntry(commit.TreeID, rel)
		if err != nil {
			return err
		}
		if entry != nil {
			if err := r.verifyEntry(*entry); err != nil {
				return err
			}
		}
		plan = append(plan, pathRestore{rel: rel, destination: destination, entry: entry})
	}

	for _, p := range plan {
		if err := r.restorePath(p); err != nil {
			return fmt.Errorf("restoring %s: %w", p.rel, err)
		}
	}
	r.removeDirCache()
	return nil
}

// cleanRelativePath normalizes a user-supplied path and refuses anything
// that is empty, absolute, escapes the work tree, or names repository
// metadata.
func cleanRelativePath(raw string) (string, error) {
	slashed := filepath.ToSlash(raw)
	if slashed == "" || path.IsAbs(slashed) || filepath.IsAbs(raw) {
		return "", fmt.Errorf("restore path must be relative to the folder: %q", raw)
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("restore path must name something inside the folder: %q", raw)
	}
	if first, _, _ := strings.Cut(cleaned, "/"); first == MetadataDirName {
		return "", fmt.Errorf("refusing to restore repository metadata: %q", raw)
	}
	return cleaned, nil
}

// requireNoSymlinkAncestors refuses a path whose existing parent
// directories include a symbolic link, which could redirect the restore
// outside the work tree.
func (r *Repository) requireNoSymlinkAncestors(rel string) error {
	current := r.root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore through a symbolic link: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("a parent of %s is not a directory: %s", rel, current)
		}
	}
	return nil
}

// lookupEntry finds rel in the tree treeID, returning nil when some
// component of it does not exist there.
func (r *Repository) lookupEntry(treeID string, rel string) (*object.TreeEntry, error) {
	parts := strings.Split(rel, "/")
	current := treeID
	for i, part := range parts {
		tree, err := r.readTree(current)
		if err != nil {
			return nil, err
		}
		var found *object.TreeEntry
		for _, entry := range tree.Entries() {
			if entry.Name == part {
				e := entry
				found = &e
				break
			}
		}
		if found == nil {
			return nil, nil
		}
		if i == len(parts)-1 {
			return found, nil
		}
		if found.Kind != object.KindDirectory {
			return nil, nil
		}
		current = found.ObjectID
	}
	return nil, nil
}

// verifyEntry integrity-checks every object needed to materialize entry.
func (r *Repository) verifyEntry(entry object.TreeEntry) error {
	if entry.Kind == object.KindDirectory {
		return r.verifyTree(entry.ObjectID, map[string]bool{}, map[string]bool{}, map[string]bool{})
	}
	return r.store.CopyPayload(entry.ObjectID, object.TypeBlob, io.Discard)
}

func (r *Repository) restorePath(p pathRestore) error {
	if _, err := os.Lstat(p.destination); err == nil {
		if err := r.removeTree(p.destination); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.entry == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.destination), 0o755); err != nil {
		return err
	}
	switch p.entry.Kind {
	case object.KindDirectory:
		return r.materializeTree(p.entry.ObjectID, p.destination)
	case object.KindFile:
		return r.restoreFile(*p.entry, p.destination)
	case object.KindSymlink:
		return r.restoreSymlink(*p.entry, p.destination)
	}
	return fmt.Errorf("unknown entry kind for %s", p.rel)
}
