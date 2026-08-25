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

// errNoSearchIndex is OpenSearcher's exact error when no index has been
// built yet.
var errNoSearchIndex = errors.New("no search index; run 'snapvault index' first")

// errIndexOutdated is OpenSearcher's exact error when the index on disk was
// built by the older SVX1 format. Its text intentionally differs from
// search.ErrIndexOutdated's own message ("... run 'snapvault index' to
// rebuild"): every OpenSearcher failure ends in "run 'snapvault index'
// first", the same instruction errNoSearchIndex gives, so a caller can
// react to either without inspecting which one it got.
var errIndexOutdated = errors.New("search index was built by an older SnapVault; run 'snapvault index' first")

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
// commit's message, the matching chunk's position and snippet, and its
// fused score.
type FindResult struct {
	BlobID   string
	Path     string
	CommitID string
	Message  string
	Snippet  string
	Sequence int32
	Score    float32
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
// reachable from any ref is extracted, chunked per p, and embedded with
// embedder, then written atomically to .snapvault/index/embeddings.svi. The
// rebuild is always full, so a run with a different embedder or a different
// p than an existing index naturally replaces it — there is nothing to
// reuse or reconcile.
func (r *Repository) Index(embedder search.Embedder, p search.Pipeline) (IndexStats, error) {
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

	idx := search.NewIndex(embedder.ID(), 0, p.IndexHash())
	var stats IndexStats
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
		chunks := search.ChunkText(text, p)
		for _, c := range chunks {
			vec, err := embedder.Embed(c.Text)
			if err != nil {
				return IndexStats{}, fmt.Errorf("embedding %s: %w", id, err)
			}
			idx.Add(id, c, vec, search.Terms(c.Text, p.Stopwords))
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
	idx.Dim = int32(dim)
	if err := search.Write(r.indexPath(), idx); err != nil {
		return IndexStats{}, err
	}
	return stats, nil
}

// Searcher holds an open search index, the embedder it names, and the map
// of every blob currently reachable from any ref, so a caller can rank many
// queries without re-decoding the index or re-walking history for each one.
// It holds the repository's process-level lock for its entire lifetime, so
// a caller must Close it promptly.
type Searcher struct {
	repo      *Repository
	lock      *repoLock
	pipeline  search.Pipeline
	embedder  search.Embedder
	idx       *search.Index
	locations map[string]BlobLocation
	closed    bool
}

// OpenSearcher acquires the repository lock, decodes the search index, and
// resolves every blob currently reachable from any ref, for ranking with p's
// query-time settings. A missing index or one built by the older SVX1
// format both fail with an error ending in "run 'snapvault index' first".
func (r *Repository) OpenSearcher(p search.Pipeline) (*Searcher, error) {
	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return nil, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			lock.close()
		}
	}()

	if _, err := os.Stat(r.indexPath()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoSearchIndex
		}
		return nil, err
	}
	idx, err := search.Read(r.indexPath())
	if err != nil {
		if errors.Is(err, search.ErrIndexOutdated) {
			return nil, errIndexOutdated
		}
		return nil, err
	}
	embedder, err := search.NewEmbedder(idx.EmbedderID)
	if err != nil {
		return nil, err
	}
	locations, err := r.reachableSearchBlobs()
	if err != nil {
		return nil, err
	}

	releaseOnError = false
	return &Searcher{
		repo:      r,
		lock:      lock,
		pipeline:  p,
		embedder:  embedder,
		idx:       idx,
		locations: locations,
	}, nil
}

// EmbedderID returns the embedder id the open index was built with.
func (s *Searcher) EmbedderID() string { return s.idx.EmbedderID }

// IndexHash returns the open index's stored Pipeline.IndexHash(), for
// comparison against a fresh Pipeline's own IndexHash to detect a
// chunking-settings mismatch.
func (s *Searcher) IndexHash() string { return s.idx.IndexHash }

