package search

import (
	"math"
	"testing"
)

func TestDenseScoresCosinePerEntry(t *testing.T) {
	idx := &Index{
		Entries: []Entry{
			{Vector: []float32{1, 0}},
			{Vector: []float32{0, 1}},
			{Vector: []float32{-1, 0}},
		},
	}
	got := DenseScores(idx, []float32{1, 0})
	want := []float32{1, 0, -1}
	if len(got) != len(want) {
		t.Fatalf("len(DenseScores) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("DenseScores[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDenseScoresEmptyIndex(t *testing.T) {
	idx := &Index{}
	if got := DenseScores(idx, []float32{1, 0}); len(got) != 0 {
		t.Errorf("DenseScores(empty index) = %v, want empty", got)
	}
}

func TestRankByScoreOrdersDescendingExcludesNonPositive(t *testing.T) {
	scores := []float32{0, 0.5, -0.2, 0.9, 0.5}
	got := RankByScore(scores)
	want := []int{3, 1, 4}
	if len(got) != len(want) {
		t.Fatalf("RankByScore(%v) = %v, want %v", scores, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RankByScore(%v)[%d] = %d, want %d", scores, i, got[i], want[i])
		}
	}
}

func TestRankByScoreEmptyWhenNothingPositive(t *testing.T) {
	if got := RankByScore([]float32{0, -1, 0}); got != nil {
		t.Errorf("RankByScore(no positive scores) = %v, want nil", got)
	}
}

func TestFuseRRFHandComputed(t *testing.T) {
	// k = 1 for arithmetic simple enough to check by hand.
	rankings := [][]int{
		{0, 1, 2},
		{2, 0, 1},
	}
	got := FuseRRF(3, 1, rankings...)
	want := []float32{
		1.0/2 + 1.0/3, // rank 1 then rank 2
		1.0/3 + 1.0/4, // rank 2 then rank 3
		1.0/4 + 1.0/2, // rank 3 then rank 1
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("FuseRRF[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseRRFEntryAbsentFromRankingContributesZero(t *testing.T) {
	got := FuseRRF(3, 60, []int{0})
	want := []float32{1.0 / 61, 0, 0}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("FuseRRF[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseRRFNoRankings(t *testing.T) {
	got := FuseRRF(2, 60)
	want := []float32{0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FuseRRF[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseAlphaMinMaxNormalizesAndWeights(t *testing.T) {
	dense := []float32{0, 0.4, 0.8, -0.1}
	lexical := []float32{2, 0, 6, 4}

	got := FuseAlpha(0.5, dense, lexical)
	want := []float32{0, 0, 1, 0.25}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("FuseAlpha[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseAlphaOneIsDenseOnly(t *testing.T) {
	dense := []float32{0.4, 0.8}
	lexical := []float32{9, 1}
	got := FuseAlpha(1, dense, lexical)
	want := []float32{0, 1} // dense min-max normalised, lexical weight zero
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("FuseAlpha(alpha=1)[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseAlphaZeroIsLexicalOnly(t *testing.T) {
	dense := []float32{0.4, 0.8}
	lexical := []float32{9, 1}
	got := FuseAlpha(0, dense, lexical)
	want := []float32{1, 0} // lexical min-max normalised, dense weight zero
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("FuseAlpha(alpha=0)[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFuseAlphaNoPositiveScoresIsAllZero(t *testing.T) {
	dense := []float32{0, -1, 0}
	lexical := []float32{0, 0, 0}
	got := FuseAlpha(0.5, dense, lexical)
	for i, v := range got {
		if v != 0 {
			t.Errorf("FuseAlpha[%d] = %v, want 0 (no positive scores to normalise)", i, v)
		}
	}
}

func TestFuseAlphaMismatchedLengthPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("FuseAlpha(len(dense) != len(lexical)) did not panic, want a named panic")
		}
	}()
	FuseAlpha(0.5, []float32{1, 2, 3}, []float32{1})
}

// TestMinMaxNormalizeNonFiniteEntryDoesNotPoisonOthers checks that a NaN
// entry, wherever it sits, normalizes to 0 and does not turn every other
// entry's result into NaN too (NaN comparisons are always false, so a naive
// s <= 0 skip does not exclude it from the min/max scan).
func TestMinMaxNormalizeNonFiniteEntryDoesNotPoisonOthers(t *testing.T) {
	nan := float32(math.NaN())
	tests := []struct {
		name   string
		scores []float32
		want   []float32
	}{
		{"NaN first", []float32{nan, 1, 2}, []float32{0, 0, 1}},
		{"NaN last", []float32{1, 2, nan}, []float32{0, 1, 0}},
		{"+Inf does not silently win the max", []float32{1, 2, float32(math.Inf(1))}, []float32{0, 1, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := minMaxNormalize(tt.scores)
			if len(got) != len(tt.want) {
				t.Fatalf("minMaxNormalize(%v) = %v, want %v", tt.scores, got, tt.want)
			}
			for i := range tt.want {
				if math.IsNaN(float64(got[i])) {
					t.Errorf("minMaxNormalize(%v)[%d] = NaN, want a finite value", tt.scores, i)
					continue
				}
				if math.Abs(float64(got[i]-tt.want[i])) > 1e-6 {
					t.Errorf("minMaxNormalize(%v)[%d] = %v, want %v", tt.scores, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestFuseAlphaNonFiniteEntryDoesNotPoisonTheFusedRanking is the
// end-to-end regression for the finding: a NaN dense score used to make
// FuseAlpha return every score as NaN, and Rank's `fused[i] <= 0` filter
// does not drop NaN (NaN <= 0 is false), so the corrupt entry survived
// into the ranking, sometimes as the top result.
func TestFuseAlphaNonFiniteEntryDoesNotPoisonTheFusedRanking(t *testing.T) {
	nan := float32(math.NaN())
	dense := []float32{nan, 1, 2}
	lexical := []float32{1, 1, 1}
	got := FuseAlpha(0.5, dense, lexical)
	for i, v := range got {
		if math.IsNaN(float64(v)) {
			t.Errorf("FuseAlpha[%d] = NaN, want every fused score finite even with one NaN input", i)
		}
	}
}
