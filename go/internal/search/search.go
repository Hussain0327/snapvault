// Package search implements SnapVault's local search sidecar: extracting
// plain text from blob content, splitting it into overlapping chunks, and
// ranking those chunks against a query by fusing two independent signals —
// a dense embedding's cosine similarity and a BM25 lexical score — into
// one hybrid ranking. It never touches objects/ — the index lives at
// .snapvault/index/embeddings.svi in the SVX2 binary format and is read
// and written entirely through this package. This package operates purely
// on byte slices, io.Readers, and the Index and Entry values defined
// here; walking a repository's commits and objects to produce blobs
// belongs to the caller.
package search

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Result is one ranked search match: a single chunk together with its
// fused score.
type Result struct {
	BlobID    string
	Sequence  int32
	StartRune int32
	EndRune   int32
	Snippet   string
	Score     float32
}

// Rank embeds query with embedder, scores every entry of idx both densely
// (cosine similarity against the query embedding, via DenseScores) and
// lexically (BM25 against Terms(query, p.Stopwords), via BM25.Scores),
// fuses the two per p.Fusion ("rrf" or "alpha"), and returns every entry
// whose fused score is greater than 0 in descending score order. Ties
// break by ascending BlobID, then by ascending Sequence, for a
// deterministic order.
//
// When allow is non-nil, entries whose blob id it rejects are dropped
// before the dense and lexical rankings are formed, not merely filtered
// out of the result afterward: find uses this to hide blobs history has
// made unreachable, and a dropped entry never occupies a rank slot that
// would otherwise go to one that remains.
func Rank(p Pipeline, embedder Embedder, idx *Index, query string, allow func(blobID string) bool) ([]Result, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pipeline: %w", err)
	}

	queryVec, err := embedder.Embed(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	dense := DenseScores(idx, queryVec)
	lexical := NewBM25(idx, p.K1, p.B).Scores(Terms(query, p.Stopwords))
	if allow != nil {
		for i, e := range idx.Entries {
			if !allow(e.BlobID) {
				dense[i] = 0
				lexical[i] = 0
			}
		}
	}

	var fused []float32
	if p.Fusion == "alpha" {
		fused = FuseAlpha(p.Alpha, dense, lexical)
	} else {
		fused = FuseRRF(len(idx.Entries), p.RRFK, RankByScore(dense), RankByScore(lexical))
	}

	var results []Result
	for i, e := range idx.Entries {
		if fused[i] <= 0 {
			continue
		}
		results = append(results, Result{
			BlobID:    e.BlobID,
			Sequence:  e.Sequence,
			StartRune: e.StartRune,
			EndRune:   e.EndRune,
			Snippet:   e.Snippet,
			Score:     fused[i],
		})
	}
	slices.SortFunc(results, func(a, b Result) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.BlobID, b.BlobID); c != 0 {
			return c
		}
		// cmp.Compare, not a - b: Sequence is a raw int32 read straight off
		// disk with no upper bound, and the subtraction can overflow int32
		// (e.g. MaxInt32 vs -1), which breaks strict-weak-ordering and makes
		// slices.SortFunc's output undefined rather than merely tied wrong.
		return cmp.Compare(a.Sequence, b.Sequence)
	})
	return results, nil
}

// GroupByBlob keeps the first Result per blob id and drops the rest,
// preserving the order results arrived in. Given results in descending
// score order (as Rank returns them), the kept result is each blob's
// best-scoring chunk.
func GroupByBlob(results []Result) []Result {
	seen := make(map[string]bool, len(results))
	grouped := make([]Result, 0, len(results))
	for _, r := range results {
		if seen[r.BlobID] {
			continue
		}
		seen[r.BlobID] = true
		grouped = append(grouped, r)
	}
	return grouped
}

// cosine returns the cosine similarity of a and b. Vectors of differing
// length are compared over their shared prefix; either vector being all
// zero yields 0 rather than a division by zero. decodeEntry already rejects
// a non-finite vector component at decode time, but cosine guards its own
// result too: a NaN or infinite score would otherwise poison every fused
// score downstream in minMaxNormalize.
func cosine(a, b []float32) float32 {
	var dot, normA, normB float64
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	result := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0
	}
	return float32(result)
}
