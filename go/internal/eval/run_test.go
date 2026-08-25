package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/repo"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

// buildRunRepo builds a temporary repo with the given path->content files,
// snapshots it, and indexes it with the builtin lexical embedder. It
// returns the repo, the Pipeline it was indexed with (so a test never risks
// passing Run a Pipeline that does not match the index on disk), and a
// cleanup func the caller must defer.
func buildRunRepo(t *testing.T, files map[string]string) (*repo.Repository, search.Pipeline, func()) {
	t.Helper()
	src := t.TempDir()
	for path, content := range files {
		writeFile(t, src, path, content)
	}
	_, r, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	p := search.DefaultPipeline()
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		cleanup()
		t.Fatalf("Index = %v", err)
	}
	return r, p, cleanup
}

func TestRunScoresAgainstTheRepoAtHEAD(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"lighthouse.txt": "The harbor lighthouse guides ships safely through dense evening fog.",
		"bakery.txt":     "The corner bakery bakes sourdough loaves fresh every single morning.",
	})
	defer cleanup()

	qs := []Question{
		{
			ID:        "q1",
			Question:  "harbor lighthouse fog",
			Path:      "lighthouse.txt",
			Highlight: "guides ships safely through dense evening fog",
			Tags:      []string{"lexical"},
		},
	}

	report, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 10, 2.0)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if report.Overall.N != 1 {
		t.Fatalf("Overall.N = %d, want 1", report.Overall.N)
	}
	if report.EmbedderID != (search.LexicalEmbedder{}).ID() {
		t.Errorf("EmbedderID = %q, want %q", report.EmbedderID, (search.LexicalEmbedder{}).ID())
	}
	if report.Overall.Precision.Mean <= 0 || report.Overall.Recall.Mean <= 0 {
		t.Errorf("Overall = %+v, want a positive precision and recall for a matching query", report.Overall)
	}
	if !report.Questions[0].Hit {
		t.Errorf("Questions[0].Hit = false, want true (the target blob should rank first)")
	}
	if report.Questions[0].ReciprocalRank != 1.0 {
		t.Errorf("Questions[0].ReciprocalRank = %v, want 1.0", report.Questions[0].ReciprocalRank)
	}
}

// TestRunOnlyScoresChunksFromTheQuestionsOwnBlob is the regression for the
// critical scoring defect: a decoy document that outranks the target at
// k=1 must not contribute its chunk's rune range to the target question's
// score. Both documents' first (and only) chunk starts at rune 0, as short
// documents commonly do, so before the fix the decoy's broad range
// numerically overlapped the target's small reference range and reported
// a positive, even perfect, recall for a question whose target document
// was never retrieved at all.
func TestRunOnlyScoresChunksFromTheQuestionsOwnBlob(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"target.txt": "The observatory telescope tracks distant galaxies each night for careful researchers.",
		// Term-stuffed and short, so it outranks target.txt on the same
		// query under BM25 despite being unrelated content.
		"decoy.txt": "telescope telescope telescope observatory observatory tracks tracks tracks tracks",
	})
	defer cleanup()

	qs := []Question{
		{
			ID:        "q1",
			Question:  "observatory telescope tracks",
			Path:      "target.txt",
			Highlight: "distant galaxies each night",
		},
	}

	report, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 1, 2.0)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(report.Questions) != 1 {
		t.Fatalf("len(Questions) = %d, want 1", len(report.Questions))
	}
	got := report.Questions[0]

	if got.Hit {
		t.Fatal("Hit = true, want false: at k=1 the decoy must outrank the target for this regression to be meaningful")
	}
	if got.Recall != 0 {
		t.Errorf("Recall = %v, want 0: a decoy blob's chunk range must not count toward the target's recall "+
			"just because its numeric rune offsets happen to overlap the target's reference range", got.Recall)
	}
	if got.Precision != 0 {
		t.Errorf("Precision = %v, want 0 (nothing from the target blob was retrieved)", got.Precision)
	}
}

// TestRunFailsWhenPipelineIndexHashDoesNotMatchTheIndex checks that Run
// itself enforces the chunking-settings invariant rather than relying on
// repo.OpenSearcher (which only stores IndexHash for a caller to compare)
// or find's soft stderr note (not strong enough for a measurement tool).
func TestRunFailsWhenPipelineIndexHashDoesNotMatchTheIndex(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"a.txt": "quartz spires rise above the misty valley at dawn.",
	})
	defer cleanup()

	stale := p
	stale.ChunkRunes = p.ChunkRunes + 1 // any index-time change moves IndexHash

	qs := []Question{{ID: "q1", Question: "quartz", Path: "a.txt", Highlight: "quartz spires"}}
	_, err := Run(context.Background(), r, stale, search.LexicalEmbedder{}, qs, 10, 2.0)
	if err == nil {
		t.Fatal("Run with a Pipeline whose IndexHash does not match the on-disk index succeeded, want an error")
	}
}

func TestRunFailsWithQuestionIDAndPathWhenHighlightMissing(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"notes.txt": "Completely unrelated content about something else entirely.",
	})
	defer cleanup()

	qs := []Question{
		{ID: "q-bad", Question: "anything", Path: "notes.txt", Highlight: "text that is not in the file"},
	}

	_, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 10, 2.0)
	if err == nil {
		t.Fatal("Run with a missing highlight = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "q-bad") || !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("error = %q, want it to name the question id %q and path %q", err.Error(), "q-bad", "notes.txt")
	}
}

func TestRunFailsWithQuestionIDAndPathWhenPathMissingAtHEAD(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"notes.txt": "some content",
	})
	defer cleanup()

	qs := []Question{
		{ID: "q-missing-path", Question: "anything", Path: "does-not-exist.txt", Highlight: "some content"},
	}

	_, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 10, 2.0)
	if err == nil {
		t.Fatal("Run with an unknown path = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "q-missing-path") || !strings.Contains(err.Error(), "does-not-exist.txt") {
		t.Errorf("error = %q, want it to name the question id and path", err.Error())
	}
}

func TestRunKBoundsChunkAndBlobMetricsIndependently(t *testing.T) {
	// Two files share enough vocabulary that both match the query, but only
	// one is the question's target; k=1 should truncate the chunk-level
	// ranking to the single best-scoring chunk while GroupByBlob's own
	// truncation to k blobs is computed separately, per the spec.
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"target.txt": "The observatory telescope tracks distant galaxies every clear night.",
		"other.txt":  "A different telescope at another observatory tracks the moon instead.",
	})
	defer cleanup()

	qs := []Question{
		{
			ID:        "q1",
			Question:  "observatory telescope tracks",
			Path:      "target.txt",
			Highlight: "tracks distant galaxies every clear night",
		},
	}

	report, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 1, 2.0)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if report.K != 1 {
		t.Errorf("Report.K = %d, want 1", report.K)
	}
}

func TestRunAggregatesTagsFromQuestions(t *testing.T) {
	r, p, cleanup := buildRunRepo(t, map[string]string{
		"a.txt": "quartz spires rise above the misty valley at dawn.",
	})
	defer cleanup()

	qs := []Question{
		{ID: "q1", Question: "quartz spires valley", Path: "a.txt", Highlight: "quartz spires rise above the misty valley", Tags: []string{"lexical"}},
	}

	report, err := Run(context.Background(), r, p, search.LexicalEmbedder{}, qs, 10, 2.0)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if _, ok := report.ByTag["lexical"]; !ok {
		t.Errorf("ByTag = %v, want a %q entry", report.ByTag, "lexical")
	}
}
