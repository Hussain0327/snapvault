package search

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// DirName is the name of the search sidecar directory inside a
// repository's metadata directory.
const DirName = "index"

// FileName is the name of the search index file inside DirName.
const FileName = "embeddings.svi"

const (
	indexMagic  = "SVX2"
	svx1Magic   = "SVX1"
	blobIDBytes = 32

	// maxDim ceilings a declared vector dimension: no real embedding model
	// gets anywhere near this, so a larger value can only be a corrupt or
	// hostile index file. Without a ceiling, decodeEntry's
	// make([]float32, dim) would request as much address space as a
	// 4-byte header field says to, before ever reading a single vector
	// byte.
	maxDim = 1 << 16

	// maxTermCount ceilings the declared size of the term string table for
	// the same reason maxDim ceilings the vector dimension.
	maxTermCount = 1 << 24

	// minStringFieldBytes is the smallest possible on-disk size of a
	// length-prefixed string: its 4-byte length, even when the string
	// itself is empty. A count of strings whose declared total exceeds
	// this times the remaining bytes cannot possibly be backed by the
	// input, so it is rejected before any per-string allocation.
	minStringFieldBytes = 4

	// minChunkFieldBytes is the smallest possible on-disk size of one
	// chunk record excluding its vector: blob id, sequence, start rune,
	// end rune, an empty snippet's length prefix, and a zero
	// term-frequency count.
	minChunkFieldBytes = blobIDBytes + 4 + 4 + 4 + minStringFieldBytes + 4
)

// maxIndexFileBytes ceilings how large a file Read is willing to load into
// memory before decodeIndex gets a chance to reject it: generous enough for
// any index maxTermCount and maxDim can actually describe, while still
// bounding a truncated-and-regrown or hostile sidecar file from being fully
// read into memory before its magic bytes are even examined. It is a var,
// not a const, so a test can shrink it temporarily rather than writing a
// file that actually reaches it.
var maxIndexFileBytes int64 = 512 << 20 // 512 MiB

// ErrIndexOutdated is returned by Read when the file was built by the
// older SVX1 format.
var ErrIndexOutdated = errors.New("search index was built by an older SnapVault; run 'snapvault index' to rebuild")

// TermFreq is one term's occurrence count within a chunk, the term
// identified by its position in Index.Terms.
type TermFreq struct {
	Term  int32
	Count int32
}

// Entry is one chunk's search record: the blob it came from, its position
// in that blob's extracted text, a short human-readable preview, its
// lexical term frequencies, and its dense embedding.
type Entry struct {
	BlobID    string
	Sequence  int32
	StartRune int32
	EndRune   int32
	Snippet   string
	Terms     []TermFreq
	Vector    []float32
}

// Index is the decoded contents of a search index sidecar file: the
// embedder and Pipeline it was built with, the interned term vocabulary
// shared by every entry's Terms, and every chunk's record.
type Index struct {
	EmbedderID string
	Dim        int32
	IndexHash  string
	Terms      []string
	Entries    []Entry

	// termIndex caches Terms's inverse mapping for Add; it is rebuilt
	// lazily from Terms and never serialized.
	termIndex map[string]int32
}

// NewIndex returns an empty index for embedderID's dim-dimensional
// vectors, built under the pipeline whose IndexHash is indexHash.
func NewIndex(embedderID string, dim int, indexHash string) *Index {
	return &Index{
		EmbedderID: embedderID,
		Dim:        int32(dim),
		IndexHash:  indexHash,
	}
}

// Add interns terms into idx.Terms and appends one Entry for chunk c of
// blobID, embedded as vector. Duplicate terms in terms are folded into one
// TermFreq whose Count is their number of occurrences.
func (idx *Index) Add(blobID string, c Chunk, vector []float32, terms []string) {
	var freq []TermFreq
	positionOf := make(map[int32]int, len(terms))
	for _, term := range terms {
		id := idx.internTerm(term)
		if pos, ok := positionOf[id]; ok {
			freq[pos].Count++
			continue
		}
		positionOf[id] = len(freq)
		freq = append(freq, TermFreq{Term: id, Count: 1})
	}

	idx.Entries = append(idx.Entries, Entry{
		BlobID:    blobID,
		Sequence:  int32(c.Sequence),
		StartRune: int32(c.StartRune),
		EndRune:   int32(c.EndRune),
		Snippet:   c.Snippet,
		Terms:     freq,
		Vector:    vector,
	})
}