// Rank ranks every reachable blob's chunks against query, hiding entries
// for blobs history has made unreachable from any ref.
func (s *Searcher) Rank(query string) ([]search.Result, error) {
	allow := func(blobID string) bool {
		_, ok := s.locations[blobID]
		return ok
	}
	return search.Rank(s.pipeline, s.embedder, s.idx, query, allow)
}

// Find ranks query, keeps each matching blob's best-scoring chunk, and
// resolves the top limit blobs to where they currently live: the newest
// commit reachable from any ref that still contains the blob, and its path
// there.
func (s *Searcher) Find(query string, limit int) ([]FindResult, error) {
	ranked, err := s.Rank(query)
	if err != nil {
		return nil, err
	}
	grouped := search.GroupByBlob(ranked)
	if len(grouped) > limit {
		grouped = grouped[:limit]
	}

	results := make([]FindResult, 0, len(grouped))
	for _, res := range grouped {
		loc, ok := s.locations[res.BlobID]
		if !ok {
			// Rank's allow function already hid unreachable blobs; this
			// only guards against a future change to that invariant rather
			// than a case reachable today.
			continue
		}
		commit, err := s.repo.ReadCommit(loc.CommitID)
		if err != nil {
			return nil, err
		}
		results = append(results, FindResult{
			BlobID:   res.BlobID,
			Path:     loc.Path,
			CommitID: loc.CommitID,
			Message:  firstMessageLine(commit.Message),
			Snippet:  res.Snippet,
			Sequence: res.Sequence,
			Score:    res.Score,
		})
	}
	return results, nil
}

// Locate reports the newest commit and path a blob is reachable at, or
// false if the blob is not (or no longer) reachable from any ref.
func (s *Searcher) Locate(blobID string) (BlobLocation, bool) {
	loc, ok := s.locations[blobID]
	return loc, ok
}

// Close releases the repository lock this Searcher has held since
// OpenSearcher. Calling Close more than once is safe.
func (s *Searcher) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.close()
}

// Find opens a Searcher with the default pipeline, ranks query, and closes
// the Searcher: a thin, one-shot wrapper for callers that do not need to
// issue more than one query. eval.Run uses OpenSearcher directly instead,
// so a batch of queries shares one lock acquisition and one index decode.
func (r *Repository) Find(query string, limit int) ([]FindResult, error) {
	s, err := r.OpenSearcher(search.DefaultPipeline())
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Find(query, limit)
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

// HeadPaths returns the id of the blob currently reachable at every path in
// the commit HEAD points to: a single walk of HEAD's own tree, not the
// blended cross-history view reachableSearchBlobs builds for find. eval.Run
// uses it once per run to resolve every question's path "at HEAD" per the
// eval spec, rather than the newest commit that merely still contains a
// given blob's content. It returns an empty, non-nil map if the repository
// has no snapshots yet.
func (r *Repository) HeadPaths() (map[string]string, error) {
	head, err := r.Head()
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string)
	if head == "" {
		return paths, nil
	}
	commit, err := r.ReadCommit(head)
	if err != nil {
		return nil, err
	}
	if err := r.walkTreeForHeadPaths(commit.TreeID, "", paths); err != nil {
		return nil, err
	}
	return paths, nil
}

func (r *Repository) walkTreeForHeadPaths(treeID string, prefix string, paths map[string]string) error {
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
			if err := r.walkTreeForHeadPaths(entry.ObjectID, path, paths); err != nil {
				return err
			}
			continue
		}
		paths[path] = entry.ObjectID
	}
	return nil
}

// BlobBytes returns the raw stored bytes of the blob id, for a caller that
// needs a blob's full original content rather than a chunk's snippet —
// eval.Run extracts and locates highlights in it directly, independent of
// whatever chunking a search index happens to hold.
func (r *Repository) BlobBytes(id string) ([]byte, error) {
	typ, payload, err := r.store.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != object.TypeBlob {
		return nil, fmt.Errorf("object is not a blob: %s", id)
	}
	return payload, nil
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
