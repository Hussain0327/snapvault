package model2vec

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// testVocab is the tiny hand-written vocabulary shared by every test that
// builds a model: the five special tokens at ids 0-4, plain words, a few
// "##"-prefixed continuations for subword splitting, one accented word
// ("café", reachable only when strip_accents is disabled) beside its
// unaccented form ("cafe", reachable through the default normalization
// pipeline), one CJK character, and a handful of ASCII punctuation marks.
var testVocab = map[string]int32{
	"[PAD]": 0, "[UNK]": 1, "[CLS]": 2, "[SEP]": 3, "[MASK]": 4,
	"hello": 5, "world": 6, "test": 7, "cafe": 8, "café": 9, "中": 10,
	"run": 11, "##ning": 12, "word": 13, "##word": 14, "piece": 15,
	"##piece": 16, "##ing": 17, "##s": 18, "##a": 19, "play": 20,
	"##ed": 21, "cat": 22, "dog": 23, "house": 24, "tree": 25,
	"book": 26, "read": 27, "write": 28, "code": 29, "bug": 30,
	"fix": 31, "one": 32, "two": 33, "three": 34, "four": 35,
	"five": 36, "six": 37, "seven": 38, "eight": 39, "nine": 40,
	"ten": 41, "red": 42, "blue": 43, "green": 44, "yellow": 45,
	"big": 46, "small": 47, ".": 48, "!": 49, "?": 50,
}

// testDim is the embedding width of every model built by newTestModel.
const testDim = 8

// testModelConfig holds the tokenizer.json normalizer flags and config.json
// normalize flag for a tiny test model; the zero value is not usable, use
// defaultTestModelConfig.
type testModelConfig struct {
	normalize            bool
	cleanText            bool
	handleChineseChars   bool
	stripAccents         *bool // nil marshals to JSON null: "same as lowercase"
	lowercase            bool
	maxInputCharsPerWord int
}

func defaultTestModelConfig() testModelConfig {
	return testModelConfig{
		normalize:            true,
		cleanText:            true,
		handleChineseChars:   true,
		stripAccents:         nil,
		lowercase:            true,
		maxInputCharsPerWord: 15,
	}
}

// testEmbeddingRow deterministically generates the embedding row a test
// model assigns to vocabulary id: dimension d holds id*10+d, small enough
// integers that mean-pooling stays exact in float32 arithmetic. Tests that
// need an expected Embed result recompute it from this same function rather
// than hardcoding vectors.
func testEmbeddingRow(id int32, dim int) []float32 {
	row := make([]float32, dim)
	for d := range row {
		row[d] = float32(int(id)*10 + d)
	}
	return row
}

// writeTestSafetensors writes a minimal safetensors file at path with a
// single "embeddings" tensor of shape [vocabSize, dim], populated by
// testEmbeddingRow.
func writeTestSafetensors(t *testing.T, path string, vocabSize, dim int) {
	t.Helper()

	dataLen := vocabSize * dim * 4
	header := fmt.Sprintf(`{"embeddings":{"dtype":"F32","shape":[%d,%d],"data_offsets":[0,%d]}}`, vocabSize, dim, dataLen)

	var buf bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(header)))
	buf.Write(lenBuf[:])
	buf.WriteString(header)

	for id := 0; id < vocabSize; id++ {
		for _, v := range testEmbeddingRow(int32(id), dim) {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
			buf.Write(b[:])
		}
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing test safetensors file: %v", err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshaling %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// writeTestModelDir writes config.json, tokenizer.json, and
// model.safetensors under dir, following cfg and testVocab.
func writeTestModelDir(t *testing.T, dir string, cfg testModelConfig) {
	t.Helper()

	writeJSONFile(t, filepath.Join(dir, "config.json"), map[string]any{
		"normalize":  cfg.normalize,
		"hidden_dim": testDim,
		// distillation-time provenance the package must ignore.
		"apply_pca":  testDim,
		"apply_zipf": true,
	})

	writeJSONFile(t, filepath.Join(dir, "tokenizer.json"), map[string]any{
		"normalizer": map[string]any{
			"type":                 "BertNormalizer",
			"clean_text":           cfg.cleanText,
			"handle_chinese_chars": cfg.handleChineseChars,
			"strip_accents":        cfg.stripAccents,
			"lowercase":            cfg.lowercase,
		},
		"pre_tokenizer": map[string]any{
			"type": "BertPreTokenizer",
		},
		"model": map[string]any{
			"type":                      "WordPiece",
			"unk_token":                 "[UNK]",
			"continuing_subword_prefix": "##",
			"max_input_chars_per_word":  cfg.maxInputCharsPerWord,
			"vocab":                     testVocab,
		},
		// post_processor is present in every real tokenizer.json but must
		// be ignored: inference never adds [CLS]/[SEP].
		"post_processor": map[string]any{
			"type": "TemplateProcessing",
		},
	})

	writeTestSafetensors(t, filepath.Join(dir, "model.safetensors"), len(testVocab), testDim)
}

// newTestModel builds a tiny model under cfg in a fresh t.TempDir() and
// loads it.
func newTestModel(t *testing.T, cfg testModelConfig) *Model {
	t.Helper()
	dir := t.TempDir()
	writeTestModelDir(t, dir, cfg)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s) = %v", dir, err)
	}
	return m
}

func boolPtr(b bool) *bool { return &b }