// internTerm returns term's id in idx.Terms, appending it and extending
// the cache when term has not been seen before.
func (idx *Index) internTerm(term string) int32 {
	if idx.termIndex == nil {
		idx.termIndex = make(map[string]int32, len(idx.Terms))
		for i, t := range idx.Terms {
			idx.termIndex[t] = int32(i)
		}
	}
	if id, ok := idx.termIndex[term]; ok {
		return id
	}
	id := int32(len(idx.Terms))
	idx.Terms = append(idx.Terms, term)
	idx.termIndex[term] = id
	return id
}

// Write serializes idx to path in the SVX2 binary format, replacing any
// existing file atomically: a temporary file in the same directory is
// written and then renamed over the destination, so a reader never
// observes a partial index.
func Write(path string, idx *Index) error {
	if idx.EmbedderID == "" {
		return errors.New("index embedder id cannot be empty")
	}
	if idx.Dim < 1 || idx.Dim > maxDim {
		return fmt.Errorf("invalid embedding dimension: %d", idx.Dim)
	}
	if len(idx.Terms) > maxTermCount {
		return fmt.Errorf("term count %d exceeds the maximum of %d", len(idx.Terms), maxTermCount)
	}
	seenTermString := make(map[string]bool, len(idx.Terms))
	for i, term := range idx.Terms {
		if len(term) > maxTextBytes {
			return fmt.Errorf("term %d is %d bytes, exceeds the maximum of %d", i, len(term), maxTextBytes)
		}
		if seenTermString[term] {
			return fmt.Errorf("term %d: duplicate term %q", i, term)
		}
		seenTermString[term] = true
	}
	for i, e := range idx.Entries {
		if err := object.RequireID(e.BlobID); err != nil {
			return fmt.Errorf("entry %d has an invalid blob id: %w", i, err)
		}
		if e.Sequence < 0 {
			return fmt.Errorf("entry %d has a negative sequence: %d", i, e.Sequence)
		}
		if int32(len(e.Vector)) != idx.Dim {
			return fmt.Errorf(
				"entry %d has a %d-dimension vector, index declares %d", i, len(e.Vector), idx.Dim)
		}
		if e.StartRune < 0 || e.StartRune > e.EndRune {
			return fmt.Errorf("entry %d has an invalid rune range [%d:%d]", i, e.StartRune, e.EndRune)
		}
		if int64(e.EndRune) > int64(maxTextBytes) {
			return fmt.Errorf("entry %d end rune %d exceeds the %d-byte extracted-text cap", i, e.EndRune, maxTextBytes)
		}
		if len(e.Snippet) > maxTextBytes {
			return fmt.Errorf("entry %d snippet is %d bytes, exceeds the maximum of %d", i, len(e.Snippet), maxTextBytes)
		}
		for _, v := range e.Vector {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return fmt.Errorf("entry %d has a non-finite vector component: %v", i, v)
			}
		}
		seenTermID := make(map[int32]bool, len(e.Terms))
		for j, tf := range e.Terms {
			if tf.Term < 0 || int(tf.Term) >= len(idx.Terms) {
				return fmt.Errorf("entry %d term-frequency %d references out-of-range term id %d", i, j, tf.Term)
			}
			if seenTermID[tf.Term] {
				return fmt.Errorf("entry %d term-frequency %d: duplicate term id %d", i, j, tf.Term)
			}
			seenTermID[tf.Term] = true
			if tf.Count < 1 {
				return fmt.Errorf("entry %d term-frequency %d has a non-positive count %d", i, j, tf.Count)
			}
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	if err := encodeIndex(tmp, idx); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}

func encodeIndex(w io.Writer, idx *Index) error {
	if _, err := io.WriteString(w, indexMagic); err != nil {
		return err
	}
	if err := writeString(w, idx.EmbedderID); err != nil {
		return err
	}
	if err := writeInt32(w, idx.Dim); err != nil {
		return err
	}
	if err := writeString(w, idx.IndexHash); err != nil {
		return err
	}
	if err := writeInt32(w, int32(len(idx.Terms))); err != nil {
		return err
	}
	for _, term := range idx.Terms {
		if err := writeString(w, term); err != nil {
			return err
		}
	}
	if err := writeInt32(w, int32(len(idx.Entries))); err != nil {
		return err
	}
	for _, e := range idx.Entries {
		if err := encodeEntry(w, e); err != nil {
			return err
		}
	}
	return nil
}

func encodeEntry(w io.Writer, e Entry) error {
	raw, err := hex.DecodeString(e.BlobID)
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := writeInt32(w, e.Sequence); err != nil {
		return err
	}
	if err := writeInt32(w, e.StartRune); err != nil {
		return err
	}
	if err := writeInt32(w, e.EndRune); err != nil {
		return err
	}
	if err := writeString(w, e.Snippet); err != nil {
		return err
	}
	if err := writeInt32(w, int32(len(e.Terms))); err != nil {
		return err
	}
	for _, tf := range e.Terms {
		if err := writeInt32(w, tf.Term); err != nil {
			return err
		}
		if err := writeInt32(w, tf.Count); err != nil {
			return err
		}
	}
	for _, v := range e.Vector {
		if err := writeFloat32(w, v); err != nil {
			return err
		}
	}
	return nil
}

// writeString writes s as a big-endian int32 byte length followed by its
// UTF-8 bytes.
func writeString(w io.Writer, s string) error {
	if err := writeInt32(w, int32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

// Read parses the SVX2 index file at path. A file whose magic is the
// older "SVX1" format returns ErrIndexOutdated.
func Read(path string) (*Index, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxIndexFileBytes {
		return nil, fmt.Errorf(
			"index file is %d bytes, larger than the %d-byte ceiling for a well-formed index", info.Size(), maxIndexFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeIndex(raw)
}

func decodeIndex(raw []byte) (*Index, error) {
	r := &indexReader{rest: raw}

	magic, err := r.bytes(4)
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	switch string(magic) {
	case indexMagic:
		// SVX2: fall through to the rest of the header.
	case svx1Magic:
		return nil, ErrIndexOutdated
	default:
		return nil, errors.New("not a SnapVault search index (bad magic)")
	}

	idLen, err := r.int32()
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	embedderID, err := r.utf8String(idLen)
	if err != nil {
		return nil, fmt.Errorf("invalid embedder id: %w", err)
	}
	if embedderID == "" {
		return nil, errors.New("index embedder id cannot be empty")
	}

	dim, err := r.int32()
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	if dim < 1 || dim > maxDim {
		return nil, fmt.Errorf("invalid embedding dimension: %d", dim)
	}

	hashLen, err := r.int32()
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	indexHash, err := r.utf8String(hashLen)
	if err != nil {
		return nil, fmt.Errorf("invalid index hash: %w", err)
	}

	termCount, err := r.int32()
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	if termCount < 0 || termCount > maxTermCount {
		return nil, fmt.Errorf("invalid term count: %d", termCount)
	}
	if int64(termCount)*minStringFieldBytes > int64(len(r.rest)) {
		return nil, fmt.Errorf("term count %d is too large for the remaining %d bytes", termCount, len(r.rest))
	}

	terms := make([]string, termCount)
	seenTerm := make(map[string]bool, termCount)
	for i := range terms {
		tLen, err := r.int32()
		if err != nil {
			return nil, fmt.Errorf("truncated term %d: %w", i, err)
		}
		term, err := r.utf8String(tLen)
		if err != nil {
			return nil, fmt.Errorf("invalid term %d: %w", i, err)
		}
		if seenTerm[term] {
			return nil, fmt.Errorf("term %d: duplicate term %q", i, term)
		}
		seenTerm[term] = true
		terms[i] = term
	}

	chunkCount, err := r.int32()
	if err != nil {
		return nil, fmt.Errorf("truncated index header: %w", err)
	}
	if chunkCount < 0 {
		return nil, fmt.Errorf("invalid chunk count: %d", chunkCount)
	}
	minChunkBytes := int64(minChunkFieldBytes) + int64(dim)*4
	if int64(chunkCount)*minChunkBytes > int64(len(r.rest)) {
		return nil, fmt.Errorf("chunk count %d is too large for the remaining %d bytes", chunkCount, len(r.rest))
	}

	entries := make([]Entry, chunkCount)
	for i := range entries {
		e, err := decodeEntry(r, dim, termCount)
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", i, err)
		}
		entries[i] = e
	}

	if len(r.rest) != 0 {
		return nil, fmt.Errorf("%d unexpected trailing bytes after the last chunk", len(r.rest))
	}

	return &Index{
		EmbedderID: embedderID,
		Dim:        dim,
		IndexHash:  indexHash,
		Terms:      terms,
		Entries:    entries,
	}, nil
}

func decodeEntry(r *indexReader, dim, termCount int32) (Entry, error) {
	id, err := r.bytes(blobIDBytes)
	if err != nil {
		return Entry{}, fmt.Errorf("truncated blob id: %w", err)
	}
	sequence, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated sequence: %w", err)
	}
	if sequence < 0 {
		return Entry{}, fmt.Errorf("invalid sequence: %d", sequence)
	}
	startRune, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated start rune: %w", err)
	}
	endRune, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated end rune: %w", err)
	}
	if startRune < 0 || startRune > endRune {
		return Entry{}, fmt.Errorf("invalid rune range [%d:%d]", startRune, endRune)
	}

	snipLen, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated snippet length: %w", err)
	}
	snippet, err := r.utf8String(snipLen)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid snippet: %w", err)
	}

	tfCount, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated term-frequency count: %w", err)
	}
	if tfCount < 0 || tfCount > termCount {
		return Entry{}, fmt.Errorf("invalid term-frequency count: %d", tfCount)
	}
	// Each tf record is 8 bytes (term id + count); reject before the
	// allocation below so a huge tfCount that merely satisfies
	// tfCount <= termCount cannot still commit far more memory than the
	// input could possibly back.
	if int64(tfCount)*8 > int64(len(r.rest)) {
		return Entry{}, fmt.Errorf(
			"term-frequency count %d is too large for the remaining %d bytes", tfCount, len(r.rest))
	}
	terms := make([]TermFreq, tfCount)
	seenTerm := make(map[int32]bool, tfCount)
	for i := range terms {
		term, err := r.int32()
		if err != nil {
			return Entry{}, fmt.Errorf("truncated term-frequency %d: %w", i, err)
		}
		if term < 0 || term >= termCount {
			return Entry{}, fmt.Errorf("term-frequency %d: term id %d out of range [0, %d)", i, term, termCount)
		}
		if seenTerm[term] {
			return Entry{}, fmt.Errorf("term-frequency %d: duplicate term id %d", i, term)
		}
		seenTerm[term] = true
		count, err := r.int32()
		if err != nil {
			return Entry{}, fmt.Errorf("truncated term-frequency %d: %w", i, err)
		}
		if count < 1 {
			return Entry{}, fmt.Errorf("term-frequency %d: count must be at least 1, got %d", i, count)
		}
		terms[i] = TermFreq{Term: term, Count: count}
	}

	if int64(endRune) > int64(maxTextBytes) {
		return Entry{}, fmt.Errorf(
			"end rune %d exceeds the %d-byte extracted-text cap; no real chunk range can reach it", endRune, maxTextBytes)
	}

	vector := make([]float32, dim)
	for i := range vector {
		v, err := r.float32()
		if err != nil {
			return Entry{}, fmt.Errorf("truncated vector: %w", err)
		}
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return Entry{}, fmt.Errorf("vector component %d is not finite: %v", i, v)
		}
		vector[i] = v
	}

	return Entry{
		BlobID:    hex.EncodeToString(id),
		Sequence:  sequence,
		StartRune: startRune,
		EndRune:   endRune,
		Snippet:   snippet,
		Terms:     terms,
		Vector:    vector,
	}, nil
}

