// Package repo implements SnapVault repository operations — init, snapshot,
// history, diff, and restore — over the format-v1 object database, with
// behavior matching the Java reference implementation. Snapshot and diff
// hash regular files on a bounded worker pool.
package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/object"
	"github.com/Hussain0327/snapvault/go/internal/store"
)

// MetadataDirName is the name of the repository metadata directory.
const MetadataDirName = ".snapvault"

const (
	formatLine    = "snapvault 1"
	defaultRef    = "refs/heads/main"
	restoreMarker = "restore-in-progress"
)

// Repository is the high-level SnapVault API used by the CLI and tests.
type Repository struct {
	root     string
	metadata string
	store    *store.Store

	// now supplies commit timestamps; a test seam.
	now func() time.Time
	// workers bounds the hashing pool; 0 means one worker per CPU.
	workers int
}

// Init initializes a repository in an existing or new ordinary directory.
func Init(directory string) (*Repository, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	metadata := filepath.Join(root, MetadataDirName)
	if _, err := os.Lstat(metadata); err == nil {
		return nil, fmt.Errorf("SnapVault is already initialized at %s", root)
	}

	if err := os.Mkdir(metadata, 0o755); err != nil {
		return nil, err
	}
	for _, dir := range []string{"objects", filepath.Join("refs", "heads")} {
		if err := os.MkdirAll(filepath.Join(metadata, dir), 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(metadata, "format"), []byte(formatLine+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(metadata, "HEAD"), []byte("ref: "+defaultRef+"\n"), 0o644); err != nil {
		return nil, err
	}
	return openAt(root)
}

// Open finds a repository at or above start, so commands also work in
// subdirectories.
func Open(start string) (*Repository, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil, fmt.Errorf("path does not exist: %s", abs)
	}
	current, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if info, err := os.Lstat(filepath.Join(current, MetadataDirName)); err == nil && info.IsDir() {
			return openAt(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, errors.New("not inside a SnapVault repository (run 'snapvault init' first)")
		}
		current = parent
	}
}

func openAt(root string) (*Repository, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	metadata := filepath.Join(realRoot, MetadataDirName)
	format, err := os.ReadFile(filepath.Join(metadata, "format"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(format)) != formatLine {
		return nil, fmt.Errorf(
			"unsupported SnapVault repository format: %s", strings.TrimSpace(string(format)))
	}
	if _, err := currentRefPath(metadata); err != nil {
		return nil, err
	}
	s, err := store.New(filepath.Join(metadata, "objects"))
	if err != nil {
		return nil, err
	}
	return &Repository{root: realRoot, metadata: metadata, store: s, now: time.Now}, nil
}

// Root returns the repository's working-directory root.
func (r *Repository) Root() string { return r.root }

// Metadata returns the repository's metadata directory.
func (r *Repository) Metadata() string { return r.metadata }

// SetWorkers bounds the hashing worker pool; 0 restores the per-CPU default.
func (r *Repository) SetWorkers(workers int) { r.workers = workers }

// Snapshot creates an immutable snapshot commit and advances the current
// branch atomically, returning the new commit id.
func (r *Repository) Snapshot(message string) (string, error) {
	normalized := strings.TrimSpace(message)
	if normalized == "" {
		return "", errors.New("snapshot message cannot be empty")
	}

	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return "", err
	}
	defer lock.close()
	if err := r.requireCompleteWorkingTree(); err != nil {
		return "", err
	}

	treeID, _, files, err := r.scanWorking(storingSink{store: r.store})
	if err != nil {
		return "", err
	}
	var parents []string
	head, err := r.Head()
	if err != nil {
		return "", err
	}
	if head != "" {
		parents = []string{head}
	}
	commit, err := object.NewCommit(treeID, parents, r.now(), normalized)
	if err != nil {
		return "", err
	}
	commitID, err := r.store.Put(object.TypeCommit, commit.Encode())
	if err != nil {
		return "", err
	}
	if err := r.writeDirCache(files); err != nil {
		return "", err
	}
	if err := r.writeCurrentRef(commitID); err != nil {
		return "", err
	}
	return commitID, nil
}

// Head returns the current commit id, or "" before the first snapshot.
func (r *Repository) Head() (string, error) {
	ref, err := currentRefPath(r.metadata)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if err := object.RequireID(id); err != nil {
		return "", fmt.Errorf("current ref contains an invalid object id: %w", err)
	}
	return id, nil
}

// ReadCommit reads and validates a commit object.
func (r *Repository) ReadCommit(id string) (*object.Commit, error) {
	t, payload, err := r.store.Get(id)
	if err != nil {
		return nil, err
	}
	if t != object.TypeCommit {
		return nil, fmt.Errorf("object is not a commit: %s", id)
	}
	return object.DecodeCommit(payload)
}

func (r *Repository) readTree(id string) (*object.Tree, error) {
	t, payload, err := r.store.Get(id)
	if err != nil {
		return nil, err
	}
	if t != object.TypeTree {
		return nil, fmt.Errorf("object is not a tree: %s", id)
	}
	return object.DecodeTree(payload)
}

// ResolveCommit resolves HEAD, a full commit id, or an unambiguous 7+
// character prefix, optionally followed by ancestor steps: "~" means one
// generation, "~N" means N, and repeated steps accumulate.
func (r *Repository) ResolveCommit(revision string) (string, error) {
	if strings.TrimSpace(revision) == "" {
		return "", errors.New("snapshot revision cannot be empty")
	}
	spec := strings.TrimSpace(revision)
	var generations int64
	for {
		tilde := strings.LastIndexByte(spec, '~')
		if tilde < 0 {
			break
		}
		suffix := spec[tilde+1:]
		step := int64(1)
		if suffix != "" {
			for i := 0; i < len(suffix); i++ {
				if suffix[i] < '0' || suffix[i] > '9' {
					return "", fmt.Errorf("invalid ancestor expression: %s", revision)
				}
			}
			var err error
			step, err = strconv.ParseInt(suffix, 10, 64)
			if err != nil {
				return "", fmt.Errorf("ancestor count is too large: %s", suffix)
			}
		}
		generations += step
		if generations < 0 {
			return "", fmt.Errorf("ancestor count is too large: %s", revision)
		}
		spec = spec[:tilde]
	}
	if spec == "" {
		return "", fmt.Errorf("revision names no starting snapshot: %s", revision)
	}

	var id string
	switch {
	case spec == "HEAD" || spec == "@":
		head, err := r.Head()
		if err != nil {
			return "", err
		}
		if head == "" {
			return "", errors.New("no snapshots exist yet")
		}
		id = head
	case len(spec) == object.IDHexLength:
		id = strings.ToLower(spec)
		if err := object.RequireID(id); err != nil {
			return "", fmt.Errorf("invalid snapshot id: %s", revision)
		}
		if !r.store.Contains(id) {
			return "", fmt.Errorf("unknown snapshot: %s", revision)
		}
	default:
		if len(spec) < 7 {
			return "", errors.New("snapshot prefixes must contain at least 7 hex characters")
		}
		candidates, err := r.store.FindByPrefix(spec)
		if err != nil {
			return "", err
		}
		var commits []string
		for _, candidate := range candidates {
			// A matching blob or tree is not a matching snapshot.
			if _, err := r.ReadCommit(candidate); err == nil {
				commits = append(commits, candidate)
			}
		}
		if len(commits) == 0 {
			return "", fmt.Errorf("unknown snapshot: %s", revision)
		}
		if len(commits) > 1 {
			return "", fmt.Errorf("ambiguous snapshot prefix: %s", revision)
		}
		id = commits[0]
	}

	if _, err := r.ReadCommit(id); err != nil {
		return "", err
	}
	for range generations {
		commit, err := r.ReadCommit(id)
		if err != nil {
			return "", err
		}
		if len(commit.Parents) == 0 {
			return "", fmt.Errorf("%s walks beyond the beginning of history", revision)
		}
		id = commit.Parents[0]
	}
	return id, nil
}

// CommitInfo is a commit paired with the content address used to reach it.
type CommitInfo struct {
	ID     string
	Commit *object.Commit
}

// History walks the commit graph depth-first, suppressing duplicate
// ancestors, and returns up to limit commits starting at startRevision.
func (r *Repository) History(startRevision string, limit int) ([]CommitInfo, error) {
	if limit < 1 {
		return nil, errors.New("history limit must be positive")
	}
	start, err := r.ResolveCommit(startRevision)
	if err != nil {
		return nil, err
	}

	pending := []string{start}
	visited := make(map[string]bool)
	var result []CommitInfo
	for len(pending) > 0 && len(result) < limit {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		commit, err := r.ReadCommit(id)
		if err != nil {
			return nil, err
		}
		result = append(result, CommitInfo{ID: id, Commit: commit})
		for i := len(commit.Parents) - 1; i >= 0; i-- {
			pending = append(pending, commit.Parents[i])
		}
	}
	return result, nil
}

// ObjectCount returns the number of stored objects.
func (r *Repository) ObjectCount() (int64, error) {
	return r.store.Count()
}

func currentRefPath(metadata string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(metadata, "HEAD"))
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(head, "ref: ") {
		return "", errors.New("detached or malformed HEAD is not supported")
	}
	refName := head[len("ref: "):]
	if !strings.HasPrefix(refName, "refs/") || strings.Contains(refName, "..") ||
		strings.ContainsRune(refName, '\\') {
		return "", fmt.Errorf("unsafe HEAD ref: %s", refName)
	}
	ref := filepath.Join(metadata, filepath.FromSlash(refName))
	refsRoot := filepath.Join(metadata, "refs")
	if ref != refsRoot && !strings.HasPrefix(ref, refsRoot+string(filepath.Separator)) {
		return "", errors.New("HEAD ref escapes repository metadata")
	}
	return ref, nil
}

func (r *Repository) writeCurrentRef(commitID string) error {
	if err := object.RequireID(commitID); err != nil {
		return err
	}
	ref, err := currentRefPath(r.metadata)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ref), ".ref-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(commitID + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, ref); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}
