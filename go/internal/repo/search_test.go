package repo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/search"
)

func TestIndexBuildsEntriesAndReportsStats(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "The obsidian lantern illuminates ancient caverns.")
	write(t, r.Root(), "binary.bin", strings.Repeat("\x01", 100))
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	p := search.DefaultPipeline()
	stats, err := r.Index(search.LexicalEmbedder{}, p)
	if err != nil {
		t.Fatalf("Index = %v", err)
	}
	if stats.Blobs != 1 {
		t.Errorf("Blobs = %d, want 1", stats.Blobs)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
	if stats.Chunks == 0 {
		t.Error("Chunks = 0, want at least 1")
	}

	idx, err := search.Read(filepath.Join(r.Metadata(), search.DirName, search.FileName))
	if err != nil {
		t.Fatalf("search.Read = %v", err)
	}
	if idx.EmbedderID != (search.LexicalEmbedder{}).ID() {
		t.Errorf("EmbedderID = %q, want %q", idx.EmbedderID, (search.LexicalEmbedder{}).ID())
	}
	if idx.IndexHash != p.IndexHash() {
		t.Errorf("IndexHash = %q, want %q", idx.IndexHash, p.IndexHash())
	}
}

func TestIndexIsDeterministic(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "quartz spires rise above the valley")
	write(t, r.Root(), "b.txt", "quiet valleys hold ancient quartz stones")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	path := filepath.Join(r.Metadata(), search.DirName, search.FileName)
	p := search.DefaultPipeline()
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		t.Fatalf("Index (1) = %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (1) = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		t.Fatalf("Index (2) = %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (2) = %v", err)
	}
	if string(first) != string(second) {
		t.Error("Index produced different bytes across two runs with the same embedder and pipeline")
	}
}

func TestIndexEmbedderMismatchRebuilds(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "the harbor lighthouse guides ships through fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	p := search.DefaultPipeline()
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		t.Fatalf("Index(builtin) = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{1, 0, 0, 0}})
	}))
	defer server.Close()
	ollama := search.NewOllamaEmbedder("test-model").WithBaseURL(server.URL)

	if _, err := r.Index(ollama, p); err != nil {
		t.Fatalf("Index(ollama) = %v", err)
	}

	idx, err := search.Read(filepath.Join(r.Metadata(), search.DirName, search.FileName))
	if err != nil {
		t.Fatalf("search.Read = %v", err)
	}
	if idx.EmbedderID != "ollama:test-model" {
		t.Errorf("EmbedderID after rebuild = %q, want %q", idx.EmbedderID, "ollama:test-model")
	}
}

func TestFindWithoutIndexReturnsExactError(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	_, err := r.Find("anything", 5)
	if err == nil || err.Error() != "no search index; run 'snapvault index' first" {
		t.Errorf("Find without an index = %v, want the exact no-index error", err)
	}
}

func TestOpenSearcherWithoutIndexReturnsExactError(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	_, err := r.OpenSearcher(search.DefaultPipeline())
	if err == nil || err.Error() != "no search index; run 'snapvault index' first" {
		t.Errorf("OpenSearcher without an index = %v, want the exact no-index error", err)
	}
}

// TestOpenSearcherOutdatedIndexReturnsExactError proves that an index left
// over from the older SVX1 format fails with a message ending the same way
// as the missing-index message ("run 'snapvault index' first"), not
// search.ErrIndexOutdated's own "... to rebuild" wording.
func TestOpenSearcherOutdatedIndexReturnsExactError(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	indexPath := filepath.Join(r.Metadata(), search.DirName, search.FileName)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("SVX1"), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	_, err := r.OpenSearcher(search.DefaultPipeline())
	want := "search index was built by an older SnapVault; run 'snapvault index' first"
	if err == nil || err.Error() != want {
		t.Errorf("OpenSearcher on an SVX1 index = %v, want %q", err, want)
	}
}

func TestFindResolvesNewestCommitAndPath(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "old-name.txt", "the harbor lighthouse guides ships through dense fog")
	if _, err := r.Snapshot("first version"); err != nil {
		t.Fatalf("Snapshot 1 = %v", err)
	}
	if err := os.Remove(filepath.Join(r.Root(), "old-name.txt")); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	write(t, r.Root(), "new-name.txt", "the harbor lighthouse guides ships through dense fog")
	secondCommit, err := r.Snapshot("renamed")
	if err != nil {
		t.Fatalf("Snapshot 2 = %v", err)
	}

	if _, err := r.Index(search.LexicalEmbedder{}, search.DefaultPipeline()); err != nil {
		t.Fatalf("Index = %v", err)
	}
	results, err := r.Find("harbor lighthouse fog", 5)
	if err != nil {
		t.Fatalf("Find = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Find returned %d results, want 1", len(results))
	}
	if results[0].Path != "new-name.txt" {
		t.Errorf("Path = %q, want %q (the newest name)", results[0].Path, "new-name.txt")
	}
	if results[0].CommitID != secondCommit {
		t.Errorf("CommitID = %s, want the newest commit %s", results[0].CommitID, secondCommit)
	}
	if results[0].Message != "renamed" {
		t.Errorf("Message = %q, want %q", results[0].Message, "renamed")
	}
	if results[0].Sequence != 0 {
		t.Errorf("Sequence = %d, want 0 (the text is one chunk)", results[0].Sequence)
	}
	if results[0].Score <= 0 {
		t.Errorf("Score = %v, want a positive fused score for a matching result", results[0].Score)
	}
}