// indexReader decodes the big-endian primitives of the SVX2 format from an
// in-memory byte slice, reporting io.ErrUnexpectedEOF when it runs out of
// input early.
type indexReader struct {
	rest []byte
}

func (r *indexReader) bytes(n int) ([]byte, error) {
	if n < 0 || len(r.rest) < n {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.rest[:n]
	r.rest = r.rest[n:]
	return b, nil
}

func (r *indexReader) uint32() (uint32, error) {
	b, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

func (r *indexReader) int32() (int32, error) {
	v, err := r.uint32()
	return int32(v), err
}

func (r *indexReader) float32() (float32, error) {
	v, err := r.uint32()
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}

func (r *indexReader) utf8String(byteCount int32) (string, error) {
	if byteCount < 0 || int64(byteCount) > int64(maxTextBytes) {
		return "", fmt.Errorf("invalid string length: %d", byteCount)
	}
	raw, err := r.bytes(int(byteCount))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", errors.New("string is not valid UTF-8")
	}
	return string(raw), nil
}

// writeUint32 writes v as 4 big-endian bytes.
func writeUint32(w io.Writer, v uint32) error {
	buf := [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	_, err := w.Write(buf[:])
	return err
}

func writeInt32(w io.Writer, v int32) error {
	return writeUint32(w, uint32(v))
}

func writeFloat32(w io.Writer, v float32) error {
	return writeUint32(w, math.Float32bits(v))
}
