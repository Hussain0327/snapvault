package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// InterruptedRestore is a restore that began but never finished, so the
// tree it targeted is incomplete.
type InterruptedRestore struct {
	CommitID string
	Target   string
}

// Restore materializes a snapshot into the repository root (target == "")
// or a separate target directory. HEAD is intentionally not moved: restore
// changes files, while snapshot changes history.
func (r *Repository) Restore(revision string, target string, force bool) error {
	commitID, err := r.ResolveCommit(revision)
	if err != nil {
		return err
	}
	commit, err := r.ReadCommit(commitID)
	if err != nil {
		return err
	}
	if err := r.verifyTree(commit.TreeID, map[string]bool{}, map[string]bool{}, map[string]bool{}); err != nil {
		return err
	}

	resolved := r.root
	if target != "" {
		abs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore through a symbolic-link target: %s", abs)
		}
		if resolved, err = canonicalizeTarget(abs); err != nil {
			return err
		}
	}
	inPlace := resolved == r.root
	if err := r.validateRestoreTarget(resolved, inPlace); err != nil {
		return err
	}

	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return err
	}
	defer lock.close()
	if inPlace {
		if !force {
			dirty, err := r.isWorkingTreeDirty()
			if err != nil {
				return err
			}
			if dirty {
				return errors.New(
					"working directory has unsnapshotted changes; rerun restore with --force")
			}
		}
	} else if err := openExternalTarget(resolved, force); err != nil {
		return err
	}
	if err := r.verifyNamesAreRepresentable(commit.TreeID, resolved); err != nil {
		return err
	}

	if err := r.beginRestore(commitID, resolved); err != nil {
		return err
	}
	preserved := ""
	if inPlace {
		preserved = r.metadata
	}
	if err := clearDirectory(resolved, preserved); err != nil {
		return err
	}
	if err := r.materializeTree(commit.TreeID, resolved); err != nil {
		return err
	}
	if inPlace {
		r.removeDirCache()
	}
	return r.endRestore()
}

// InterruptedRestoreState returns the restore that was interrupted, if one
// left a target half-written.
func (r *Repository) InterruptedRestoreState() (*InterruptedRestore, error) {
	marker := filepath.Join(r.metadata, restoreMarker)
	raw, err := os.ReadFile(marker)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return nil, fmt.Errorf("a restore was interrupted and its marker is unreadable: %s", marker)
	}
	return &InterruptedRestore{
		CommitID: strings.TrimSpace(lines[0]),
		Target:   strings.TrimSpace(lines[1]),
	}, nil
}

// beginRestore records a restore before it removes anything, and forces the
// record to disk. A crash between clearing and materializing is otherwise
// indistinguishable from an empty directory.
func (r *Repository) beginRestore(commitID string, target string) error {
	f, err := os.OpenFile(
		filepath.Join(r.metadata, restoreMarker), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(commitID + "\n" + target + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *Repository) endRestore() error {
	err := os.Remove(filepath.Join(r.metadata, restoreMarker))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// requireCompleteWorkingTree refuses work that a half-restored working tree
// would silently corrupt. Only an interrupted in-place restore leaves this
// repository's own files incomplete; an external target does not.
func (r *Repository) requireCompleteWorkingTree() error {
	interrupted, err := r.InterruptedRestoreState()
	if err != nil {
		return err
	}
	if interrupted == nil || filepath.Clean(interrupted.Target) != r.root {
		return nil
	}
	return fmt.Errorf(
		"a restore of %s was interrupted, so the working directory is incomplete;"+
			" finish it with 'snapvault restore %s --force'",
		interrupted.CommitID, interrupted.CommitID)
}

func (r *Repository) isWorkingTreeDirty() (bool, error) {
	treeID, _, err := r.scanTree(hashingSink{})
	if err != nil {
		return false, err
	}
	head, err := r.Head()
	if err != nil {
		return false, err
	}
	if head == "" {
		empty, err := object.NewTree(nil)
		if err != nil {
			return false, err
		}
		return treeID != object.ID(object.TypeTree, empty.Encode()), nil
	}
	commit, err := r.ReadCommit(head)
	if err != nil {
		return false, err
	}
	return treeID != commit.TreeID, nil
}

func (r *Repository) validateRestoreTarget(target string, inPlace bool) error {
	if inPlace {
		return nil
	}
	if filepath.Dir(target) == target {
		return errors.New("refusing to restore over a filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if abs, err := filepath.Abs(home); err == nil && target == filepath.Clean(abs) {
			return errors.New("refusing to restore over the user home directory")
		}
	}
	if isWithin(r.metadata, target) || isWithin(target, r.root) {
		return errors.New("restore target would overwrite the SnapVault repository")
	}
	if isWithin(r.root, target) {
		return fmt.Errorf(
			"restore target is inside the repository and would be captured by the next"+
				" snapshot; choose a directory outside %s", r.root)
	}
	return nil
}

// isWithin reports whether child equals parent or sits below it.
func isWithin(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// canonicalizeTarget resolves the deepest existing ancestor of target
// through symlinks and re-appends the missing components, so containment
// checks see the target's real location.
func canonicalizeTarget(target string) (string, error) {
	var missing []string
	existing := target
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("restore target has no existing filesystem ancestor: %s", target)
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	canonical, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, missing[i])
	}
	return filepath.Clean(canonical), nil
}

func openExternalTarget(target string, force bool) error {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.MkdirAll(target, 0o755)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("restore target is not a directory: %s", target)
	}
	if force {
		return nil
	}
	children, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return errors.New("restore target is not empty; rerun with --force")
	}
	return nil
}

