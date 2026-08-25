package search

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestModel writes a minimal, valid Model2Vec model (config.json,
// tokenizer.json, model.safetensors) into dir: a three-word vocabulary and
// dim-dimensional embedding rows deterministically derived from each
// vocabulary id (row id holds id*10+d at dimension d), so an expected
// embedding can be recomputed rather than hardcoded.
func writeTestModel(t *testing.T, dir string, dim int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) = %v", dir, err)
	}

	vocab := map[string]int32{"[UNK]": 0, "hello": 1, "world": 2}

	writeTestJSON(t, filepath.Join(dir, "config.json"), map[string]any{
		"normalize":  false,
		"hidden_dim": dim,
	})
	writeTestJSON(t, filepath.Join(dir, "tokenizer.json"), map[string]any{
		"normalizer": map[string]any{
			"type":                 "BertNormalizer",
			"clean_text":           true,
			"handle_chinese_chars": true,
			"strip_accents":        nil,
			"lowercase":            true,
		},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type":                      "WordPiece",
			"unk_token":                 "[UNK]",
			"continuing_subword_prefix": "##",
			"max_input_chars_per_word":  100,
			"vocab":                     vocab,
		},
	})

	dataLen := len(vocab) * dim * 4
	header := fmt.Sprintf(`{"embeddings":{"dtype":"F32","shape":[%d,%d],"data_offsets":[0,%d]}}`,
		len(vocab), dim, dataLen)
	var buf bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(header)))
	buf.Write(lenBuf[:])
	buf.WriteString(header)
	for id := 0; id < len(vocab); id++ {
		for d := 0; d < dim; d++ {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(id*10+d)))
			buf.Write(b[:])
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile(model.safetensors) = %v", err)
	}
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestNewStaticEmbedderNotInstalled(t *testing.T) {
	t.Setenv("SNAPVAULT_MODEL_DIR", t.TempDir())

	_, err := NewStaticEmbedder("potion-base-8M")
	if err == nil {
		t.Fatal("NewStaticEmbedder on a missing model succeeded, want an error")
	}
	want := "model potion-base-8M is not installed; run 'snapvault model pull potion-base-8M'"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestStaticEmbedderIDDimAndEmbed(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SNAPVAULT_MODEL_DIR", base)
	writeTestModel(t, filepath.Join(base, "tiny"), 4)

	e, err := NewStaticEmbedder("tiny")
	if err != nil {
		t.Fatalf("NewStaticEmbedder = %v", err)
	}
	if got, want := e.Dim(), 4; got != want {
		t.Errorf("Dim() = %d, want %d", got, want)
	}
	if !strings.HasPrefix(e.ID(), "static:tiny@") {
		t.Errorf("ID() = %q, want a static:tiny@<digest12> prefix", e.ID())
	}
	digestPart := strings.TrimPrefix(e.ID(), "static:tiny@")
	if len(digestPart) != 12 {
		t.Errorf("ID() digest suffix = %q, want 12 hex characters", digestPart)
	}

	vec, err := e.Embed("hello world")
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	if len(vec) != 4 {
		t.Fatalf("len(Embed(...)) = %d, want 4", len(vec))
	}
	var sumSquares float64
	for _, v := range vec {
		sumSquares += float64(v) * float64(v)
	}
	if norm := math.Sqrt(sumSquares); math.Abs(norm-1) > 1e-5 {
		t.Errorf("||Embed(...)|| = %v, want 1 (L2-normalized)", norm)
	}
}

func TestStaticEmbedderDeterministic(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SNAPVAULT_MODEL_DIR", base)
	writeTestModel(t, filepath.Join(base, "tiny"), 4)

	e, err := NewStaticEmbedder("tiny")
	if err != nil {
		t.Fatalf("NewStaticEmbedder = %v", err)
	}
	first, err := e.Embed("hello world")
	if err != nil {
		t.Fatalf("Embed (1) = %v", err)
	}
	second, err := e.Embed("hello world")
	if err != nil {
		t.Fatalf("Embed (2) = %v", err)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Embed is not deterministic: component %d differs (%v vs %v)", i, first[i], second[i])
		}
	}
}

func TestNewEmbedderStatic(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SNAPVAULT_MODEL_DIR", base)
	writeTestModel(t, filepath.Join(base, "tiny"), 4)

	direct, err := NewStaticEmbedder("tiny")
	if err != nil {
		t.Fatalf("NewStaticEmbedder = %v", err)
	}

	via, err := NewEmbedder(direct.ID())
	if err != nil {
		t.Fatalf("NewEmbedder(%q) = %v", direct.ID(), err)
	}
	if _, ok := via.(*StaticEmbedder); !ok {
		t.Errorf("NewEmbedder(%q) = %T, want *StaticEmbedder", direct.ID(), via)
	}
	if via.ID() != direct.ID() {
		t.Errorf("ID() = %q, want %q", via.ID(), direct.ID())
	}

	if _, err := NewEmbedder("static:tiny@000000000000"); err == nil {
		t.Error("NewEmbedder with a mismatched digest succeeded, want an error")
	}
	if _, err := NewEmbedder("static"); err == nil {
		t.Error(`NewEmbedder("static") succeeded, want an error ("static" alone is only a CLI alias)`)
	}
}
