package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ignoreFile names the file in a detached checkpoint store listing entry
// names to skip, one per line.
const ignoreFile = "ignore"

// SetIgnore replaces the names this checkpoint store skips at any depth
// (such as node_modules or .git) and records them in the store. Ignored
// entries are never snapshotted, never removed or replaced by a restore,
// and cannot be named in RestorePaths. Only detached checkpoint stores may
// ignore anything: an ordinary repository must snapshot exactly what the
// Java implementation does, or the two would disagree on tree ids.
func (r *Repository) SetIgnore(names []string) error {
	if _, err := os.Stat(filepath.Join(r.metadata, worktreeFile)); err != nil {
		return errors.New("ignore rules are only supported in checkpoint stores kept outside the folder")
	}
	set := make(map[string]bool, len(names))
	var b strings.Builder
	for _, name := range names {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\n") {
			return fmt.Errorf("an ignore rule must be a single file or directory name: %q", name)
		}
		if !set[name] {
			set[name] = true
			b.WriteString(name + "\n")
		}
	}
	tmp, err := os.CreateTemp(r.metadata, ".ignore-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(r.metadata, ignoreFile)); err != nil {
		return err
	}
	r.ignore = set
	return nil
}

// ignored reports whether an entry with this name is skipped.
func (r *Repository) ignored(name string) bool {
	return r.ignore[name]
}

func loadIgnore(metadata string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(metadata, ignoreFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// removeTree deletes path, sparing ignored entries at any depth. A
// directory left non-empty because it still holds ignored entries is kept.
// With no ignore rules it behaves exactly like deleteRecursively.
func (r *Repository) removeTree(path string) error {
	if len(r.ignore) == 0 {
		return deleteRecursively(path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		if err := r.clearKeepingIgnored(path, ""); err != nil {
			return err
		}
		remaining, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return nil
		}
	}
	return os.Remove(path)
}

// clearKeepingIgnored removes every child of directory except preserved
// and ignored entries.
func (r *Repository) clearKeepingIgnored(directory string, preserved string) error {
	children, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, child := range children {
		path := filepath.Join(directory, child.Name())
		if (preserved != "" && path == preserved) || r.ignored(child.Name()) {
			continue
		}
		if err := r.removeTree(path); err != nil {
			return err
		}
	}
	return nil
}