// verifyTree inflates and integrity-checks every tree and every unique blob
// reachable from treeID before a restore removes anything.
func (r *Repository) verifyTree(
	treeID string, verified, ancestors, blobs map[string]bool,
) error {
	if verified[treeID] {
		return nil
	}
	if ancestors[treeID] {
		return fmt.Errorf("tree graph contains a cycle at %s", treeID)
	}
	ancestors[treeID] = true
	defer delete(ancestors, treeID)

	tree, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries() {
		if entry.Kind == object.KindDirectory {
			if err := r.verifyTree(entry.ObjectID, verified, ancestors, blobs); err != nil {
				return err
			}
		} else if !blobs[entry.ObjectID] {
			blobs[entry.ObjectID] = true
			if err := r.store.CopyPayload(entry.ObjectID, object.TypeBlob, io.Discard); err != nil {
				return err
			}
		}
	}
	verified[treeID] = true
	return nil
}

// verifyNamesAreRepresentable refuses, before anything is removed, a
// snapshot holding sibling names this filesystem cannot keep apart. The
// fold is case-only (the Go standard library has no Unicode normalizer), so
// names differing solely by composition are caught later by the
// two-entries-one-file check during materialization instead.
func (r *Repository) verifyNamesAreRepresentable(treeID string, target string) error {
	distinguishes, err := distinguishesNameCase(target)
	if err != nil {
		return err
	}
	if distinguishes {
		return nil
	}
	return r.verifyNamesIn(treeID, "", map[string]bool{})
}

func (r *Repository) verifyNamesIn(treeID string, prefix string, checked map[string]bool) error {
	if checked[treeID] {
		return nil
	}
	checked[treeID] = true
	byFoldedName := make(map[string]string)
	tree, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries() {
		folded := strings.ToLower(entry.Name)
		if clashing, ok := byFoldedName[folded]; ok {
			location := prefix
			if location == "" {
				location = "the snapshot root"
			}
			return fmt.Errorf(
				"this filesystem cannot keep %q and %q apart in %s;"+
					" restore on a case-sensitive filesystem instead",
				clashing, entry.Name, location)
		}
		byFoldedName[folded] = entry.Name
		if entry.Kind == object.KindDirectory {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if err := r.verifyNamesIn(entry.ObjectID, path, checked); err != nil {
				return err
			}
		}
	}
	return nil
}

// distinguishesNameCase probes whether directory keeps names that differ
// only by case apart.
func distinguishesNameCase(directory string) (bool, error) {
	probe, err := os.CreateTemp(directory, "snapvault-probe-*.tmp")
	if err != nil {
		return false, err
	}
	name := probe.Name()
	probe.Close()
	defer os.Remove(name)
	upper := strings.ToUpper(filepath.Base(name))
	_, err = os.Lstat(filepath.Join(directory, upper))
	return errors.Is(err, os.ErrNotExist), nil
}

func (r *Repository) materializeTree(treeID string, directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	tree, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries() {
		destination := filepath.Join(directory, entry.Name)
		if filepath.Dir(destination) != directory {
			return fmt.Errorf("unsafe path in snapshot: %s", entry.Name)
		}
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("two entries in this snapshot resolve to the same file: %s", destination)
		}
		switch entry.Kind {
		case object.KindDirectory:
			if err := r.materializeTree(entry.ObjectID, destination); err != nil {
				return err
			}
		case object.KindFile:
			if err := r.restoreFile(entry, destination); err != nil {
				return err
			}
		case object.KindSymlink:
			if err := r.restoreSymlink(entry, destination); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Repository) restoreFile(entry object.TreeEntry, destination string) error {
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".snapvault-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if err := r.store.CopyPayload(entry.ObjectID, object.TypeBlob, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return err
	}
	tmpPath = ""
	mode := os.FileMode(0o600)
	if entry.Executable {
		mode = 0o711
	}
	return os.Chmod(destination, mode)
}

func (r *Repository) restoreSymlink(entry object.TreeEntry, destination string) error {
	t, payload, err := r.store.Get(entry.ObjectID)
	if err != nil {
		return err
	}
	if t != object.TypeBlob {
		return errors.New("symlink target is not stored as a blob")
	}
	if !utf8.Valid(payload) {
		return errors.New("symlink target is not valid UTF-8")
	}
	return os.Symlink(string(payload), destination)
}

// clearDirectory removes every child of directory except preserved (the
// metadata directory during an in-place restore; "" preserves nothing).
func clearDirectory(directory string, preserved string) error {
	children, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, child := range children {
		path := filepath.Join(directory, child.Name())
		if preserved != "" && path == preserved {
			continue
		}
		if err := deleteRecursively(path); err != nil {
			return err
		}
	}
	return nil
}

func deleteRecursively(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		if err := clearDirectory(path, ""); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
