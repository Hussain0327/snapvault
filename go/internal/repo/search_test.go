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

	stats, err := r.Index(search.LexicalEmbedder{})
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
}

func TestIndexIsDeterministic(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "quartz spires rise above the valley")
	write(t, r.Root(), "b.txt", "quiet valleys hold ancient quartz stones")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	path := filepath.Join(r.Metadata(), search.DirName, search.FileName)
	if _, err := r.Index(search.LexicalEmbedder{}); err != nil {
		t.Fatalf("Index (1) = %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (1) = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}); err != nil {
		t.Fatalf("Index (2) = %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (2) = %v", err)
	}
	if string(first) != string(second) {
		t.Error("Index produced different bytes across two runs with the same embedder")
	}
}

func TestIndexEmbedderMismatchRebuilds(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "the harbor lighthouse guides ships through fog")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}); err != nil {
		t.Fatalf("Index(builtin) = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{1, 0, 0, 0}})
	}))
	defer server.Close()
	ollama := search.NewOllamaEmbedder("test-model").WithBaseURL(server.URL)

	if _, err := r.Index(ollama); err != nil {
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

	if _, err := r.Index(search.LexicalEmbedder{}); err != nil {
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
}

func TestIndexAndFindHoldTheRepoLock(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "notes.txt", "content")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := r.Index(search.LexicalEmbedder{}); err != nil {
		t.Fatalf("Index = %v", err)
	}

	lock, err := acquireLock(filepath.Join(r.Metadata(), "lock"))
	if err != nil {
		t.Fatalf("acquireLock = %v", err)
	}
	defer lock.close()

	if _, err := r.Index(search.LexicalEmbedder{}); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("Index under a held lock = %v, want already-running error", err)
	}
	if _, err := r.Find("content", 5); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("Find under a held lock = %v, want already-running error", err)
	}
}
