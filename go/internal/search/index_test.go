package search

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

func testIndex(t *testing.T) *Index {
	t.Helper()
	idx := NewIndex("builtin-lexical-v1", 3, "0123456789abcdef")
	idx.Add(
		object.ID(object.TypeBlob, []byte("payment systems overview")),
		Chunk{Sequence: 0, StartRune: 0, EndRune: 5, Text: "hello", Snippet: "Payment systems process transactions."},
		[]float32{0.6, 0.8, 0},
		[]string{"payment", "systems", "payment"},
	)
	idx.Add(
		object.ID(object.TypeBlob, []byte("weather forecasting overview")),
		Chunk{Sequence: 1, StartRune: 5, EndRune: 10, Text: "world", Snippet: "Weather forecasting models predict rainfall."},
		[]float32{0, -1, 0},
		[]string{"weather", "forecasting"},
	)
	return idx
}

func TestIndexAddInternsTermsAcrossEntries(t *testing.T) {
	idx := testIndex(t)

	wantTerms := []string{"payment", "systems", "weather", "forecasting"}
	if len(idx.Terms) != len(wantTerms) {
		t.Fatalf("Terms = %v, want %v", idx.Terms, wantTerms)
	}
	for i, term := range wantTerms {
		if idx.Terms[i] != term {
			t.Errorf("Terms[%d] = %q, want %q", i, idx.Terms[i], term)
		}
	}

	if len(idx.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(idx.Entries))
	}
	first := idx.Entries[0]
	wantFirst := []TermFreq{{Term: 0, Count: 2}, {Term: 1, Count: 1}}
	if len(first.Terms) != len(wantFirst) || first.Terms[0] != wantFirst[0] || first.Terms[1] != wantFirst[1] {
		t.Errorf("Entries[0].Terms = %v, want %v", first.Terms, wantFirst)
	}
	if first.StartRune != 0 || first.EndRune != 5 {
		t.Errorf("Entries[0] rune range = [%d:%d], want [0:5]", first.StartRune, first.EndRune)
	}

	second := idx.Entries[1]
	wantSecond := []TermFreq{{Term: 2, Count: 1}, {Term: 3, Count: 1}}
	if len(second.Terms) != len(wantSecond) || second.Terms[0] != wantSecond[0] || second.Terms[1] != wantSecond[1] {
		t.Errorf("Entries[1].Terms = %v, want %v", second.Terms, wantSecond)
	}
}

func TestIndexAddReusesTermIndexAfterDecode(t *testing.T) {
	// A term interned via Add on a freshly decoded Index (termIndex not yet
	// built) must still dedupe against the terms that decode produced.
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := Write(path, testIndex(t)); err != nil {
		t.Fatalf("Write = %v", err)
	}
	idx, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	before := len(idx.Terms)
	idx.Add(object.ID(object.TypeBlob, []byte("z")), Chunk{EndRune: 1, Text: "z", Snippet: "z"},
		[]float32{1, 0, 0}, []string{"payment"})
	if len(idx.Terms) != before {
		t.Errorf("Terms grew from %d to %d after interning an existing term, want unchanged", before, len(idx.Terms))
	}
}

func TestIndexWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	want := testIndex(t)

	if err := Write(path, want); err != nil {
		t.Fatalf("Write = %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}

	if got.EmbedderID != want.EmbedderID {
		t.Errorf("EmbedderID = %q, want %q", got.EmbedderID, want.EmbedderID)
	}
	if got.Dim != want.Dim {
		t.Errorf("Dim = %d, want %d", got.Dim, want.Dim)
	}
	if got.IndexHash != want.IndexHash {
		t.Errorf("IndexHash = %q, want %q", got.IndexHash, want.IndexHash)
	}
	if len(got.Terms) != len(want.Terms) {
		t.Fatalf("len(Terms) = %d, want %d", len(got.Terms), len(want.Terms))
	}
	for i := range want.Terms {
		if got.Terms[i] != want.Terms[i] {
			t.Errorf("Terms[%d] = %q, want %q", i, got.Terms[i], want.Terms[i])
		}
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("len(Entries) = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for i := range want.Entries {
		g, w := got.Entries[i], want.Entries[i]
		if g.BlobID != w.BlobID || g.Sequence != w.Sequence || g.StartRune != w.StartRune ||
			g.EndRune != w.EndRune || g.Snippet != w.Snippet {
			t.Errorf("Entries[%d] = %+v, want %+v", i, g, w)
		}
		if len(g.Terms) != len(w.Terms) {
			t.Fatalf("Entries[%d].Terms = %v, want %v", i, g.Terms, w.Terms)
		}
		for j := range w.Terms {
			if g.Terms[j] != w.Terms[j] {
				t.Errorf("Entries[%d].Terms[%d] = %v, want %v", i, j, g.Terms[j], w.Terms[j])
			}
		}
		if len(g.Vector) != len(w.Vector) {
			t.Fatalf("Entries[%d].Vector = %v, want %v", i, g.Vector, w.Vector)
		}
		for j := range w.Vector {
			if g.Vector[j] != w.Vector[j] {
				t.Errorf("Entries[%d].Vector[%d] = %v, want %v", i, j, g.Vector[j], w.Vector[j])
			}
		}
	}
}

func TestIndexWriteReadRoundTripEmptyIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	want := NewIndex("builtin-lexical-v1", 3, "0123456789abcdef")

	if err := Write(path, want); err != nil {
		t.Fatalf("Write = %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if got.EmbedderID != want.EmbedderID || got.Dim != want.Dim || got.IndexHash != want.IndexHash {
		t.Errorf("header = %+v, want %+v", got, want)
	}
	if len(got.Terms) != 0 {
		t.Errorf("Terms = %v, want empty", got.Terms)
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %v, want empty", got.Entries)
	}
}

func TestIndexWriteIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "embeddings.svi")

	first := NewIndex("builtin-lexical-v1", 3, "0123456789abcdef")
	first.Add(object.ID(object.TypeBlob, []byte("a")), Chunk{EndRune: 1, Text: "a", Snippet: "a"}, []float32{1, 0, 0}, nil)
	if err := Write(path, first); err != nil {
		t.Fatalf("first Write = %v", err)
	}
	second := testIndex(t)
	if err := Write(path, second); err != nil {
		t.Fatalf("second Write = %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if len(got.Entries) != len(second.Entries) {
		t.Errorf("Read after overwrite has %d entries, want %d", len(got.Entries), len(second.Entries))
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".index-*"))
	if err != nil {
		t.Fatalf("Glob = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestIndexWriteCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index", "embeddings.svi")
	if err := Write(path, testIndex(t)); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if _, err := Read(path); err != nil {
		t.Fatalf("Read = %v", err)
	}
}

func TestIndexFileNameConstants(t *testing.T) {
	if DirName != "index" {
		t.Errorf("DirName = %q, want %q", DirName, "index")
	}
	if FileName != "embeddings.svi" {
		t.Errorf("FileName = %q, want %q", FileName, "embeddings.svi")
	}
}

func putInt32(v int32) []byte {
	u := uint32(v)
	return []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
}

func putFloat32(v float32) []byte {
	return putInt32(int32(math.Float32bits(v)))
}

// workedExampleOffsets names the byte offsets, within workedExample's
// output, of every field the bound-violation tests mutate.
type workedExampleOffsets struct {
	dim       int
	termCount int
	startRune int
	endRune   int
	snippet   int // first byte of the snippet's UTF-8 content
	tfTermID  int // term id of the sole term-frequency pair
	tfOccurs  int // occurrence count of the sole term-frequency pair
}

// workedExample builds the byte-for-byte SVX2 encoding from the design
// spec's worked example: embedder "builtin", dim 2, one term "cat", one
// chunk with an all-zero blob id and vector (1.0, 0.0). It reports the
// offsets bound-violation tests need to corrupt one field at a time.
func workedExample() ([]byte, workedExampleOffsets) {
	var buf bytes.Buffer
	var off workedExampleOffsets

	buf.WriteString(indexMagic)
	buf.Write(putInt32(7))
	buf.WriteString("builtin")
	off.dim = buf.Len()
	buf.Write(putInt32(2)) // dim
	buf.Write(putInt32(16))
	buf.WriteString("0123456789abcdef") // indexHash
	off.termCount = buf.Len()
	buf.Write(putInt32(1)) // termCount
	buf.Write(putInt32(3))
	buf.WriteString("cat")               // terms[0]
	buf.Write(putInt32(1))               // chunkCount
	buf.Write(make([]byte, blobIDBytes)) // blobID: all zero
	buf.Write(putInt32(0))               // sequence
	off.startRune = buf.Len()
	buf.Write(putInt32(0)) // startRune
	off.endRune = buf.Len()
	buf.Write(putInt32(3)) // endRune
	buf.Write(putInt32(3)) // snippetLen
	off.snippet = buf.Len()
	buf.WriteString("cat") // snippet
	buf.Write(putInt32(1)) // tfCount
	off.tfTermID = buf.Len()
	buf.Write(putInt32(0)) // term 0
	off.tfOccurs = buf.Len()
	buf.Write(putInt32(1)) // count 1
	buf.Write(putFloat32(1.0))
	buf.Write(putFloat32(0.0))

	return buf.Bytes(), off
}

// replaceInt32 overwrites the 4 big-endian bytes at byte offset off within
// raw with v, returning a new slice so the caller's original is untouched.
func replaceInt32(raw []byte, off int, v int32) []byte {
	out := append([]byte(nil), raw...)
	copy(out[off:off+4], putInt32(v))
	return out
}

func TestDecodeIndexWorkedExample(t *testing.T) {
	raw, _ := workedExample()
	idx, err := decodeIndex(raw)
	if err != nil {
		t.Fatalf("decodeIndex(workedExample) = %v", err)
	}
	if idx.EmbedderID != "builtin" {
		t.Errorf("EmbedderID = %q, want %q", idx.EmbedderID, "builtin")
	}
	if idx.Dim != 2 {
		t.Errorf("Dim = %d, want 2", idx.Dim)
	}
	if idx.IndexHash != "0123456789abcdef" {
		t.Errorf("IndexHash = %q, want %q", idx.IndexHash, "0123456789abcdef")
	}
	if len(idx.Terms) != 1 || idx.Terms[0] != "cat" {
		t.Errorf("Terms = %v, want [cat]", idx.Terms)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(idx.Entries))
	}
	e := idx.Entries[0]
	wantBlobID := hex.EncodeToString(make([]byte, blobIDBytes))
	if e.BlobID != wantBlobID {
		t.Errorf("BlobID = %q, want %q", e.BlobID, wantBlobID)
	}
	if e.Sequence != 0 || e.StartRune != 0 || e.EndRune != 3 {
		t.Errorf("Sequence/StartRune/EndRune = %d/%d/%d, want 0/0/3", e.Sequence, e.StartRune, e.EndRune)
	}
	if e.Snippet != "cat" {
		t.Errorf("Snippet = %q, want %q", e.Snippet, "cat")
	}
	if len(e.Terms) != 1 || e.Terms[0] != (TermFreq{Term: 0, Count: 1}) {
		t.Errorf("Terms = %v, want [{0 1}]", e.Terms)
	}
	if len(e.Vector) != 2 || e.Vector[0] != 1.0 || e.Vector[1] != 0.0 {
		t.Errorf("Vector = %v, want [1 0]", e.Vector)
	}
}

