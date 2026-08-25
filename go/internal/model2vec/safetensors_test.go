package model2vec

import (
	"encoding/binary"
	"os"
	"testing"
)

// realHeaderJSON is byte-for-byte the header of the real
// minishlab/potion-base-8M model.safetensors, including the two trailing
// ASCII spaces the safetensors writer pads it with to align the data
// section to an 8-byte boundary. Its length, 80, is the literal value
// named in the package spec.
const realHeaderJSON = `{"embeddings":{"dtype":"F32","shape":[29528,256],"data_offsets":[0,30236672]}}  `

func lengthPrefixed(header string) []byte {
	buf := make([]byte, safetensorsHeaderLenBytes+len(header))
	binary.LittleEndian.PutUint64(buf, uint64(len(header)))
	copy(buf[safetensorsHeaderLenBytes:], header)
	return buf
}

func TestParseSafetensorsHeaderRealFileHeader(t *testing.T) {
	if len(realHeaderJSON) != 80 {
		t.Fatalf("realHeaderJSON is %d bytes, want 80", len(realHeaderJSON))
	}

	info, headerEnd, err := parseSafetensorsHeader(lengthPrefixed(realHeaderJSON))
	if err != nil {
		t.Fatalf("parseSafetensorsHeader = %v", err)
	}
	if info.Dtype != "F32" {
		t.Errorf("Dtype = %q, want F32", info.Dtype)
	}
	if want := []int64{29528, 256}; len(info.Shape) != 2 || info.Shape[0] != want[0] || info.Shape[1] != want[1] {
		t.Errorf("Shape = %v, want %v", info.Shape, want)
	}
	if want := [2]int64{0, 30236672}; info.DataOffsets != want {
		t.Errorf("DataOffsets = %v, want %v", info.DataOffsets, want)
	}
	if want := int64(safetensorsHeaderLenBytes + 80); headerEnd != want {
		t.Errorf("headerEnd = %d, want %d", headerEnd, want)
	}
}

func TestParseSafetensorsHeaderRejectsWeightsTensor(t *testing.T) {
	header := `{"weights":{"dtype":"F32","shape":[10,2],"data_offsets":[0,80]},` +
		`"mapping":{"dtype":"I64","shape":[10],"data_offsets":[80,160]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err == nil {
		t.Error("parseSafetensorsHeader on a weights/mapping header succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderRejectsWrongDtype(t *testing.T) {
	header := `{"embeddings":{"dtype":"F16","shape":[10,2],"data_offsets":[0,40]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err == nil {
		t.Error("parseSafetensorsHeader with dtype F16 succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderRejectsWrongShapeRank(t *testing.T) {
	header := `{"embeddings":{"dtype":"F32","shape":[10],"data_offsets":[0,40]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err == nil {
		t.Error("parseSafetensorsHeader with a 1-dimensional shape succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderRejectsMissingEmbeddingsTensor(t *testing.T) {
	header := `{"pooling":{"dtype":"F32","shape":[10,2],"data_offsets":[0,80]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err == nil {
		t.Error("parseSafetensorsHeader with no embeddings tensor succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderRejectsExtraTensor(t *testing.T) {
	header := `{"embeddings":{"dtype":"F32","shape":[10,2],"data_offsets":[0,80]},` +
		`"extra":{"dtype":"F32","shape":[10,2],"data_offsets":[80,160]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err == nil {
		t.Error("parseSafetensorsHeader with two tensors succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderIgnoresMetadata(t *testing.T) {
	header := `{"__metadata__":{"format":"pt"},"embeddings":{"dtype":"F32","shape":[10,2],"data_offsets":[0,80]}}`
	if _, _, err := parseSafetensorsHeader(lengthPrefixed(header)); err != nil {
		t.Errorf("parseSafetensorsHeader with a __metadata__ entry = %v, want success", err)
	}
}

func TestParseSafetensorsHeaderTruncatedLengthPrefix(t *testing.T) {
	if _, _, err := parseSafetensorsHeader([]byte{1, 2, 3}); err == nil {
		t.Error("parseSafetensorsHeader on a 3-byte file succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderLengthExceedsFile(t *testing.T) {
	buf := make([]byte, safetensorsHeaderLenBytes)
	binary.LittleEndian.PutUint64(buf, 1000)
	if _, _, err := parseSafetensorsHeader(buf); err == nil {
		t.Error("parseSafetensorsHeader with a header length past EOF succeeded, want an error")
	}
}

func TestParseSafetensorsHeaderInvalidJSON(t *testing.T) {
	if _, _, err := parseSafetensorsHeader(lengthPrefixed("not json")); err == nil {
		t.Error("parseSafetensorsHeader on invalid JSON succeeded, want an error")
	}
}

func TestParseSafetensorsEmbeddingsRoundTrip(t *testing.T) {
	const vocabSize, dim = 3, 4
	dir := t.TempDir()
	path := dir + "/model.safetensors"
	writeTestSafetensors(t, path, vocabSize, dim)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading test safetensors file: %v", err)
	}
	gotVocabSize, gotDim, embeddings, err := parseSafetensorsEmbeddings(raw)
	if err != nil {
		t.Fatalf("parseSafetensorsEmbeddings = %v", err)
	}
	if gotVocabSize != vocabSize || gotDim != dim {
		t.Fatalf("parseSafetensorsEmbeddings shape = [%d,%d], want [%d,%d]", gotVocabSize, gotDim, vocabSize, dim)
	}
	for id := 0; id < vocabSize; id++ {
		want := testEmbeddingRow(int32(id), dim)
		got := embeddings[id*dim : (id+1)*dim]
		for d := range want {
			if got[d] != want[d] {
				t.Errorf("row %d[%d] = %v, want %v", id, d, got[d], want[d])
			}
		}
	}
}

func TestParseSafetensorsEmbeddingsDataOffsetsMismatch(t *testing.T) {
	header := `{"embeddings":{"dtype":"F32","shape":[10,2],"data_offsets":[0,4]}}`
	buf := lengthPrefixed(header)
	buf = append(buf, make([]byte, 80)...)
	if _, _, _, err := parseSafetensorsEmbeddings(buf); err == nil {
		t.Error("parseSafetensorsEmbeddings with mismatched data_offsets succeeded, want an error")
	}
}

// TestParseSafetensorsEmbeddingsRejectsOverflowingShape checks a shape
// whose product overflows int64 once multiplied by 4 (the bytes-per-float32
// factor): unguarded, vocabSize*dim*4 wraps to 0, which then equals a
// data_offsets span of [0,0] and lets the header through to
// make([]float32, vocabSize*dim) with a length no real machine can satisfy.
func TestParseSafetensorsEmbeddingsRejectsOverflowingShape(t *testing.T) {
	header := `{"embeddings":{"dtype":"F32","shape":[1,4611686018427387904],"data_offsets":[0,0]}}`
	buf := lengthPrefixed(header)
	if _, _, _, err := parseSafetensorsEmbeddings(buf); err == nil {
		t.Error("parseSafetensorsEmbeddings with an overflowing shape succeeded, want an error")
	}
}

func TestParseSafetensorsEmbeddingsDataPastEOF(t *testing.T) {
	header := `{"embeddings":{"dtype":"F32","shape":[10,2],"data_offsets":[0,80]}}`
	buf := lengthPrefixed(header) // no data section appended at all.
	if _, _, _, err := parseSafetensorsEmbeddings(buf); err == nil {
		t.Error("parseSafetensorsEmbeddings with data past EOF succeeded, want an error")
	}
}
