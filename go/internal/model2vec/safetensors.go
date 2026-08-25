package model2vec

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// safetensorsHeaderLenBytes is the width of the little-endian header-length
// prefix every safetensors file starts with.
const safetensorsHeaderLenBytes = 8

// maxSafetensorsDim ceilings each dimension of the embeddings tensor's
// shape: far beyond any real embedding matrix (the pinned model is
// [29528,256]), but small enough that shape[0]*shape[1]*4 can never
// overflow int64, so parseSafetensorsEmbeddings's byte-span check cannot be
// defeated by a wraparound.
const maxSafetensorsDim = 1 << 24

// tensorInfo is one entry of a safetensors header: a tensor's dtype, shape,
// and its byte range within the data section that follows the header.
type tensorInfo struct {
	Dtype       string   `json:"dtype"`
	Shape       []int64  `json:"shape"`
	DataOffsets [2]int64 `json:"data_offsets"`
}

// parseSafetensorsHeader reads a safetensors file's 8-byte little-endian
// header length and its JSON header, and returns the "embeddings" tensor's
// info plus the byte offset where the header ends and tensor data begins.
// It requires exactly one tensor, named "embeddings"; a "weights" or
// "mapping" tensor is rejected by name, since a vocabulary-quantised model
// needs both and neither is supported. It does not read or validate the
// data section itself; parseSafetensorsEmbeddings does that.
func parseSafetensorsHeader(raw []byte) (info tensorInfo, headerEnd int64, err error) {
	if len(raw) < safetensorsHeaderLenBytes {
		return tensorInfo{}, 0, errors.New("safetensors file is shorter than the 8-byte header length prefix")
	}
	headerLen := binary.LittleEndian.Uint64(raw[:safetensorsHeaderLenBytes])
	if headerLen > uint64(len(raw)-safetensorsHeaderLenBytes) {
		return tensorInfo{}, 0, fmt.Errorf(
			"safetensors header length %d exceeds the %d bytes remaining in the file", headerLen, len(raw)-safetensorsHeaderLenBytes)
	}
	headerEnd = safetensorsHeaderLenBytes + int64(headerLen)
	header := raw[safetensorsHeaderLenBytes:headerEnd]

	var rawTensors map[string]json.RawMessage
	if err := json.Unmarshal(header, &rawTensors); err != nil {
		return tensorInfo{}, 0, fmt.Errorf("invalid safetensors header: %w", err)
	}
	delete(rawTensors, "__metadata__")

	if _, bad := rawTensors["weights"]; bad {
		return tensorInfo{}, 0, errors.New(
			`model.safetensors has a "weights" tensor; vocabulary-quantised models are unsupported`)
	}
	if _, bad := rawTensors["mapping"]; bad {
		return tensorInfo{}, 0, errors.New(
			`model.safetensors has a "mapping" tensor; vocabulary-quantised models are unsupported`)
	}
	if len(rawTensors) != 1 {
		return tensorInfo{}, 0, fmt.Errorf(
			`model.safetensors has %d tensors, want exactly one named "embeddings"`, len(rawTensors))
	}
	rawInfo, ok := rawTensors["embeddings"]
	if !ok {
		return tensorInfo{}, 0, errors.New(`model.safetensors has no tensor named "embeddings"`)
	}
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		return tensorInfo{}, 0, fmt.Errorf("invalid safetensors embeddings tensor: %w", err)
	}
	if info.Dtype != "F32" {
		return tensorInfo{}, 0, fmt.Errorf("embeddings tensor has dtype %q, want F32", info.Dtype)
	}
	if len(info.Shape) != 2 {
		return tensorInfo{}, 0, fmt.Errorf("embeddings tensor has %d dimensions, want 2", len(info.Shape))
	}
	if info.Shape[0] <= 0 || info.Shape[1] <= 0 {
		return tensorInfo{}, 0, fmt.Errorf("embeddings tensor has invalid shape [%d,%d]", info.Shape[0], info.Shape[1])
	}
	// Reject a shape whose product overflows before parseSafetensorsEmbeddings
	// ever computes vocabSize*dim*4: an overflowed byte count can wrap
	// around to match a tiny (or zero) data_offsets span, letting a
	// multi-exabyte shape pass every later check and reach
	// make([]float32, vocabSize*dim) with a length no real machine can
	// satisfy. maxSafetensorsDim ceilings each dimension well below where
	// their product could approach math.MaxInt64/4, with room to spare for
	// any real embedding matrix.
	if info.Shape[0] > maxSafetensorsDim || info.Shape[1] > maxSafetensorsDim {
		return tensorInfo{}, 0, fmt.Errorf(
			"embeddings tensor shape [%d,%d] exceeds the %d-per-dimension limit", info.Shape[0], info.Shape[1], maxSafetensorsDim)
	}
	if info.DataOffsets[0] < 0 || info.DataOffsets[1] < info.DataOffsets[0] {
		return tensorInfo{}, 0, fmt.Errorf(
			"embeddings tensor has invalid data_offsets [%d,%d]", info.DataOffsets[0], info.DataOffsets[1])
	}
	return info, headerEnd, nil
}

// parseSafetensorsEmbeddings reads a safetensors file's header and the
// embeddings tensor's raw little-endian F32 data, and returns it decoded as
// a row-major [vocabSize, dim] matrix.
func parseSafetensorsEmbeddings(raw []byte) (vocabSize, dim int, embeddings []float32, err error) {
	info, headerEnd, err := parseSafetensorsHeader(raw)
	if err != nil {
		return 0, 0, nil, err
	}

	vocabSize64, dim64 := info.Shape[0], info.Shape[1]
	wantBytes := vocabSize64 * dim64 * 4
	start, end := info.DataOffsets[0], info.DataOffsets[1]
	if end-start != wantBytes {
		return 0, 0, nil, fmt.Errorf(
			"embeddings tensor data_offsets span %d bytes, want %d for shape [%d,%d]", end-start, wantBytes, vocabSize64, dim64)
	}
	dataStart := headerEnd + start
	dataEnd := headerEnd + end
	if dataEnd > int64(len(raw)) {
		return 0, 0, nil, fmt.Errorf(
			"embeddings tensor data extends to byte %d, past the file's %d bytes", dataEnd, len(raw))
	}

	data := raw[dataStart:dataEnd]
	vocabSize, dim = int(vocabSize64), int(dim64)
	embeddings = make([]float32, vocabSize*dim)
	for i := range embeddings {
		bits := binary.LittleEndian.Uint32(data[i*4 : i*4+4])
		embeddings[i] = math.Float32frombits(bits)
	}
	return vocabSize, dim, embeddings, nil
}
