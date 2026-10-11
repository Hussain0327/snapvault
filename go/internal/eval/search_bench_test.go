package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/repo"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

var benchmarkFindResults []repo.FindResult

// BenchmarkGoldenFind measures real queries over the committed corpus using
// the builtin embedder. Reopen includes index decoding and history traversal
// on every query, but uses warm OS file caches and excludes process startup.
// Reuse keeps a Searcher open, as batch evaluation does.
func BenchmarkGoldenFind(b *testing.B) {
	f, err := os.Open(filepath.Join(goldenDir, "questions.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	questions, err := LoadQuestions(f)
	closeErr := f.Close()
	if err != nil {
		b.Fatal(err)
	}
	if closeErr != nil {
		b.Fatal(closeErr)
	}
	if len(questions) == 0 {
		b.Fatal("benchmark needs at least one question")
	}
	_, r, cleanup, err := BuildCorpusRepo(filepath.Join(goldenDir, "corpus"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(cleanup)
	p := search.DefaultPipeline()
	if _, err := r.Index(search.LexicalEmbedder{}, p); err != nil {
		b.Fatal(err)
	}
	b.Run("Reopen", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var err error
			benchmarkFindResults, err = r.Find(questions[i%len(questions)].Question, goldenK)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Reuse", func(b *testing.B) {
		s, err := r.OpenSearcher(p)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := s.Close(); err != nil {
				b.Error(err)
			}
		})
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var err error
			benchmarkFindResults, err = s.Find(questions[i%len(questions)].Question, goldenK)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