func TestReadRejectsSVX1Magic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := os.WriteFile(path, []byte(svx1Magic), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	_, err := Read(path)
	if !errors.Is(err, ErrIndexOutdated) {
		t.Errorf("Read of an SVX1 file = %v, want errors.Is(err, ErrIndexOutdated)", err)
	}
}

func TestReadRejectsUnknownMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := os.WriteFile(path, []byte("not an index file at all"), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Error("Read of a non-index file succeeded, want an error")
	}
}

func TestReadRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := Write(path, testIndex(t)); err != nil {
		t.Fatalf("Write = %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if err := os.WriteFile(path, full[:len(full)-1], 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Error("Read of a truncated index file succeeded, want an error")
	}
}

// TestDecodeIndexRejectsEachBoundInIsolation mutates one worked-example
// field at a time and asserts decodeIndex rejects it, so a regression that
// drops any single bound check is caught precisely.
func TestDecodeIndexRejectsEachBoundInIsolation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(raw []byte, off workedExampleOffsets) []byte
	}{
		{"dim 0", func(raw []byte, off workedExampleOffsets) []byte {
			return replaceInt32(raw, off.dim, 0)
		}},
		{"dim 65537", func(raw []byte, off workedExampleOffsets) []byte {
			return replaceInt32(raw, off.dim, 65537)
		}},
		{"term count too large for input", func(raw []byte, off workedExampleOffsets) []byte {
			return replaceInt32(raw, off.termCount, 1<<20)
		}},
		{"term id >= termCount", func(raw []byte, off workedExampleOffsets) []byte {
			// termCount is 1, so the only valid id is 0; 1 is out of range.
			return replaceInt32(raw, off.tfTermID, 1)
		}},
		{"count 0", func(raw []byte, off workedExampleOffsets) []byte {
			return replaceInt32(raw, off.tfOccurs, 0)
		}},
		{"startRune > endRune", func(raw []byte, off workedExampleOffsets) []byte {
			return replaceInt32(raw, off.startRune, 10) // endRune stays 3
		}},
		{"truncated vector", func(raw []byte, off workedExampleOffsets) []byte {
			return raw[:len(raw)-1]
		}},
		{"invalid UTF-8 string", func(raw []byte, off workedExampleOffsets) []byte {
			out := append([]byte(nil), raw...)
			out[off.snippet] = 0xff
			return out
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, off := workedExample()
			mutated := tt.mutate(raw, off)
			if _, err := decodeIndex(mutated); err == nil {
				t.Errorf("decodeIndex succeeded for %s, want an error", tt.name)
			}
		})
	}
}

func TestIndexWriteRejectsVectorDimensionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 3, "0123456789abcdef")
	idx.Entries = []Entry{{
		BlobID:   object.ID(object.TypeBlob, []byte("x")),
		Sequence: 0,
		EndRune:  1,
		Snippet:  "short",
		Vector:   []float32{1, 2}, // declares dim 3, vector has 2
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a mismatched vector dimension succeeded, want an error")
	}
}

func TestIndexWriteRejectsInvalidBlobID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Entries = []Entry{{
		BlobID:   "not-a-valid-object-id",
		Sequence: 0,
		EndRune:  1,
		Snippet:  "short",
		Vector:   []float32{1},
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with an invalid blob id succeeded, want an error")
	}
}

func TestIndexWriteRejectsEmptyEmbedderID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("", 3, "0123456789abcdef")
	if err := Write(path, idx); err == nil {
		t.Error("Write with an empty embedder id succeeded, want an error")
	}
}

func TestIndexWriteRejectsOutOfRangeTermID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Entries = []Entry{{
		BlobID:  object.ID(object.TypeBlob, []byte("x")),
		EndRune: 1,
		Snippet: "short",
		Terms:   []TermFreq{{Term: 0, Count: 1}}, // idx.Terms is empty: id 0 is out of range
		Vector:  []float32{1},
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with an out-of-range term id succeeded, want an error")
	}
}

// The following TestIndexWriteRejects* cases close the Write/Read
// asymmetry: before this fix, Write happily produced a file for each of
// these Index values that decodeIndex's own bound checks (added alongside
// these Write checks) call corrupt, so SnapVault could write an index it
// could not itself read back correctly.

func TestIndexWriteRejectsNonFiniteVectorComponent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Entries = []Entry{{
		BlobID:  object.ID(object.TypeBlob, []byte("x")),
		EndRune: 1,
		Snippet: "short",
		Vector:  []float32{float32(math.NaN())},
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a NaN vector component succeeded, want an error")
	}
}

func TestIndexWriteRejectsDuplicateTermID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Terms = []string{"cat"}
	idx.Entries = []Entry{{
		BlobID:  object.ID(object.TypeBlob, []byte("x")),
		EndRune: 1,
		Snippet: "short",
		Terms:   []TermFreq{{Term: 0, Count: 1}, {Term: 0, Count: 1}},
		Vector:  []float32{1},
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a duplicate term id within one entry succeeded, want an error")
	}
}

func TestIndexWriteRejectsDuplicateTermString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Terms = []string{"cat", "cat"}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a duplicate term string succeeded, want an error")
	}
}

func TestIndexWriteRejectsNegativeSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := NewIndex("builtin-lexical-v1", 1, "0123456789abcdef")
	idx.Entries = []Entry{{
		BlobID:   object.ID(object.TypeBlob, []byte("x")),
		Sequence: -1,
		EndRune:  1,
		Snippet:  "short",
		Vector:   []float32{1},
	}}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a negative sequence succeeded, want an error")
	}
}

// TestDecodeEntryRejectsTFCountExceedingRemainingBytes builds a chunk record
// directly (bypassing decodeIndex's own term-table parsing) whose tfCount is
// within the tfCount <= termCount bound but has no bytes behind it, so only
// the remaining-bytes guard added to decodeEntry can reject it. Without that
// guard, make([]TermFreq, tfCount) would commit far more memory than the
// input could possibly back before the first read ever fails.
func TestDecodeEntryRejectsTFCountExceedingRemainingBytes(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(make([]byte, blobIDBytes)) // blobID
	buf.Write(putInt32(0))               // sequence
	buf.Write(putInt32(0))               // startRune
	buf.Write(putInt32(0))               // endRune
	buf.Write(putInt32(0))               // snippet length 0
	buf.Write(putInt32(1 << 22))         // tfCount: huge, but <= termCount below
	// No tf bytes, and no vector, follow.

	r := &indexReader{rest: buf.Bytes()}
	if _, err := decodeEntry(r, 2, 1<<22); err == nil {
		t.Error("decodeEntry with a huge tfCount and no backing bytes succeeded, want an error")
	}
}

