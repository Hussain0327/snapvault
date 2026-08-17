package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/object"
	"github.com/Hussain0327/snapvault/go/internal/store"
)

// sink receives what a working-tree scan discovers. storingSink persists it
// for snapshot; hashingSink only addresses it, which is what keeps diff a
// read-only operation. Ids match exactly because both hash the same
// canonical envelope. Blob may be called from multiple goroutines.
type sink interface {
	blob(path string) (string, error)
	symlinkTarget(target []byte) (string, error)
	tree(t *object.Tree) (string, error)
}

type storingSink struct {
	store *store.Store
}

func (s storingSink) blob(path string) (string, error) { return s.store.PutBlobFile(path) }
func (s storingSink) symlinkTarget(target []byte) (string, error) {
	return s.store.Put(object.TypeBlob, target)
}
func (s storingSink) tree(t *object.Tree) (string, error) {
	return s.store.Put(object.TypeTree, t.Encode())
}

type hashingSink struct{}

func (hashingSink) blob(path string) (string, error) { return store.BlobFileID(path) }
func (hashingSink) symlinkTarget(target []byte) (string, error) {
	return object.ID(object.TypeBlob, target), nil
}
func (hashingSink) tree(t *object.Tree) (string, error) {
	return object.ID(object.TypeTree, t.Encode()), nil
}

// pendingEntry is one directory child discovered by the walk. Regular files
// carry the pre-hash size and mtime so the pool can detect concurrent
// modification; their objectID is filled in by a worker.
type pendingEntry struct {
	name       string
	kind       object.EntryKind
	executable bool
	objectID   string
	child      *pendingDir
	relPath    string
	absPath    string
	size       int64
	mtime      time.Time
}

type pendingDir struct {
	entries []*pendingEntry
}

// scanTree walks the working tree, hashes regular files on the worker pool,
// assembles tree objects bottom-up, and returns the root tree id plus every
// leaf: files, symlinks, and directories that contain nothing.
func (r *Repository) scanTree(snk sink) (string, map[string]object.TreeEntry, error) {
	var files []*pendingEntry
	root, err := r.walk(r.root, "", snk, &files)
	if err != nil {
		return "", nil, err
	}
	if err := r.hashFiles(files, snk); err != nil {
		return "", nil, err
	}
	leaves := make(map[string]object.TreeEntry)
	treeID, err := assemble(root, snk, leaves)
	if err != nil {
		return "", nil, err
	}
	return treeID, leaves, nil
}

func (r *Repository) walk(
	dir string, prefix string, snk sink, files *[]*pendingEntry,
) (*pendingDir, error) {
	listing, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range listing {
		child := filepath.Join(dir, entry.Name())
		if child == r.metadata || isRepositoryMetadata(child, entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.SortFunc(names, object.CompareNames)

	result := &pendingDir{}
	for _, name := range names {
		abs := filepath.Join(dir, name)
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		entryName := strings.ToValidUTF8(name, "�")
		rel := entryName
		if prefix != "" {
			rel = prefix + "/" + entryName
		}
		entry := &pendingEntry{name: entryName, relPath: rel, absPath: abs}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(abs)
			if err != nil {
				return nil, err
			}
			id, err := snk.symlinkTarget([]byte(target))
			if err != nil {
				return nil, err
			}
			entry.kind, entry.objectID = object.KindSymlink, id
		case mode.IsDir():
			child, err := r.walk(abs, rel, snk, files)
			if err != nil {
				return nil, err
			}
			entry.kind, entry.child = object.KindDirectory, child
		case mode.IsRegular():
			entry.kind = object.KindFile
			entry.executable = mode.Perm()&0o111 != 0
			entry.size, entry.mtime = info.Size(), info.ModTime()
			*files = append(*files, entry)
		default:
			return nil, fmt.Errorf("unsupported filesystem entry: %s", abs)
		}
		result.entries = append(result.entries, entry)
	}
	return result, nil
}

// isRepositoryMetadata reports whether a child is the metadata directory of
// a nested SnapVault repository, which is skipped at any depth.
func isRepositoryMetadata(path string, name string) bool {
	if name != MetadataDirName {
		return false
	}
	info, err := os.Lstat(filepath.Join(path, "format"))
	return err == nil && info.Mode().IsRegular()
}

// hashFiles feeds every regular file to a bounded worker pool. Each worker
// streams and hashes one file, then re-checks its size and mtime so a file
// rewritten mid-scan aborts the snapshot instead of corrupting it.
func (r *Repository) hashFiles(files []*pendingEntry, snk sink) error {
	if len(files) == 0 {
		return nil
	}
	workers := r.workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	workers = min(workers, len(files))

	jobs := make(chan *pendingEntry)
	quit := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		once.Do(func() { close(quit) })
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				if err := hashOne(entry, snk); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
feed:
	for _, entry := range files {
		select {
		case jobs <- entry:
		case <-quit:
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func hashOne(entry *pendingEntry, snk sink) error {
	id, err := snk.blob(entry.absPath)
	if err != nil {
		return err
	}
	after, err := os.Lstat(entry.absPath)
	if err != nil || !after.Mode().IsRegular() || after.Size() != entry.size ||
		!after.ModTime().Equal(entry.mtime) {
		return fmt.Errorf("file changed while snapshotting: %s", entry.absPath)
	}
	entry.objectID = id
	return nil
}

// assemble builds tree objects bottom-up, records every leaf, and returns
// the directory's tree id. A directory that contributed no leaves is itself
// a leaf: an empty directory is part of the snapshotted state.
func assemble(
	dir *pendingDir, snk sink, leaves map[string]object.TreeEntry,
) (string, error) {
	entries := make([]object.TreeEntry, 0, len(dir.entries))
	for _, pending := range dir.entries {
		entry := object.TreeEntry{
			Name:       pending.name,
			Kind:       pending.kind,
			ObjectID:   pending.objectID,
			Executable: pending.executable,
		}
		if pending.kind == object.KindDirectory {
			leavesBefore := len(leaves)
			id, err := assemble(pending.child, snk, leaves)
			if err != nil {
				return "", err
			}
			entry.ObjectID = id
			if len(leaves) == leavesBefore {
				leaves[pending.relPath] = entry
			}
		} else {
			leaves[pending.relPath] = entry
		}
		entries = append(entries, entry)
	}
	tree, err := object.NewTree(entries)
	if err != nil {
		return "", err
	}
	return snk.tree(tree)
}
