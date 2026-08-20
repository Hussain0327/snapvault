package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Hussain0327/snapvault/go/internal/object"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

// errNoSearchIndex is Find's exact error when no index has been built yet.
var errNoSearchIndex = errors.New("no search index; run 'snapvault index' first")

// IndexStats reports what one Index call did: how many blobs it embedded
// (and how many chunks those blobs produced), and how many blobs it skipped
// for lacking extractable text.
type IndexStats struct {
	Blobs   int
	Chunks  int
	Skipped int
}

// FindResult is one ranked search match, resolved to where it currently
// lives: the blob's content id, the path it is reachable at in the newest
// commit that still contains it, that commit's id, the first line of that
// commit's message, and the matching chunk's snippet.
type FindResult struct {
	BlobID   string
	Path     string
	CommitID string
	Message  string
	Snippet  string
}

// BlobLocation is where the newest commit reachable from any ref currently
// places one blob: the commit's id and the blob's path in that commit's
// tree.
type BlobLocation struct {
	CommitID string
	Path     string
}

func (r *Repository) indexPath() string {
	return filepath.Join(r.metadata, search.DirName, search.FileName)
}

// Index rebuilds the repository's search index sidecar: every unique blob
// reachable from any ref is extracted, chunked, and embedded with embedder,
// then written atomically to .snapvault/index/embeddings.svi. The rebuild is
// always full, so a run with a different embedder than an existing index
// naturally replaces it — there is nothing to reuse or reconcile.
func (r *Repository) Index(embedder search.Embedder) (IndexStats, error) {
	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return IndexStats{}, err
	}
	defer lock.close()

	locations, err := r.reachableSearchBlobs()
	if err != nil {
		return IndexStats{}, err
	}
	ids := make([]string, 0, len(locations))
	for id := range locations {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	var stats IndexStats
	var entries []search.Entry
	for _, id := range ids {
		typ, payload, err := r.store.Get(id)
		if err != nil {
			return IndexStats{}, err
		}
		if typ != object.TypeBlob {
			continue
		}
		text, ok := search.Extract(payload)
		if !ok {
			stats.Skipped++
			continue
		}
		chunks := search.ChunkText(text)
		for _, c := range chunks {
			vec, err := embedder.Embed(c.Text)
			if err != nil {
				return IndexStats{}, fmt.Errorf("embedding %s: %w", id, err)
			}
			entries = append(entries, search.Entry{
				BlobID:   id,
				Sequence: int32(c.Sequence),
				Snippet:  c.Snippet,
				Vector:   vec,
			})
		}
		stats.Blobs++
		stats.Chunks += len(chunks)
	}

	// embedder.Dim() only reports an ollama embedder's dimension once Embed
	// has succeeded at least once; when nothing was embedded (an empty
	// repository, or every blob skipped) fall back to 1 so Write's
	// positive-dimension invariant holds even though no vector's width
	// matters when there are no entries to hold one.
	dim := embedder.Dim()
	if dim <= 0 {
		dim = 1
	}
	idx := search.Index{EmbedderID: embedder.ID(), Dim: int32(dim), Entries: entries}
	if err := search.Write(r.indexPath(), idx); err != nil {
		return IndexStats{}, err
	}
	return stats, nil
}

// Find ranks the repository's search index against query and resolves each
// match to where it lives right now: the newest commit reachable from any
// ref that still contains the matching blob, and that blob's path there.
// Resolution always walks the repository fresh, so a result is never stale
// even if the index predates a later snapshot.
func (r *Repository) Find(query string, limit int) ([]FindResult, error) {
	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return nil, err
	}
	defer lock.close()

	if _, err := os.Stat(r.indexPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoSearchIndex
		}
		return nil, err
	}
	idx, err := search.Read(r.indexPath())
	if err != nil {
		return nil, err
	}
	embedder, err := search.NewEmbedder(idx.EmbedderID)
	if err != nil {
		return nil, err
	}
	matches, err := search.Search(embedder, idx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}

	locations, err := r.reachableSearchBlobs()
	if err != nil {
		return nil, err
	}

	results := make([]FindResult, 0, len(matches))
	for _, m := range matches {
		loc, ok := locations[m.BlobID]
		if !ok {
			// The index remembers a blob no longer reachable from any ref;
			// nothing to annotate it with, so it is left out of the results
			// rather than shown with a stale or missing location.
			continue
		}
		commit, err := r.ReadCommit(loc.CommitID)
		if err != nil {
			return nil, err
		}
		results = append(results, FindResult{
			BlobID:   m.BlobID,
			Path:     loc.Path,
			CommitID: loc.CommitID,
			Message:  firstMessageLine(commit.Message),
			Snippet:  m.Snippet,
		})
	}
	return results, nil
}

// reachableSearchBlobs walks every commit reachable from every ref and every
// tree those commits root, recording for each unique blob the newest commit
// that contains it and the path referencing it there. "Newest" follows the
// same discovery order as reachableObjects: commits are discovered
// depth-first from each ref's head (first-parent line first, matching
// History's order), then trees are walked oldest-discovered commit to
// newest so a later assignment overwrites an earlier one. Unlike
// reachableObjects, every commit's tree is walked in full — a blob's
// location needs the commit id a name hint does not, so an unchanged
// subtree cannot be skipped just because an older commit already visited it.
func (r *Repository) reachableSearchBlobs() (map[string]BlobLocation, error) {
	heads, err := r.allRefHeads()
	if err != nil {
		return nil, err
	}

	visited := make(map[string]bool)
	var commits []string
	for _, head := range heads {
		pending := []string{head}
		for len(pending) > 0 {
			id := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if visited[id] {
				continue
			}
			visited[id] = true
			commits = append(commits, id)
			commit, err := r.ReadCommit(id)
			if err != nil {
				return nil, err
			}
			for i := len(commit.Parents) - 1; i >= 0; i-- {
				pending = append(pending, commit.Parents[i])
			}
		}
	}

	locations := make(map[string]BlobLocation)
	for i := len(commits) - 1; i >= 0; i-- {
		commit, err := r.ReadCommit(commits[i])
		if err != nil {
			return nil, err
		}
		if err := r.walkTreeForSearch(commit.TreeID, "", commits[i], locations); err != nil {
			return nil, err
		}
	}
	return locations, nil
}

func (r *Repository) walkTreeForSearch(
	treeID string, prefix string, commitID string, locations map[string]BlobLocation,
) error {
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
			if err := r.walkTreeForSearch(entry.ObjectID, path, commitID, locations); err != nil {
				return err
			}
			continue
		}
		locations[entry.ObjectID] = BlobLocation{CommitID: commitID, Path: path}
	}
	return nil
}

// allRefHeads returns the commit id at the tip of every ref, sorted for a
// deterministic walk order. Today there is only ever one, refs/heads/main,
// but this walks the whole refs/ tree so a future branch command needs no
// change here.
func (r *Repository) allRefHeads() ([]string, error) {
	refsRoot := filepath.Join(r.metadata, "refs")
	var heads []string
	err := filepath.WalkDir(refsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		id := strings.TrimSpace(string(raw))
		if id == "" {
			return nil
		}
		if err := object.RequireID(id); err != nil {
			return fmt.Errorf("ref %s contains an invalid object id: %w", path, err)
		}
		heads = append(heads, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(heads)
	return heads, nil
}

// firstMessageLine returns the first line of a commit message, normalizing
// line terminators the way messageLines does in the cli package.
func firstMessageLine(message string) string {
	normalized := strings.ReplaceAll(message, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if idx := strings.IndexByte(normalized, '\n'); idx >= 0 {
		return normalized[:idx]
	}
	return normalized
}
