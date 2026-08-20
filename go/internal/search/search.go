// Package search implements SnapVault's local search sidecar: extracting
// plain text from blob content, splitting it into overlapping chunks,
// embedding those chunks into vectors, and ranking them by cosine
// similarity. It never touches objects/ — the index lives at
// .snapvault/index/embeddings.svi and is read and written entirely through
// this package. This package operates purely on byte slices, io.Readers,
// and the Index and Entry values defined here; walking a repository's
// commits and objects to produce blobs belongs to the caller.
package search

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Result is one ranked search match: the best-scoring chunk of one blob.
type Result struct {
	BlobID  string
	Snippet string
	Score   float32
}

// Search embeds query with embedder and returns its top-k matches over
// idx.Entries. It errors only if embedding the query itself fails; a
// mismatch between embedder and idx is the caller's concern (find rebuilds
// or refuses on a mismatched --embedder, per the index file's recorded
// embedder id).
func Search(embedder Embedder, idx Index, query string, k int) ([]Result, error) {
	vec, err := embedder.Embed(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}
	return TopK(vec, idx.Entries, k), nil
}

// TopK returns up to k entries most similar to query by cosine similarity,
// reduced to one Result per blob id — its single best-scoring chunk —
// ordered by descending score. Equal scores break ties by ascending blob id
// for a deterministic order.
func TopK(query []float32, entries []Entry, k int) []Result {
	if k <= 0 || len(entries) == 0 {
		return nil
	}

	best := make(map[string]Result, len(entries))
	for _, e := range entries {
		score := cosine(query, e.Vector)
		if current, ok := best[e.BlobID]; !ok || score > current.Score {
			best[e.BlobID] = Result{BlobID: e.BlobID, Snippet: e.Snippet, Score: score}
		}
	}

	results := make([]Result, 0, len(best))
	for _, r := range best {
		results = append(results, r)
	}
	slices.SortFunc(results, func(a, b Result) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.BlobID, b.BlobID)
	})

	if len(results) > k {
		results = results[:k]
	}
	return results
}

// cosine returns the cosine similarity of a and b. Vectors of differing
// length are compared over their shared prefix; either vector being all
// zero yields 0 rather than a division by zero.
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
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
