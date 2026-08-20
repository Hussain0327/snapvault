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

// DirName is the name of the search sidecar directory inside a repository's
// metadata directory.
const DirName = "index"

// FileName is the name of the search index file inside DirName.
const FileName = "embeddings.svi"

const (
	indexMagic  = "SVX1"
	blobIDBytes = 32

	// maxDim ceilings a declared vector dimension: the builtin embedder
	// uses 512 and no real embedding model gets anywhere near this, so a
	// larger value can only be a corrupt or hostile index file. Without a
	// ceiling, decodeEntry's make([]float32, dim) would request as much
	// address space as a 4-byte header field says to, before ever reading
	// a single vector byte.
	maxDim = 1 << 16
)

// Entry is one chunk's embedding record: the blob it came from, that blob's
// chunks are numbered from 0 by Sequence, a short human-readable preview,
// and the chunk's L2-normalized embedding.
type Entry struct {
	BlobID   string
	Sequence int32
	Snippet  string
	Vector   []float32
}

// Index is the decoded contents of a search index sidecar file: every
// chunk's embedding, plus the embedder that produced them and the vector
// dimension they share.
type Index struct {
	EmbedderID string
	Dim        int32
	Entries    []Entry
}

// Write serializes idx to path in the SVX1 binary format, replacing any
// existing file atomically: a temporary file in the same directory is
// written and then renamed over the destination, so a reader never observes
// a partial index.
func Write(path string, idx Index) error {
	if idx.EmbedderID == "" {
		return errors.New("index embedder id cannot be empty")
	}
	if idx.Dim <= 0 {
		return fmt.Errorf("invalid embedding dimension: %d", idx.Dim)
	}
	for i, e := range idx.Entries {
		if err := object.RequireID(e.BlobID); err != nil {
			return fmt.Errorf("entry %d has an invalid blob id: %w", i, err)
		}
		if int32(len(e.Vector)) != idx.Dim {
			return fmt.Errorf(
				"entry %d has a %d-dimension vector, index declares %d", i, len(e.Vector), idx.Dim)
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

func encodeIndex(w io.Writer, idx Index) error {
	if _, err := io.WriteString(w, indexMagic); err != nil {
		return err
	}
	if err := writeInt32(w, int32(len(idx.EmbedderID))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, idx.EmbedderID); err != nil {
		return err
	}
	if err := writeInt32(w, idx.Dim); err != nil {
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
	if err := writeInt32(w, int32(len(e.Snippet))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, e.Snippet); err != nil {
		return err
	}
	for _, v := range e.Vector {
		if err := writeFloat32(w, v); err != nil {
			return err
		}
	}
	return nil
}

// Read parses the SVX1 index file at path.
func Read(path string) (Index, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Index{}, err
	}
	return decodeIndex(raw)
}

func decodeIndex(raw []byte) (Index, error) {
	r := &indexReader{rest: raw}

	magic, err := r.bytes(len(indexMagic))
	if err != nil || string(magic) != indexMagic {
		return Index{}, errors.New("not a SnapVault search index (bad magic)")
	}
	idLen, err := r.int32()
	if err != nil {
		return Index{}, fmt.Errorf("truncated index header: %w", err)
	}
	embedderID, err := r.utf8String(idLen)
	if err != nil {
		return Index{}, fmt.Errorf("invalid embedder id: %w", err)
	}
	dim, err := r.int32()
	if err != nil {
		return Index{}, fmt.Errorf("truncated index header: %w", err)
	}
	if dim <= 0 || dim > maxDim {
		return Index{}, fmt.Errorf("invalid embedding dimension: %d", dim)
	}

	var entries []Entry
	for len(r.rest) > 0 {
		e, err := decodeEntry(r, dim)
		if err != nil {
			return Index{}, err
		}
		entries = append(entries, e)
	}
	return Index{EmbedderID: embedderID, Dim: dim, Entries: entries}, nil
}

func decodeEntry(r *indexReader, dim int32) (Entry, error) {
	id, err := r.bytes(blobIDBytes)
	if err != nil {
		return Entry{}, fmt.Errorf("truncated index entry: %w", err)
	}
	seq, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated index entry: %w", err)
	}
	snipLen, err := r.int32()
	if err != nil {
		return Entry{}, fmt.Errorf("truncated index entry: %w", err)
	}
	snippet, err := r.utf8String(snipLen)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid index snippet: %w", err)
	}
	vector := make([]float32, dim)
	for i := range vector {
		v, err := r.float32()
		if err != nil {
			return Entry{}, fmt.Errorf("truncated index entry: %w", err)
		}
		vector[i] = v
	}
	return Entry{
		BlobID:   hex.EncodeToString(id),
		Sequence: seq,
		Snippet:  snippet,
		Vector:   vector,
	}, nil
}

// indexReader decodes the big-endian primitives of the SVX1 format from an
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
