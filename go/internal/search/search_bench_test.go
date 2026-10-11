package search

import (
	"fmt"
	"math/rand"
	"testing"
)

var (
	benchmarkScores  []float32
	benchmarkRanking []int
	benchmarkResults []Result
)

// BenchmarkExactSearch measures the current implementation without embedding
// inference, disk I/O, or history traversal. Synthetic vectors are independent
// normalized Gaussian samples, not a model of real document neighborhoods.
// Setup is excluded; all query-time allocations and sorting are included.
func BenchmarkExactSearch(b *testing.B) {
	for _, dim := range []int{256, 512} {
		for _, n := range []int{100, 1000, 10000, 50000} {
			b.Run(fmt.Sprintf("dim=%d/n=%d", dim, n), func(b *testing.B) {
				idx, queries := benchmarkIndex(n, dim)
				b.Run("DenseScores", func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						benchmarkScores = DenseScores(idx, queries[i%len(queries)])
					}
				})
				b.Run("DenseRanking", func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						benchmarkRanking = RankByScore(DenseScores(idx, queries[i%len(queries)]))
					}
				})
				b.Run("HybridRank", func(b *testing.B) {
					p := DefaultPipeline()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						e := fixedEmbedder{vec: queries[i%len(queries)]}
						var err error
						benchmarkResults, err = Rank(p, e, idx, "shared", nil)
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}

func benchmarkIndex(n, dim int) (*Index, [][]float32) {
	rng := rand.New(rand.NewSource(42))
	vector := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		normalize(v)
		return v
	}
	idx := &Index{Dim: int32(dim), Terms: []string{"shared", "other"}}
	for i := 0; i < n; i++ {
		idx.Entries = append(idx.Entries, Entry{
			BlobID: fmt.Sprintf("%064x", i),
			Vector: vector(),
			Terms:  []TermFreq{{Term: int32(i % 2), Count: 1}},
		})
	}
	queries := make([][]float32, 32)
	for i := range queries {
		queries[i] = vector()
	}
	return idx, queries
}
