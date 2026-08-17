package repo

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// ChangeType is the kind of change between two directory trees.
type ChangeType int

// The change kinds reported by diff.
const (
	Added ChangeType = iota
	Modified
	Deleted
	TypeChanged
)

// Status returns the single-letter code printed by the CLI.
func (t ChangeType) Status() byte {
	switch t {
	case Added:
		return 'A'
	case Modified:
		return 'M'
	case Deleted:
		return 'D'
	case TypeChanged:
		return 'T'
	default:
		panic(fmt.Sprintf("unknown change type %d", int(t)))
	}
}

// Change is one leaf-path difference between snapshots or between a
// snapshot and the working directory.
type Change struct {
	Type   ChangeType
	Path   string
	Before *object.TreeEntry
	After  *object.TreeEntry
}

// Entry returns the change's surviving entry: After when present, else
// Before.
func (c Change) Entry() *object.TreeEntry {
	if c.After != nil {
		return c.After
	}
	return c.Before
}

// Diff compares two stored snapshots.
func (r *Repository) Diff(fromRevision, toRevision string) ([]Change, error) {
	before, err := r.commitLeaves(fromRevision)
	if err != nil {
		return nil, err
	}
	after, err := r.commitLeaves(toRevision)
	if err != nil {
		return nil, err
	}
	return compare(before, after), nil
}

// DiffWorking compares one stored snapshot to the live working directory
// without writing objects.
func (r *Repository) DiffWorking(fromRevision string) ([]Change, error) {
	id, err := r.ResolveCommit(fromRevision)
	if err != nil {
		return nil, err
	}
	commit, err := r.ReadCommit(id)
	if err != nil {
		return nil, err
	}
	return r.lockedWorkingDiff(func() (map[string]object.TreeEntry, error) {
		return r.flatten(commit.TreeID)
	})
}

// DiffWorkingFromHead compares HEAD to the working directory, treating an
// unborn HEAD as an empty tree.
func (r *Repository) DiffWorkingFromHead() ([]Change, error) {
	return r.lockedWorkingDiff(func() (map[string]object.TreeEntry, error) {
		head, err := r.Head()
		if err != nil {
			return nil, err
		}
		if head == "" {
			return make(map[string]object.TreeEntry), nil
		}
		commit, err := r.ReadCommit(head)
		if err != nil {
			return nil, err
		}
		return r.flatten(commit.TreeID)
	})
}

// lockedWorkingDiff compares beforeLeaves() to a working-tree scan, holding
// the repository lock across both so the pair is consistent.
func (r *Repository) lockedWorkingDiff(
	beforeLeaves func() (map[string]object.TreeEntry, error),
) ([]Change, error) {
	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return nil, err
	}
	defer lock.close()
	if err := r.requireCompleteWorkingTree(); err != nil {
		return nil, err
	}
	before, err := beforeLeaves()
	if err != nil {
		return nil, err
	}
	_, after, err := r.scanTree(hashingSink{})
	if err != nil {
		return nil, err
	}
	return compare(before, after), nil
}

func (r *Repository) commitLeaves(revision string) (map[string]object.TreeEntry, error) {
	id, err := r.ResolveCommit(revision)
	if err != nil {
		return nil, err
	}
	commit, err := r.ReadCommit(id)
	if err != nil {
		return nil, err
	}
	return r.flatten(commit.TreeID)
}

// flatten collects the leaves of a tree: every file, every symbolic link,
// and every directory that contains nothing.
func (r *Repository) flatten(treeID string) (map[string]object.TreeEntry, error) {
	leaves := make(map[string]object.TreeEntry)
	if err := r.flattenInto(treeID, "", leaves, make(map[string]bool)); err != nil {
		return nil, err
	}
	return leaves, nil
}

func (r *Repository) flattenInto(
	treeID string, prefix string, leaves map[string]object.TreeEntry, ancestors map[string]bool,
) error {
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
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Kind == object.KindDirectory {
			leavesBefore := len(leaves)
			if err := r.flattenInto(entry.ObjectID, path, leaves, ancestors); err != nil {
				return err
			}
			if len(leaves) == leavesBefore {
				leaves[path] = entry
			}
		} else {
			leaves[path] = entry
		}
	}
	return nil
}

// compare diffs two flattened trees. Both sides contain every file,
// symbolic link, and empty directory, so an empty result always means the
// trees are byte-for-byte identical.
func compare(before, after map[string]object.TreeEntry) []Change {
	paths := make([]string, 0, len(before)+len(after))
	for path := range before {
		paths = append(paths, path)
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.SortFunc(paths, object.CompareNames)
	beforePaths := sortedPaths(before)
	afterPaths := sortedPaths(after)

	changes := []Change{}
	for _, path := range paths {
		oldEntry, hasOld := before[path]
		newEntry, hasNew := after[path]
		switch {
		case !hasOld:
			if !hasDescendants(newEntry, path, beforePaths) {
				entry := newEntry
				changes = append(changes, Change{Type: Added, Path: path, After: &entry})
			}
		case !hasNew:
			if !hasDescendants(oldEntry, path, afterPaths) {
				entry := oldEntry
				changes = append(changes, Change{Type: Deleted, Path: path, Before: &entry})
			}
		case oldEntry.Kind != newEntry.Kind:
			o, n := oldEntry, newEntry
			changes = append(changes, Change{Type: TypeChanged, Path: path, Before: &o, After: &n})
		case oldEntry.ObjectID != newEntry.ObjectID || oldEntry.Executable != newEntry.Executable:
			o, n := oldEntry, newEntry
			changes = append(changes, Change{Type: Modified, Path: path, Before: &o, After: &n})
		}
	}
	return changes
}

// hasDescendants reports whether a directory that is empty on one side
// holds entries on the other side. Those entries already describe the
// difference, so reporting the directory itself would be noise.
func hasDescendants(entry object.TreeEntry, path string, otherSide []string) bool {
	if entry.Kind != object.KindDirectory {
		return false
	}
	prefix := path + "/"
	i := sort.Search(len(otherSide), func(i int) bool {
		return object.CompareNames(otherSide[i], prefix) >= 0
	})
	return i < len(otherSide) && strings.HasPrefix(otherSide[i], prefix)
}

func sortedPaths(leaves map[string]object.TreeEntry) []string {
	paths := make([]string, 0, len(leaves))
	for path := range leaves {
		paths = append(paths, path)
	}
	slices.SortFunc(paths, object.CompareNames)
	return paths
}