func TestIndexAndFindHoldTheRepoLock(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}, search.DefaultPipeline()); err != nil {
		t.Fatalf("Index = %v", err)
	}

	lock, err := acquireLock(filepath.Join(r.Metadata(), "lock"))
	if err != nil {
		t.Fatalf("acquireLock = %v", err)
	}
	defer lock.close()

	if _, err := r.Index(search.LexicalEmbedder{}, search.DefaultPipeline()); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("Index under a held lock = %v, want already-running error", err)
	}
	if _, err := r.Find("content", 5); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("Find under a held lock = %v, want already-running error", err)
	}
	if _, err := r.OpenSearcher(search.DefaultPipeline()); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("OpenSearcher under a held lock = %v, want already-running error", err)
	}
}

// TestSearcherOutlivesTheOpeningCallForRepeatedQueries proves a single
// Searcher answers more than one Rank/Find call without losing its lock or
// its decoded state between them, and that a second OpenSearcher is refused
// until the first is closed.
func TestSearcherOutlivesTheOpeningCallForRepeatedQueries(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "the harbor lighthouse guides ships through fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}, search.DefaultPipeline()); err != nil {
		t.Fatalf("Index = %v", err)
	}

	s, err := r.OpenSearcher(search.DefaultPipeline())
	if err != nil {
		t.Fatalf("OpenSearcher = %v", err)
	}

	if _, err := r.OpenSearcher(search.DefaultPipeline()); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("second OpenSearcher while the first is open = %v, want already-running error", err)
	}

	for i := 0; i < 3; i++ {
		results, err := s.Find("harbor lighthouse fog", 5)
		if err != nil {
			t.Fatalf("Find (call %d) = %v", i, err)
		}
		if len(results) != 1 {
			t.Fatalf("Find (call %d) returned %d results, want 1", i, len(results))
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// Closing twice must be safe.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}

	again, err := r.OpenSearcher(search.DefaultPipeline())
	if err != nil {
		t.Fatalf("OpenSearcher after Close = %v", err)
	}
	defer again.Close()
}

func TestSearcherEmbedderIDAndIndexHash(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content about lighthouses and fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	p := search.DefaultPipeline()
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		t.Fatalf("Index = %v", err)
	}

	s, err := r.OpenSearcher(p)
	if err != nil {
		t.Fatalf("OpenSearcher = %v", err)
	}
	defer s.Close()

	if got, want := s.EmbedderID(), (search.LexicalEmbedder{}).ID(); got != want {
		t.Errorf("EmbedderID() = %q, want %q", got, want)
	}
	if got, want := s.IndexHash(), p.IndexHash(); got != want {
		t.Errorf("IndexHash() = %q, want %q", got, want)
	}
}

// TestSearcherIndexHashMismatchIsObservable proves a Searcher's IndexHash
// differs from a fresh Pipeline's IndexHash when the index on disk was
// built with different index-time settings, so a caller (the find command)
// can detect and report the mismatch itself.
func TestSearcherIndexHashMismatchIsObservable(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content about lighthouses and fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	built := search.DefaultPipeline()
	built.ChunkRunes = 400
	if _, err := r.Index(search.LexicalEmbedder{}, built); err != nil {
		t.Fatalf("Index = %v", err)
	}

	fresh := search.DefaultPipeline()
	s, err := r.OpenSearcher(fresh)
	if err != nil {
		t.Fatalf("OpenSearcher = %v", err)
	}
	defer s.Close()

	if s.IndexHash() == fresh.IndexHash() {
		t.Error("IndexHash() matches a Pipeline with different index-time settings, want a mismatch")
	}
	if s.IndexHash() != built.IndexHash() {
		t.Errorf("IndexHash() = %q, want the hash the index was actually built with, %q", s.IndexHash(), built.IndexHash())
	}
}

