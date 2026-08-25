package search

import (
	"errors"
	"math"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

func TestCosineIdenticalVectors(t *testing.T) {
	a := []float32{0.6, 0.8, 0}
	if got := cosine(a, a); math.Abs(float64(got-1)) > 1e-6 {
		t.Errorf("cosine(a, a) = %v, want 1", got)
	}
}

func TestCosineOppositeVectors(t *testing.T) {
	a := []float32{0.6, 0.8, 0}
	b := []float32{-0.6, -0.8, 0}
	if got := cosine(a, b); math.Abs(float64(got+1)) > 1e-6 {
		t.Errorf("cosine(a, -a) = %v, want -1", got)
	}
}

func TestCosineOrthogonalVectors(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := cosine(a, b); math.Abs(float64(got)) > 1e-6 {
		t.Errorf("cosine(orthogonal) = %v, want 0", got)
	}
}

func TestCosineZeroVector(t *testing.T) {
	if got := cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Errorf("cosine(zero, a) = %v, want 0", got)
	}
}

// TestCosineNonFiniteInputYieldsZeroNotNaN defends the score path even
// though decodeEntry already rejects a non-finite vector component at
// decode time: cosine(+Inf, +Inf) computes Inf/Inf = NaN despite neither
// norm being zero, which would otherwise poison minMaxNormalize downstream.
func TestCosineNonFiniteInputYieldsZeroNotNaN(t *testing.T) {
	inf := float32(math.Inf(1))
	if got := cosine([]float32{inf, 0}, []float32{inf, 0}); got != 0 {
		t.Errorf("cosine(+Inf, +Inf) = %v, want 0 rather than NaN", got)
	}
}

// fixedEmbedder is an Embedder test double that returns vec for every
// query, ignoring the query text, so Rank tests can pin down the dense
// score of each entry exactly.
type fixedEmbedder struct {
	vec []float32
	err error
}

func (e fixedEmbedder) ID() string { return "fixed-test-embedder" }
func (e fixedEmbedder) Dim() int   { return len(e.vec) }
func (e fixedEmbedder) Embed(string) ([]float32, error) {
	return e.vec, e.err
}

// TestRankRejectsInvalidPipeline checks that Rank validates p itself rather
// than trusting the caller: pipeline.go is the one file the autoretrieval
// loop is licensed to edit, so a bad edit there (or a zero-value Pipeline)
// must fail loudly here instead of silently producing garbage, such as the
// +Inf FuseRRF divides by zero when RRFK is non-positive.
func TestRankRejectsInvalidPipeline(t *testing.T) {
	idx := &Index{Entries: []Entry{{BlobID: "a", Vector: []float32{1, 0}}}}
	p := DefaultPipeline()
	p.RRFK = -1
	_, err := Rank(p, fixedEmbedder{vec: []float32{1, 0}}, idx, "q", nil)
	if err == nil {
		t.Error("Rank with an invalid pipeline (RRFK = -1) succeeded, want an error")
	}
}

