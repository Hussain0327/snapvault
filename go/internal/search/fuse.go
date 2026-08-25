package search

import (
	"fmt"
	"math"
	"slices"
)

// DenseScores returns cosine(query, entry.Vector) per idx.Entries, in entry
// order.
func DenseScores(idx *Index, query []float32) []float32 {
	scores := make([]float32, len(idx.Entries))
	for i, e := range idx.Entries {
		scores[i] = cosine(query, e.Vector)
	}
	return scores
}

// RankByScore returns the indices of scores greater than 0, in descending
// score order, ties broken by ascending index. It is how a dense or lexical
// score slice becomes a ranking FuseRRF can consume.
func RankByScore(scores []float32) []int {
	var ranked []int
	for i, s := range scores {
		if s > 0 {
			ranked = append(ranked, i)
		}
	}
	slices.SortFunc(ranked, func(a, b int) int {
		if scores[a] != scores[b] {
			if scores[a] > scores[b] {
				return -1
			}
			return 1
		}
		return a - b
	})
	return ranked
}

// FuseRRF returns one fused score per entry (0 to n-1): the sum, over every
// ranking, of 1/(k+rank) where rank is counted from 1 for that ranking's
// first entry. An entry absent from a ranking contributes 0 for it, so an
// entry that appears in no ranking scores 0 overall.
func FuseRRF(n, k int, rankings ...[]int) []float32 {
	scores := make([]float32, n)
	for _, ranking := range rankings {
		for pos, entry := range ranking {
			rank := pos + 1
			scores[entry] += float32(1) / float32(k+rank)
		}
	}
	return scores
}

// FuseAlpha independently min-max normalises dense and lexical over the
// entries each scores greater than 0 — entries at or below 0 normalise to
// 0 and are excluded from the min and max — and returns
// alpha*dense + (1-alpha)*lexical per entry. dense and lexical must be the
// same length; the result has that length.
func FuseAlpha(alpha float64, dense, lexical []float32) []float32 {
	if len(dense) != len(lexical) {
		panic(fmt.Sprintf("FuseAlpha: dense has %d entries, lexical has %d, they must match", len(dense), len(lexical)))
	}
	d := minMaxNormalize(dense)
	l := minMaxNormalize(lexical)
	fused := make([]float32, len(d))
	for i := range fused {
		fused[i] = float32(alpha)*d[i] + float32(1-alpha)*l[i]
	}
	return fused
}

// minMaxNormalize rescales the entries of scores that are greater than 0
// into [0, 1] by their min and max; every other entry becomes 0. When every
// positive entry is equal, it becomes 1 rather than dividing by a zero
// range. A NaN or infinite entry is treated like a non-positive one: it is
// excluded from the min and max and normalizes to 0, rather than either
// poisoning every other entry's result (NaN propagates through any
// arithmetic it touches) or, for +Inf, silently winning the max and so the
// top normalized score.
func minMaxNormalize(scores []float32) []float32 {
	normalized := make([]float32, len(scores))

	min, max := float32(0), float32(0)
	found := false
	for _, s := range scores {
		if s <= 0 || !isFinite(s) {
			continue
		}
		if !found || s < min {
			min = s
		}
		if !found || s > max {
			max = s
		}
		found = true
	}
	if !found {
		return normalized
	}

	for i, s := range scores {
		if s <= 0 || !isFinite(s) {
			continue
		}
		if max == min {
			normalized[i] = 1
			continue
		}
		normalized[i] = (s - min) / (max - min)
	}
	return normalized
}

// isFinite reports whether s is neither NaN nor an infinity.
func isFinite(s float32) bool {
	return !math.IsNaN(float64(s)) && !math.IsInf(float64(s), 0)
}