func TestSearcherLocate(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "the harbor lighthouse guides ships through fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}, search.DefaultPipeline()); err != nil {
		t.Fatalf("Index = %v", err)
	}

	s, err := r.OpenSearcher(search.DefaultPipeline())
	if err != nil {
		t.Fatalf("OpenSearcher = %v", err)
	}
	defer s.Close()

	results, err := s.Find("harbor lighthouse fog", 5)
	if err != nil {
		t.Fatalf("Find = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Find returned %d results, want 1", len(results))
	}

	loc, ok := s.Locate(results[0].BlobID)
	if !ok {
		t.Fatalf("Locate(%s) = false, want true", results[0].BlobID)
	}
	if loc.Path != "notes.txt" {
		t.Errorf("Locate(...).Path = %q, want %q", loc.Path, "notes.txt")
	}

	if _, ok := s.Locate(strings.Repeat("0", 64)); ok {
		t.Error("Locate on an unknown blob id = true, want false")
	}
}

// TestHeadPathsResolvesNestedFiles proves HeadPaths walks HEAD's tree (not
// history, and not the working directory) and returns every regular file's
// current blob id keyed by its path, including nested directories.
func TestHeadPathsResolvesNestedFiles(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "top.txt", "top level content")
	write(t, r.Root(), "docs/nested/deep.txt", "nested content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	paths, err := r.HeadPaths()
	if err != nil {
		t.Fatalf("HeadPaths = %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("HeadPaths returned %d entries, want 2: %v", len(paths), paths)
	}
	topID, ok := paths["top.txt"]
	if !ok || topID == "" {
		t.Errorf("HeadPaths()[%q] missing or empty", "top.txt")
	}
	nestedID, ok := paths["docs/nested/deep.txt"]
	if !ok || nestedID == "" {
		t.Errorf("HeadPaths()[%q] missing or empty", "docs/nested/deep.txt")
	}
	if topID == nestedID {
		t.Error("top.txt and docs/nested/deep.txt resolved to the same blob id, want distinct content")
	}
}

// TestHeadPathsReflectsHeadNotHistory proves a path deleted since an older
// commit is absent from HeadPaths, even though its blob is still reachable
// (and still shown by find/Locate) through that older commit.
func TestHeadPathsReflectsHeadNotHistory(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "gone.txt", "will be deleted")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot 1 = %v", err)
	}
	if err := os.Remove(filepath.Join(r.Root(), "gone.txt")); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	write(t, r.Root(), "stays.txt", "still here")
	if _, err := r.Snapshot("second"); err != nil {
		t.Fatalf("Snapshot 2 = %v", err)
	}

	paths, err := r.HeadPaths()
	if err != nil {
		t.Fatalf("HeadPaths = %v", err)
	}
	if _, ok := paths["gone.txt"]; ok {
		t.Error(`HeadPaths()["gone.txt"] present, want absent (deleted before HEAD)`)
	}
	if _, ok := paths["stays.txt"]; !ok {
		t.Error(`HeadPaths()["stays.txt"] absent, want present`)
	}
}

// TestHeadPathsEmptyRepoReturnsEmptyMap proves a repository with no
// snapshots yet reports an empty, non-nil map rather than an error, mirroring
// Head()'s own "" for "no HEAD yet" rather than failing.
func TestHeadPathsEmptyRepoReturnsEmptyMap(t *testing.T) {
	r := newTestRepo(t)
	paths, err := r.HeadPaths()
	if err != nil {
		t.Fatalf("HeadPaths on an empty repo = %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("HeadPaths on an empty repo = %v, want empty", paths)
	}
}

// TestBlobBytesReturnsRawContent proves BlobBytes returns exactly the bytes
// that were snapshotted, for the same blob id HeadPaths names.
func TestBlobBytesReturnsRawContent(t *testing.T) {
	r := newTestRepo(t)
	const content = "the exact bytes stored for this blob"
	write(t, r.Root(), "f.txt", content)
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	paths, err := r.HeadPaths()
	if err != nil {
		t.Fatalf("HeadPaths = %v", err)
	}
	blobID, ok := paths["f.txt"]
	if !ok {
		t.Fatalf("HeadPaths()[%q] missing", "f.txt")
	}

	data, err := r.BlobBytes(blobID)
	if err != nil {
		t.Fatalf("BlobBytes = %v", err)
	}
	if string(data) != content {
		t.Errorf("BlobBytes = %q, want %q", data, content)
	}
}

// TestBlobBytesRejectsNonBlobID proves BlobBytes refuses to hand back a
// commit or tree object's payload as if it were blob content.
func TestBlobBytesRejectsNonBlobID(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f.txt", "content")
	commitID, err := r.Snapshot("first")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	if _, err := r.BlobBytes(commitID); err == nil {
		t.Error("BlobBytes(commitID) = nil error, want an error")
	}
}
