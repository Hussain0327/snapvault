package search

import (
	"math"
	"testing"
)

// threeChunkBM25Index builds a toy 3-entry index over a 3-term vocabulary
// ["a", "b", "c"]: doc0 has a:2 b:1 (length 3), doc1 has b:1 c:1 (length
// 2), doc2 has a:1 c:2 (length 3). Every term has document frequency 2.
func threeChunkBM25Index(t *testing.T) *Index {
	t.Helper()
	return &Index{
		EmbedderID: "builtin-lexical-v1",
		Dim:        1,
		Terms:      []string{"a", "b", "c"},
		Entries: []Entry{
			{Terms: []TermFreq{{Term: 0, Count: 2}, {Term: 1, Count: 1}}},
			{Terms: []TermFreq{{Term: 1, Count: 1}, {Term: 2, Count: 1}}},
			{Terms: []TermFreq{{Term: 0, Count: 1}, {Term: 2, Count: 2}}},
		},
	}
}

// TestBM25ScoresHandComputedThreeChunkIndex checks Scores against values
// independently computed from the spec's formula (see the Python snippet
// in the design doc's worked derivation): avgLen = 8/3, idf(a)=idf(b)=
// idf(c)=ln(1.6) since every term has document frequency 2 of 3.
func TestBM25ScoresHandComputedThreeChunkIndex(t *testing.T) {
	idx := threeChunkBM25Index(t)
	m := NewBM25(idx, 1.2, 0.75)

	tests := []struct {
		name  string
		query []string
		want  []float64
	}{
		{"query a", []string{"a"}, []float64{0.6243067075264112, 0, 0.44713858782297017}},
		{"query b", []string{"b"}, []float64{0.44713858782297017, 0.523548346501579, 0}},
		{"query a and c", []string{"a", "c"}, []float64{0.6243067075264112, 0.523548346501579, 1.0714452953493814}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Scores(tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("Scores(%v) returned %d scores, want %d", tt.query, len(got), len(tt.want))
			}
			for i := range tt.want {
				if diff := math.Abs(float64(got[i]) - tt.want[i]); diff > 1e-6 {
					t.Errorf("Scores(%v)[%d] = %v, want %v (diff %v)", tt.query, i, got[i], tt.want[i], diff)
				}
			}
		})
	}
}

func TestBM25ScoresNoSharedTermIsZero(t *testing.T) {
	idx := threeChunkBM25Index(t)
	m := NewBM25(idx, 1.2, 0.75)
	got := m.Scores([]string{"nonexistent"})
	for i, s := range got {
		if s != 0 {
			t.Errorf("Scores[%d] = %v, want 0 for a query term absent from the vocabulary", i, s)
		}
	}
}

func TestBM25ScoresEmptyIndex(t *testing.T) {
	idx := &Index{EmbedderID: "builtin-lexical-v1", Dim: 1}
	m := NewBM25(idx, 1.2, 0.75)
	if got := m.Scores([]string{"a"}); len(got) != 0 {
		t.Errorf("Scores(...) on an empty index = %v, want empty", got)
	}
}

// TestNewBM25SaturatingCountsDoNotOverflowDocLen checks that accumulating a
// chunk's term counts cannot wrap a document's length negative: decodeEntry
// only requires Count >= 1, so a crafted (or, before this fix, even a
// legitimately huge) index can carry counts that overflow int32 once
// summed. A negative docLen used to invert BM25's length normalisation and
// could drop a genuinely matching entry's score below the RankByScore
// filter's s > 0 threshold entirely.
func TestNewBM25SaturatingCountsDoNotOverflowDocLen(t *testing.T) {
	// Two distinct terms, not a repeated one, so this test isolates the
	// int32 sum overflow from the separate duplicate-term-id df inflation
	// bug: decodeEntry and Write both reject a duplicate term id today, so
	// exercising that path here would conflate the two.
	idx := &Index{
		Terms: []string{"cat", "dog"},
		Entries: []Entry{
			{Terms: []TermFreq{{Term: 0, Count: math.MaxInt32}, {Term: 1, Count: 2}}},
		},
	}
	m := NewBM25(idx, 1.2, 0.75)
	if m.docLen[0] <= 0 {
		t.Errorf("docLen[0] = %d, want a positive int64 sum, not an overflowed negative int32", m.docLen[0])
	}
	if m.avgLen <= 0 {
		t.Errorf("avgLen = %v, want positive", m.avgLen)
	}
	got := m.Scores([]string{"cat"})
	if got[0] < 0 {
		t.Errorf("Scores[0] = %v, want a non-negative score for an entry that genuinely contains the query term", got[0])
	}
}

// TestNewBM25IgnoresOutOfRangeTermID checks that NewBM25 does not panic
// when handed an Index built programmatically (as the eval and repo
// packages do) whose entry references a term id past the vocabulary:
// Read and Write both reject this on disk, but NewBM25 takes any *Index.
func TestNewBM25IgnoresOutOfRangeTermID(t *testing.T) {
	idx := &Index{
		Terms: []string{"cat"},
		Entries: []Entry{
			{Terms: []TermFreq{{Term: 5, Count: 1}}}, // 5 is out of range for a 1-term vocabulary
		},
	}
	m := NewBM25(idx, 1.2, 0.75)
	if got := m.Scores([]string{"cat"}); len(got) != 1 {
		t.Errorf("Scores returned %d scores, want 1", len(got))
	}
}

func TestBM25ScoresRepeatedQueryTermNotDoubleCounted(t *testing.T) {
	idx := threeChunkBM25Index(t)
	m := NewBM25(idx, 1.2, 0.75)
	once := m.Scores([]string{"a"})
	repeated := m.Scores([]string{"a", "a", "a"})
	for i := range once {
		if once[i] != repeated[i] {
			t.Errorf("Scores[%d] = %v for [a] but %v for [a,a,a], want the same (idf summed once per distinct term)",
				i, once[i], repeated[i])
		}
	}
}