func TestRankWrapsEmbedderError(t *testing.T) {
	wantErr := errors.New("boom")
	idx := &Index{Entries: []Entry{{BlobID: "a", Vector: []float32{1, 0}}}}
	_, err := Rank(DefaultPipeline(), fixedEmbedder{err: wantErr}, idx, "q", nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("Rank error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestRankFusedOrderCombinesDenseAndLexical hand-builds an index where the
// dense ranking (by cosine) and the lexical ranking (by BM25) disagree:
// dense favors x over y, lexical favors y over z. Reciprocal rank fusion
// (the default pipeline's "rrf") must rank y first because it is the only
// entry present in both rankings, ahead of x and z, which each appear in
// only one.
func TestRankFusedOrderCombinesDenseAndLexical(t *testing.T) {
	idx := &Index{
		Terms: []string{"shared", "filler1", "filler2", "filler3"},
		Entries: []Entry{
			{BlobID: "x", Vector: []float32{1, 0}}, // no "shared": lexical score 0
			{
				BlobID: "y",
				Vector: []float32{0.5, 0.866}, // cos(query) = 0.5
				Terms:  []TermFreq{{Term: 0, Count: 1}},
			},
			{
				BlobID: "z",
				Vector: []float32{0, 1}, // cos(query) = 0: excluded from dense ranking
				Terms: []TermFreq{
					{Term: 0, Count: 1}, {Term: 1, Count: 1}, {Term: 2, Count: 1}, {Term: 3, Count: 1},
				},
			},
		},
	}

	p := DefaultPipeline() // rrf, k=60, k1=1.2, b=0.75
	results, err := Rank(p, fixedEmbedder{vec: []float32{1, 0}}, idx, "shared", nil)
	if err != nil {
		t.Fatalf("Rank = %v", err)
	}

	wantOrder := []string{"y", "x", "z"}
	if len(results) != len(wantOrder) {
		t.Fatalf("Rank returned %d results, want %d: %+v", len(results), len(wantOrder), results)
	}
	for i, want := range wantOrder {
		if results[i].BlobID != want {
			t.Errorf("results[%d].BlobID = %q, want %q (order %v)", i, results[i].BlobID, want, results)
		}
	}

	// y appears in both rankings (dense rank 2, lexical rank 1) so its
	// fused score must exceed x and z, which each appear in only one.
	if !(results[0].Score > results[1].Score && results[1].Score > results[2].Score) {
		t.Errorf("fused scores not strictly descending: %v, %v, %v", results[0].Score, results[1].Score, results[2].Score)
	}
}

// TestRankTieBreaksByBlobIDThenSequence gives three entries an identical
// positive cosine similarity to the query, so alpha fusion's min-max
// normalisation (min == max) assigns every one of them the same fused
// score of 1. The resulting three-way tie must break by ascending BlobID,
// then by ascending Sequence within a repeated BlobID.
func TestRankTieBreaksByBlobIDThenSequence(t *testing.T) {
	idx := &Index{
		Entries: []Entry{
			{BlobID: "bravo", Sequence: 1, Vector: []float32{1, 0}},
			{BlobID: "alpha", Sequence: 5, Vector: []float32{1, 0}},
			{BlobID: "bravo", Sequence: 0, Vector: []float32{1, 0}},
		},
	}

	p := DefaultPipeline()
	p.Fusion = "alpha"
	p.Alpha = 1 // dense only; lexical (all zero here) carries no weight

	results, err := Rank(p, fixedEmbedder{vec: []float32{1, 0}}, idx, "q", nil)
	if err != nil {
		t.Fatalf("Rank = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Rank returned %d results, want 3 (a three-way tie)", len(results))
	}

	type key struct {
		blobID   string
		sequence int32
	}
	want := []key{{"alpha", 5}, {"bravo", 0}, {"bravo", 1}}
	for i, k := range want {
		if results[i].BlobID != k.blobID || results[i].Sequence != k.sequence {
			t.Errorf("results[%d] = {%s, %d}, want {%s, %d}",
				i, results[i].BlobID, results[i].Sequence, k.blobID, k.sequence)
		}
	}
}

// TestRankTieBreakSequenceOverflowSafe checks that the Sequence tie-break
// uses a comparison, not a subtraction: Sequence is a raw int32 read
// straight off disk with no upper bound, and MaxInt32 - (-1) overflows
// int32 to a negative number, which used to report "less" in both
// directions and make the sort order depend on the entries' order in the
// input file rather than being deterministic as the spec requires.
func TestRankTieBreakSequenceOverflowSafe(t *testing.T) {
	build := func(first, second int32) *Index {
		return &Index{
			Entries: []Entry{
				{BlobID: "same", Sequence: first, Vector: []float32{1, 0}},
				{BlobID: "same", Sequence: second, Vector: []float32{1, 0}},
			},
		}
	}

	p := DefaultPipeline()
	p.Fusion = "alpha"
	p.Alpha = 1
	embedder := fixedEmbedder{vec: []float32{1, 0}}

	for _, order := range []struct {
		name          string
		first, second int32
	}{
		{"MaxInt32 then -1", math.MaxInt32, -1},
		{"-1 then MaxInt32", -1, math.MaxInt32},
	} {
		t.Run(order.name, func(t *testing.T) {
			idx := build(order.first, order.second)
			results, err := Rank(p, embedder, idx, "q", nil)
			if err != nil {
				t.Fatalf("Rank = %v", err)
			}
			if len(results) != 2 {
				t.Fatalf("Rank returned %d results, want 2", len(results))
			}
			if results[0].Sequence != -1 || results[1].Sequence != math.MaxInt32 {
				t.Errorf("results = [%d, %d], want [-1, %d] regardless of input order",
					results[0].Sequence, results[1].Sequence, int32(math.MaxInt32))
			}
		})
	}
}

// TestRankAllowFilterDropsBeforeRanking checks that allow excludes an
// entry from the ranking step itself, not merely from the returned list:
// with the top-ranked entry excluded, the runner-up must move into its
// vacated rank-1 slot and score accordingly higher, not keep the rank-2
// score it had when the top entry was still present.
func TestRankAllowFilterDropsBeforeRanking(t *testing.T) {
	idx := &Index{
		Entries: []Entry{
			{BlobID: "a", Vector: []float32{1, 0}},       // cos(query) = 1: rank 1
			{BlobID: "b", Vector: []float32{0.5, 0.866}}, // cos(query) = 0.5: rank 2
		},
	}
	p := DefaultPipeline()
	embedder := fixedEmbedder{vec: []float32{1, 0}}

	unfiltered, err := Rank(p, embedder, idx, "q", nil)
	if err != nil {
		t.Fatalf("Rank(no filter) = %v", err)
	}
	if len(unfiltered) != 2 || unfiltered[0].BlobID != "a" || unfiltered[1].BlobID != "b" {
		t.Fatalf("Rank(no filter) = %+v, want [a, b]", unfiltered)
	}

	allow := func(blobID string) bool { return blobID != "a" }
	filtered, err := Rank(p, embedder, idx, "q", allow)
	if err != nil {
		t.Fatalf("Rank(allow) = %v", err)
	}
	if len(filtered) != 1 || filtered[0].BlobID != "b" {
		t.Fatalf("Rank(allow excludes a) = %+v, want only b", filtered)
	}

	if filtered[0].Score <= unfiltered[1].Score {
		t.Errorf("b's filtered score (%v) is not higher than its unfiltered rank-2 score (%v); "+
			"allow must drop entries before ranking so b takes rank 1",
			filtered[0].Score, unfiltered[1].Score)
	}
}

func TestGroupByBlobKeepsFirstPerBlobPreservingOrder(t *testing.T) {
	results := []Result{
		{BlobID: "a", Sequence: 0, Score: 0.9},
		{BlobID: "b", Sequence: 0, Score: 0.8},
		{BlobID: "a", Sequence: 1, Score: 0.5}, // duplicate blob: must be dropped
		{BlobID: "c", Sequence: 0, Score: 0.3},
	}
	got := GroupByBlob(results)
	want := []Result{results[0], results[1], results[3]}
	if len(got) != len(want) {
		t.Fatalf("GroupByBlob returned %d results, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GroupByBlob()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestGroupByBlobEmpty(t *testing.T) {
	if got := GroupByBlob(nil); len(got) != 0 {
		t.Errorf("GroupByBlob(nil) = %v, want empty", got)
	}
}

// TestRankPaymentSystemsQueryFirst is the end-to-end ranking test: two PDF
// fixtures on unrelated topics are extracted, chunked, and indexed with
// the builtin lexical embedder, and a "payment systems" query must rank
// the payment fixture's chunk above the weather fixture's.
func TestRankPaymentSystemsQueryFirst(t *testing.T) {
	embedder := LexicalEmbedder{}
	p := DefaultPipeline()
	idx := NewIndex(embedder.ID(), embedder.Dim(), p.IndexHash())

	build := func(name string) string {
		t.Helper()
		data := readTestdata(t, name)
		text, ok := Extract(data)
		if !ok {
			t.Fatalf("Extract(%s) = false, want true", name)
		}
		blobID := object.ID(object.TypeBlob, data)
		for _, c := range ChunkText(text, p) {
			vec, err := embedder.Embed(c.Text)
			if err != nil {
				t.Fatalf("Embed = %v", err)
			}
			idx.Add(blobID, c, vec, Terms(c.Text, p.Stopwords))
		}
		return blobID
	}

	paymentBlobID := build("payment-uncompressed.pdf")
	build("weather-flate.pdf")

	results, err := Rank(p, embedder, idx, "payment systems", nil)
	if err != nil {
		t.Fatalf("Rank = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Rank returned no results")
	}
	if results[0].BlobID != paymentBlobID {
		t.Errorf("top result blob = %s, want the payment fixture's blob %s", results[0].BlobID, paymentBlobID)
	}
}

func TestRankTieBreakUsesEntryBlobID(t *testing.T) {
	// Sanity check that Result carries the fields Rank documents, keeping
	// this test resistant to a future field being silently dropped.
	entries := []Entry{
		{BlobID: object.ID(object.TypeBlob, []byte("z")), Vector: []float32{1, 0}},
		{BlobID: object.ID(object.TypeBlob, []byte("a")), Vector: []float32{1, 0}},
	}
	idx := &Index{Entries: entries}
	p := DefaultPipeline()
	p.Fusion = "alpha"
	p.Alpha = 1

	results, err := Rank(p, fixedEmbedder{vec: []float32{1, 0}}, idx, "q", nil)
	if err != nil {
		t.Fatalf("Rank = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Rank returned %d results, want 2", len(results))
	}
	if results[0].BlobID >= results[1].BlobID {
		t.Errorf("tied scores not broken by ascending blob id: %s then %s", results[0].BlobID, results[1].BlobID)
	}
}
