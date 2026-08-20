package search

import (
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

func TestTopKEmptyEntries(t *testing.T) {
	if got := TopK([]float32{1, 0}, nil, 5); got != nil {
		t.Errorf("TopK(nil entries) = %v, want nil", got)
	}
}

func TestTopKZeroOrNegativeLimit(t *testing.T) {
	entries := []Entry{{BlobID: object.ID(object.TypeBlob, []byte("a")), Vector: []float32{1, 0}}}
	if got := TopK([]float32{1, 0}, entries, 0); got != nil {
		t.Errorf("TopK(k=0) = %v, want nil", got)
	}
}

func TestTopKBestChunkPerBlob(t *testing.T) {
	blob := object.ID(object.TypeBlob, []byte("multi-chunk blob"))
	entries := []Entry{
		{BlobID: blob, Sequence: 0, Snippet: "weak match", Vector: []float32{0.1, 0.995}},
		{BlobID: blob, Sequence: 1, Snippet: "strong match", Vector: []float32{1, 0}},
	}
	results := TopK([]float32{1, 0}, entries, 5)
	if len(results) != 1 {
		t.Fatalf("TopK returned %d results, want 1 (one blob, deduplicated)", len(results))
	}
	if results[0].Snippet != "strong match" {
		t.Errorf("TopK kept snippet %q, want the best-scoring chunk's snippet %q",
			results[0].Snippet, "strong match")
	}
}

func TestTopKOrdersByScoreDescending(t *testing.T) {
	entries := []Entry{
		{BlobID: object.ID(object.TypeBlob, []byte("low")), Vector: []float32{0, 1}},
		{BlobID: object.ID(object.TypeBlob, []byte("high")), Vector: []float32{1, 0}},
		{BlobID: object.ID(object.TypeBlob, []byte("mid")), Vector: []float32{0.7, 0.7}},
	}
	results := TopK([]float32{1, 0}, entries, 3)
	if len(results) != 3 {
		t.Fatalf("TopK returned %d results, want 3", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Errorf("results not sorted by descending score: %v then %v", results[i-1].Score, results[i].Score)
		}
	}
	if results[0].BlobID != object.ID(object.TypeBlob, []byte("high")) {
		t.Errorf("top result blob = %s, want the exact-match blob", results[0].BlobID)
	}
}

func TestTopKLimitsResults(t *testing.T) {
	entries := []Entry{
		{BlobID: object.ID(object.TypeBlob, []byte("1")), Vector: []float32{1, 0}},
		{BlobID: object.ID(object.TypeBlob, []byte("2")), Vector: []float32{0.9, 0.1}},
		{BlobID: object.ID(object.TypeBlob, []byte("3")), Vector: []float32{0.1, 0.9}},
	}
	results := TopK([]float32{1, 0}, entries, 2)
	if len(results) != 2 {
		t.Fatalf("TopK(k=2) returned %d results, want 2", len(results))
	}
}

func TestTopKTieBreaksByBlobID(t *testing.T) {
	entries := []Entry{
		{BlobID: object.ID(object.TypeBlob, []byte("z")), Vector: []float32{1, 0}},
		{BlobID: object.ID(object.TypeBlob, []byte("a")), Vector: []float32{1, 0}},
	}
	results := TopK([]float32{1, 0}, entries, 2)
	if len(results) != 2 {
		t.Fatalf("TopK returned %d results, want 2", len(results))
	}
	if results[0].BlobID >= results[1].BlobID {
		t.Errorf("tied scores not broken by ascending blob id: %s then %s", results[0].BlobID, results[1].BlobID)
	}
}

// TestSearchRanksPaymentSystemsQueryFirst is the end-to-end ranking test:
// two PDF fixtures on unrelated topics are extracted, chunked, and embedded
// with the builtin lexical embedder, and a "payment systems" query must
// rank the payment fixture's chunk above the weather fixture's.
func TestSearchRanksPaymentSystemsQueryFirst(t *testing.T) {
	embedder := LexicalEmbedder{}

	build := func(name string) (string, []Entry) {
		data := readTestdata(t, name)
		text, ok := Extract(data)
		if !ok {
			t.Fatalf("Extract(%s) = false, want true", name)
		}
		blobID := object.ID(object.TypeBlob, data)
		var entries []Entry
		for _, c := range ChunkText(text) {
			vec, err := embedder.Embed(c.Text)
			if err != nil {
				t.Fatalf("Embed = %v", err)
			}
			entries = append(entries, Entry{
				BlobID:   blobID,
				Sequence: int32(c.Sequence),
				Snippet:  c.Snippet,
				Vector:   vec,
			})
		}
		return blobID, entries
	}

	paymentBlobID, paymentEntries := build("payment-uncompressed.pdf")
	_, weatherEntries := build("weather-flate.pdf")

	var all []Entry
	all = append(all, paymentEntries...)
	all = append(all, weatherEntries...)

	results, err := Search(embedder, Index{EmbedderID: embedder.ID(), Dim: int32(embedder.Dim()), Entries: all},
		"payment systems", 5)
	if err != nil {
		t.Fatalf("Search = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}
	if results[0].BlobID != paymentBlobID {
		t.Errorf("top result blob = %s, want the payment fixture's blob %s", results[0].BlobID, paymentBlobID)
	}
}
