package model2vec

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadDim(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())
	if got, want := m.Dim(), testDim; got != want {
		t.Errorf("Dim() = %d, want %d", got, want)
	}
}

func TestLoadDigestMatchesSafetensorsSHA256(t *testing.T) {
	dir := t.TempDir()
	writeTestModelDir(t, dir, defaultTestModelConfig())

	raw, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatalf("reading model.safetensors: %v", err)
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s) = %v", dir, err)
	}
	if got := m.Digest(); got != want {
		t.Errorf("Digest() = %s, want %s", got, want)
	}
}

func TestLoadHiddenDimMismatch(t *testing.T) {
	dir := t.TempDir()
	writeTestModelDir(t, dir, defaultTestModelConfig())

	// Corrupt config.json's hidden_dim so it disagrees with the tensor's
	// actual second dimension (testDim).
	writeJSONFile(t, filepath.Join(dir, "config.json"), map[string]any{
		"normalize":  true,
		"hidden_dim": testDim + 1,
	})

	if _, err := Load(dir); err == nil {
		t.Error("Load with mismatched hidden_dim succeeded, want an error")
	}
}

// TestLoadRejectsVocabIDPastEmbeddingsRowCount checks that a tokenizer.json
// vocab entry whose id has no matching row in model.safetensors's
// embeddings tensor fails Load with a named error, rather than succeeding
// and later panicking inside Embed with a slice-bounds error on whatever
// text happens to tokenize to that id.
func TestLoadRejectsVocabIDPastEmbeddingsRowCount(t *testing.T) {
	dir := t.TempDir()
	writeTestModelDir(t, dir, defaultTestModelConfig())

	// Corrupt tokenizer.json's vocab with an id far past testVocab's real
	// range, as if the tokenizer and tensor came from mismatched revisions.
	vocab := make(map[string]int32, len(testVocab)+1)
	for k, v := range testVocab {
		vocab[k] = v
	}
	vocab["ghost"] = 9999

	writeJSONFile(t, filepath.Join(dir, "tokenizer.json"), map[string]any{
		"normalizer": map[string]any{
			"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true,
			"strip_accents": nil, "lowercase": true,
		},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type": "WordPiece", "unk_token": "[UNK]", "continuing_subword_prefix": "##",
			"max_input_chars_per_word": 15, "vocab": vocab,
		},
	})

	if _, err := Load(dir); err == nil {
		t.Error("Load with a vocab id past the embeddings tensor's row count succeeded, want an error")
	}
}

// TestLoadRejectsNegativeVocabID is the same check for a negative id, which
// panics Embed's row slice on the other side (a negative start index).
func TestLoadRejectsNegativeVocabID(t *testing.T) {
	dir := t.TempDir()
	writeTestModelDir(t, dir, defaultTestModelConfig())

	vocab := make(map[string]int32, len(testVocab)+1)
	for k, v := range testVocab {
		vocab[k] = v
	}
	vocab["ghost"] = -5

	writeJSONFile(t, filepath.Join(dir, "tokenizer.json"), map[string]any{
		"normalizer": map[string]any{
			"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true,
			"strip_accents": nil, "lowercase": true,
		},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type": "WordPiece", "unk_token": "[UNK]", "continuing_subword_prefix": "##",
			"max_input_chars_per_word": 15, "vocab": vocab,
		},
	})

	if _, err := Load(dir); err == nil {
		t.Error("Load with a negative vocab id succeeded, want an error")
	}
}

func TestLoadPreservesUnicodeVocabEntry(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())
	id, ok := m.tokenizer.vocab["café"]
	if !ok {
		t.Fatal(`vocab has no entry for "café"`)
	}
	if id != testVocab["café"] {
		t.Errorf(`vocab["café"] = %d, want %d`, id, testVocab["café"])
	}
}

