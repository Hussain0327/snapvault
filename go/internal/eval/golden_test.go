package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/search"
)

// goldenDir holds the shared search-eval golden fixture: a corpus of
// original documents plus a question set, both frozen enough to run in CI
// as a quality floor for snapvault find. See
// tests/golden/search/MANIFEST.md for what each file covers.
const goldenDir = "../../../tests/golden/search"

// goldenK and goldenBeta are the eval spec's defaults, and match the
// "fbeta@10" key baseline.json records against.
const (
	goldenK    = 10
	goldenBeta = 2.0
)

// runGolden builds the golden corpus into a fresh temporary repository,
// indexes it with embedder, and runs the golden question set through Run.
func runGolden(t *testing.T, embedder search.Embedder) Report {
	t.Helper()

	f, err := os.Open(filepath.Join(goldenDir, "questions.jsonl"))
	if err != nil {
		t.Fatalf("open questions.jsonl: %v", err)
	}
	defer f.Close()
	qs, err := LoadQuestions(f)
	if err != nil {
		t.Fatalf("LoadQuestions: %v", err)
	}

	_, r, cleanup, err := BuildCorpusRepo(filepath.Join(goldenDir, "corpus"))
	if err != nil {
		t.Fatalf("BuildCorpusRepo: %v", err)
	}
	t.Cleanup(cleanup)

	p := search.DefaultPipeline()
	if _, err := r.Index(embedder, p); err != nil {
		t.Fatalf("Index: %v", err)
	}

	report, err := Run(context.Background(), r, p, embedder, qs, goldenK, goldenBeta)
	if err != nil {
		// A LocateHighlight failure surfaces here, satisfying "still
		// asserts every highlight locates" even when the baseline
		// comparison below is skipped.
		t.Fatalf("Run: %v", err)
	}
	return report
}

// goldenBaseline is the shape of tests/golden/search/baseline.json:
// {"<embedderID>": {"fbeta@10": <float>}}.
type goldenBaseline map[string]struct {
	FBeta10 float64 `json:"fbeta@10"`
}

// assertFBetaAtLeastBaseline compares report's headline FBeta@10 against
// baseline.json's floor for embedderID, skipping the comparison (but still
// logging the actual number) while no floor has been recorded yet.
func assertFBetaAtLeastBaseline(t *testing.T, report Report, embedderID string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(goldenDir, "baseline.json"))
	if err != nil {
		t.Fatalf("read baseline.json: %v", err)
	}
	var baseline goldenBaseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatalf("parse baseline.json: %v", err)
	}

	got := report.Overall.FBeta.Mean
	floor, ok := baseline[embedderID]
	if !ok {
		t.Logf("no baseline recorded yet for %q; fbeta@10 = %.4f (n=%d)", embedderID, got, report.Overall.N)
		return
	}
	if got < floor.FBeta10 {
		t.Errorf("fbeta@10 = %.4f, want >= baseline floor %.4f for %q", got, floor.FBeta10, embedderID)
	}
}

// TestGoldenFloorLexical runs the golden question set against the builtin
// lexical embedder and checks its headline FBeta@10 against baseline.json's
// recorded floor, if any.
func TestGoldenFloorLexical(t *testing.T) {
	embedder := search.LexicalEmbedder{}
	report := runGolden(t, embedder)
	assertFBetaAtLeastBaseline(t, report, embedder.ID())
}

// TestGoldenFloorStatic does the same as TestGoldenFloorLexical with the
// static Model2Vec embedder. It skips when the model is not installed,
// unless SNAPVAULT_REQUIRE_MODEL=1, in which case a missing model fails the
// test instead.
func TestGoldenFloorStatic(t *testing.T) {
	embedder, err := search.NewStaticEmbedder("potion-base-8M")
	if err != nil {
		if os.Getenv("SNAPVAULT_REQUIRE_MODEL") == "1" {
			t.Fatalf("SNAPVAULT_REQUIRE_MODEL=1 but the static model is unavailable: %v", err)
		}
		t.Skipf("skipping: %v (set SNAPVAULT_REQUIRE_MODEL=1 to require it)", err)
	}

	report := runGolden(t, embedder)
	assertFBetaAtLeastBaseline(t, report, embedder.ID())
}