// TestDecodeEntryRejectsNonFiniteVectorComponent checks that a NaN or +Inf
// vector component is rejected at decode time rather than round-tripping
// into an Entry that would later poison FuseAlpha's min-max normalisation.
func TestDecodeEntryRejectsNonFiniteVectorComponent(t *testing.T) {
	tests := []struct {
		name string
		bits float32
	}{
		{"NaN", float32(math.NaN())},
		{"+Inf", float32(math.Inf(1))},
		{"-Inf", float32(math.Inf(-1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			buf.Write(make([]byte, blobIDBytes))
			buf.Write(putInt32(0)) // sequence
			buf.Write(putInt32(0)) // startRune
			buf.Write(putInt32(0)) // endRune
			buf.Write(putInt32(0)) // snippet length 0
			buf.Write(putInt32(0)) // tfCount 0
			buf.Write(putFloat32(tt.bits))

			r := &indexReader{rest: buf.Bytes()}
			if _, err := decodeEntry(r, 1, 0); err == nil {
				t.Errorf("decodeEntry with a %s vector component succeeded, want an error", tt.name)
			}
		})
	}
}

// TestDecodeEntryRejectsDuplicateTermID checks that a chunk listing the same
// term id twice is rejected: unchecked, it lets df exceed the entry count
// in NewBM25 and can invert idf's sign.
func TestDecodeEntryRejectsDuplicateTermID(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(make([]byte, blobIDBytes))
	buf.Write(putInt32(0)) // sequence
	buf.Write(putInt32(0)) // startRune
	buf.Write(putInt32(0)) // endRune
	buf.Write(putInt32(0)) // snippet length 0
	buf.Write(putInt32(2)) // tfCount 2
	buf.Write(putInt32(0)) // term 0
	buf.Write(putInt32(1)) // count 1
	buf.Write(putInt32(0)) // term 0 again: duplicate
	buf.Write(putInt32(1)) // count 1

	r := &indexReader{rest: buf.Bytes()}
	if _, err := decodeEntry(r, 0, 5); err == nil {
		t.Error("decodeEntry with a duplicate term id succeeded, want an error")
	}
}

// TestDecodeEntryRejectsEndRuneBeyondMaxTextBytes checks that an EndRune
// past the byte cap on any real extracted text is rejected, rather than
// latently surviving to panic a future consumer that slices text with it.
func TestDecodeEntryRejectsEndRuneBeyondMaxTextBytes(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(make([]byte, blobIDBytes))
	buf.Write(putInt32(0))                       // sequence
	buf.Write(putInt32(0))                       // startRune
	buf.Write(putInt32(int32(maxTextBytes + 1))) // endRune: one past the cap
	buf.Write(putInt32(0))                       // snippet length 0
	buf.Write(putInt32(0))                       // tfCount 0

	r := &indexReader{rest: buf.Bytes()}
	if _, err := decodeEntry(r, 0, 0); err == nil {
		t.Error("decodeEntry with endRune beyond maxTextBytes succeeded, want an error")
	}
}

// TestDecodeEntryRejectsNegativeSequence checks that decodeEntry rejects a
// negative Sequence, matching Write's own validation.
func TestDecodeEntryRejectsNegativeSequence(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(make([]byte, blobIDBytes))
	buf.Write(putInt32(-1)) // sequence: negative
	buf.Write(putInt32(0))  // startRune
	buf.Write(putInt32(0))  // endRune
	buf.Write(putInt32(0))  // snippet length 0
	buf.Write(putInt32(0))  // tfCount 0

	r := &indexReader{rest: buf.Bytes()}
	if _, err := decodeEntry(r, 0, 0); err == nil {
		t.Error("decodeEntry with a negative sequence succeeded, want an error")
	}
}

// TestDecodeIndexRejectsEmptyEmbedderID checks that decodeIndex rejects an
// empty embedder id, matching Write's own validation: before this fix, a
// crafted file with a zero-length embedder id decoded with no error even
// though Write rejects that exact Index.
func TestDecodeIndexRejectsEmptyEmbedderID(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(indexMagic)
	buf.Write(putInt32(0)) // embedderID length 0: empty
	buf.Write(putInt32(1)) // dim
	buf.Write(putInt32(16))
	buf.WriteString("0123456789abcdef") // indexHash
	buf.Write(putInt32(0))              // termCount 0
	buf.Write(putInt32(0))              // chunkCount 0

	if _, err := decodeIndex(buf.Bytes()); err == nil {
		t.Error("decodeIndex with an empty embedder id succeeded, want an error")
	}
}

// TestDecodeIndexRejectsDuplicateTermString checks that decodeIndex rejects
// a term table with a repeated string, matching the spec's "unique, in
// first-seen order" invariant: unenforced, NewBM25 would resolve a query
// term to the last duplicate id and silently zero-score entries holding an
// earlier one.
func TestDecodeIndexRejectsDuplicateTermString(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(indexMagic)
	buf.Write(putInt32(7))
	buf.WriteString("builtin")
	buf.Write(putInt32(1)) // dim
	buf.Write(putInt32(16))
	buf.WriteString("0123456789abcdef") // indexHash
	buf.Write(putInt32(2))              // termCount 2
	buf.Write(putInt32(3))
	buf.WriteString("cat") // terms[0]
	buf.Write(putInt32(3))
	buf.WriteString("cat") // terms[1]: duplicate
	buf.Write(putInt32(0)) // chunkCount 0

	if _, err := decodeIndex(buf.Bytes()); err == nil {
		t.Error("decodeIndex with a duplicate term string succeeded, want an error")
	}
}

// TestDecodeIndexRejectsTrailingBytes checks that decodeIndex rejects a
// well-formed encoding with garbage appended after the last chunk, so the
// file is canonical: exactly one byte string decodes to any given Index.
func TestDecodeIndexRejectsTrailingBytes(t *testing.T) {
	raw, _ := workedExample()
	raw = append(raw, []byte("TRAILING JUNK")...)
	if _, err := decodeIndex(raw); err == nil {
		t.Error("decodeIndex with trailing bytes succeeded, want an error")
	}
}

// TestReadRejectsFileLargerThanCeiling shrinks maxIndexFileBytes for the
// duration of the test so the check can be exercised without actually
// writing a file anywhere near its real size.
func TestReadRejectsFileLargerThanCeiling(t *testing.T) {
	old := maxIndexFileBytes
	maxIndexFileBytes = 16
	t.Cleanup(func() { maxIndexFileBytes = old })

	raw, _ := workedExample() // far more than 16 bytes
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("writing test index file: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Error("Read on a file exceeding maxIndexFileBytes succeeded, want an error")
	}
}

// FuzzDecodeIndex asserts decodeIndex never panics, seeded with a valid
// encoding, the SVX1 sentinel, and assorted truncations and garbage, and
// that it never allocates drastically more than the input size. A
// length-prefixed format inherently costs more heap than its wire bytes
// (a Go string or slice header alone is 16-24 bytes against as little as 4
// on-disk bytes of length prefix), so amplificationCeiling below is not 1x;
// it is chosen generously above the largest amplification measured by hand
// across every decode path (terms ~4x, term-frequencies ~6x, entries
// ~1.7x), so it still catches a regression that makes any of them
// unbounded rather than merely proportional to the input.
func FuzzDecodeIndex(f *testing.F) {
	full, _ := workedExample()
	f.Add(full)
	f.Add([]byte(svx1Magic))
	f.Add([]byte{})
	f.Add([]byte("garbage"))
	for _, n := range []int{1, 4, 8, 20, len(full) / 2, len(full) - 1} {
		f.Add(full[:n])
	}

	// amplificationCeiling bounds bytes allocated by decodeIndex per byte
	// of input; fixedOverheadBytes absorbs Go runtime/test-harness
	// allocation noise around tiny or empty inputs where a ratio is
	// meaningless.
	const amplificationCeiling = 8
	const fixedOverheadBytes = 65536

	f.Fuzz(func(t *testing.T, data []byte) {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		decodeIndex(data)

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		allocated := after.TotalAlloc - before.TotalAlloc
		limit := uint64(len(data))*amplificationCeiling + fixedOverheadBytes
		if allocated > limit {
			t.Errorf("decodeIndex(%d input bytes) allocated %d bytes, want at most %d (%dx + %d fixed overhead)",
				len(data), allocated, limit, amplificationCeiling, fixedOverheadBytes)
		}
	})
}