// TestTokenizeEdgeCases covers the table-driven edge cases the spec calls
// out by name: empty input, whitespace only, lone punctuation, a word
// longer than max_input_chars_per_word, an all-[UNK] sentence, mixed CJK
// and ASCII, accented input, and subword splitting.
func TestTokenizeEdgeCases(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())

	tests := []struct {
		name string
		text string
		want []int
	}{
		{"empty", "", nil},
		{"whitespace only", "   \t\n  ", nil},
		{"lone punctuation", "...", []int{48, 48, 48}},
		{"word longer than max_input_chars_per_word", "abcdefghijklmnopqrstuvwxyz", nil},
		{"all-UNK sentence", "xyzzyzzy plugh", nil},
		{"mixed CJK and ASCII", "hello中world", []int{5, 10, 6}},
		{"accented input (accents stripped by default)", "café", []int{8}},
		{"subword splitting", "running", []int{11, 12}},
		{"plain sentence", "hello world", []int{5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Tokenize(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Tokenize(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestTokenizeAccentedVocabEntryReachableWhenStripAccentsDisabled(t *testing.T) {
	cfg := defaultTestModelConfig()
	cfg.stripAccents = boolPtr(false)
	m := newTestModel(t, cfg)

	got := m.Tokenize("café")
	want := []int{9} // the literal "café" vocab entry, accent intact.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenize(café) with strip_accents=false = %v, want %v", got, want)
	}
}

func TestTokenizeStripAccentsNullDefersToLowercase(t *testing.T) {
	cfg := defaultTestModelConfig()
	cfg.lowercase = false
	cfg.stripAccents = nil
	m := newTestModel(t, cfg)

	// lowercase is false, so a null strip_accents must resolve to false
	// too: the accent on "café" must survive, but nothing gets lowercased
	// (there is nothing to lowercase in this input anyway).
	got := m.Tokenize("café")
	want := []int{9}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenize(café) with lowercase=false, strip_accents=null = %v, want %v", got, want)
	}
}

func TestEmbedZeroVectorWhenNoIDsSurvive(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())
	for _, text := range []string{"", "   ", "xyzzyzzy plugh"} {
		vec := m.Embed(text)
		if len(vec) != testDim {
			t.Fatalf("Embed(%q) has length %d, want %d", text, len(vec), testDim)
		}
		for i, v := range vec {
			if v != 0 {
				t.Errorf("Embed(%q)[%d] = %v, want 0", text, i, v)
			}
		}
	}
}

// TestEmbedMeanPoolUnnormalized checks Embed's arithmetic directly against
// an independently computed mean of testEmbeddingRow rows, using a model
// configured with normalize: false so no L2 division muddies the
// comparison.
func TestEmbedMeanPoolUnnormalized(t *testing.T) {
	cfg := defaultTestModelConfig()
	cfg.normalize = false
	m := newTestModel(t, cfg)

	ids := []int32{5, 6} // "hello", "world"
	want := make([]float32, testDim)
	for _, id := range ids {
		for d, v := range testEmbeddingRow(id, testDim) {
			want[d] += v
		}
	}
	for d := range want {
		want[d] /= float32(len(ids))
	}

	got := m.Embed("hello world")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Embed(hello world) = %v, want %v", got, want)
	}
}

func TestEmbedNormalizedIsUnitNorm(t *testing.T) {
	cfg := defaultTestModelConfig()
	cfg.normalize = true
	m := newTestModel(t, cfg)

	vec := m.Embed("hello world")
	var sumSquares float64
	for _, v := range vec {
		sumSquares += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSquares)
	if math.Abs(norm-1) > 1e-6 {
		t.Errorf("||Embed(hello world)|| = %v, want 1", norm)
	}
}

// TestEmbedNormalizedZeroPooledVectorReturnsZeroNotNaN checks that
// normalize=true does not divide by a zero norm: an embeddings tensor whose
// rows for the tokenized ids are all zero (a degenerate but not impossible
// model) must return the zero vector, not an all-NaN one that would poison
// every cosine score it later touches.
func TestEmbedNormalizedZeroPooledVectorReturnsZeroNotNaN(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultTestModelConfig()
	cfg.normalize = true
	writeTestModelDir(t, dir, cfg)

	// Overwrite model.safetensors with an all-zero embeddings matrix so
	// every token's row is zero and the mean pool is exactly zero too.
	zeroPath := filepath.Join(dir, "model.safetensors")
	raw, err := os.ReadFile(zeroPath)
	if err != nil {
		t.Fatalf("reading model.safetensors: %v", err)
	}
	headerLen := binary.LittleEndian.Uint64(raw[:8])
	dataStart := 8 + headerLen
	for i := dataStart; i < uint64(len(raw)); i++ {
		raw[i] = 0
	}
	if err := os.WriteFile(zeroPath, raw, 0o644); err != nil {
		t.Fatalf("writing zeroed model.safetensors: %v", err)
	}

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	vec := m.Embed("hello world")
	for i, v := range vec {
		if v != 0 {
			t.Errorf("Embed[%d] = %v, want 0 (not NaN) for an all-zero embeddings tensor", i, v)
		}
	}
}

func TestEmbedDeterministic(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())
	const text = "hello world, running with a café"

	first := m.Embed(text)
	second := m.Embed(text)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("Embed is not deterministic:\n%v\n%v", first, second)
	}
}

func TestTokenizePreTruncatesPathologicallyLongInput(t *testing.T) {
	m := newTestModel(t, defaultTestModelConfig())

	// One word repeated far past the pre-truncation window; Tokenize must
	// not hang or panic, and must still return a bounded result.
	huge := strings.Repeat("hello ", 100000)
	ids := m.Tokenize(huge)
	if len(ids) > maxTokens {
		t.Errorf("Tokenize returned %d ids, want at most %d", len(ids), maxTokens)
	}
}
