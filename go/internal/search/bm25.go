package search

import "math"

// BM25 scores idx.Entries against a query's terms, using the interned term
// vocabulary and per-chunk lengths recorded when idx was built.
type BM25 struct {
	idx    *Index
	k1, b  float64
	termID map[string]int32 // idx.Terms string -> its id
	df     []int32          // per term id: number of entries containing it
	docLen []int64          // per entry: total term count (sum of Count)
	avgLen float64
}

// NewBM25 indexes idx for repeated Scores calls at the given BM25
// parameters.
func NewBM25(idx *Index, k1, b float64) *BM25 {
	m := &BM25{
		idx:    idx,
		k1:     k1,
		b:      b,
		termID: make(map[string]int32, len(idx.Terms)),
		df:     make([]int32, len(idx.Terms)),
		docLen: make([]int64, len(idx.Entries)),
	}
	for i, term := range idx.Terms {
		m.termID[term] = int32(i)
	}

	var totalLen int64
	for i, e := range idx.Entries {
		var length int64
		for _, tf := range e.Terms {
			// Read and Write both reject a term id outside [0, len(Terms))
			// today, but NewBM25 also takes an Index the eval and repo
			// packages can build programmatically, so it guards its own
			// index into df rather than trusting the caller.
			if tf.Term < 0 || int(tf.Term) >= len(m.df) {
				continue
			}
			length += int64(tf.Count)
			m.df[tf.Term]++
		}
		m.docLen[i] = length
		totalLen += length
	}
	if len(idx.Entries) > 0 {
		m.avgLen = float64(totalLen) / float64(len(idx.Entries))
	}
	return m
}

// Scores returns one BM25 score per idx.Entries for queryTerms; chunks
// sharing no term with queryTerms score 0. Standard Okapi BM25:
//
//	idf = ln(1 + (N - df + 0.5) / (df + 0.5))
//	score = sum idf * tf*(k1+1) / (tf + k1*(1 - b + b*len/avglen))
//
// where len is a chunk's total term count and the sum ranges over the
// distinct terms queryTerms shares with idx.Terms.
func (m *BM25) Scores(queryTerms []string) []float32 {
	scores := make([]float32, len(m.idx.Entries))
	if len(m.idx.Entries) == 0 {
		return scores
	}

	n := float64(len(m.idx.Entries))
	idf := make(map[int32]float64)
	for _, qt := range queryTerms {
		id, ok := m.termID[qt]
		if !ok {
			continue
		}
		if _, done := idf[id]; done {
			continue
		}
		df := float64(m.df[id])
		idf[id] = math.Log(1 + (n-df+0.5)/(df+0.5))
	}
	if len(idf) == 0 {
		return scores
	}

	for i, e := range m.idx.Entries {
		length := m.docLen[i]
		if length == 0 {
			continue
		}
		var score float64
		for _, tf := range e.Terms {
			weight, ok := idf[tf.Term]
			if !ok {
				continue
			}
			t := float64(tf.Count)
			denom := t + m.k1*(1-m.b+m.b*float64(length)/m.avgLen)
			score += weight * (t * (m.k1 + 1)) / denom
		}
		scores[i] = float32(score)
	}
	return scores
}
