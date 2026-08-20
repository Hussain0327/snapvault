package search

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

func testEntries(t *testing.T) []Entry {
	t.Helper()
	return []Entry{
		{
			BlobID:   object.ID(object.TypeBlob, []byte("payment systems overview")),
			Sequence: 0,
			Snippet:  "Payment systems process transactions between banks and café merchants.",
			Vector:   []float32{0.6, 0.8, 0},
		},
		{
			BlobID:   object.ID(object.TypeBlob, []byte("weather forecasting overview")),
			Sequence: 1,
			Snippet:  "Weather forecasting models predict rainfall.",
			Vector:   []float32{0, -1, 0},
		},
	}
}

func TestIndexWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	want := Index{
		EmbedderID: "builtin-lexical-v1",
		Dim:        3,
		Entries:    testEntries(t),
	}

	if err := Write(path, want); err != nil {
		t.Fatalf("Write = %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read round-trip mismatch:\n got:  %+v\n want: %+v", got, want)
	}
}

func TestIndexWriteIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "embeddings.svi")

	first := Index{EmbedderID: "builtin-lexical-v1", Dim: 3, Entries: testEntries(t)[:1]}
	if err := Write(path, first); err != nil {
		t.Fatalf("first Write = %v", err)
	}
	second := Index{EmbedderID: "builtin-lexical-v1", Dim: 3, Entries: testEntries(t)}
	if err := Write(path, second); err != nil {
		t.Fatalf("second Write = %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Errorf("Read after overwrite = %+v, want %+v", got, second)
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
	idx := Index{EmbedderID: "builtin-lexical-v1", Dim: 3, Entries: testEntries(t)}
	if err := Write(path, idx); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if _, err := Read(path); err != nil {
		t.Fatalf("Read = %v", err)
	}
}

func TestIndexReadRejectsBadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	if err := os.WriteFile(path, []byte("not an index file at all"), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Error("Read of a non-index file succeeded, want an error")
	}
}

func TestIndexReadRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := Index{EmbedderID: "builtin-lexical-v1", Dim: 3, Entries: testEntries(t)}
	if err := Write(path, idx); err != nil {
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

// TestIndexReadRejectsImplausibleDimension reproduces a hand-crafted index
// whose declared dim is large enough (0x7FFFFFFF) to make a naive
// make([]float32, dim) request roughly 8 GiB from four attacker-controlled
// header bytes. Read must reject the dimension itself -- checked against
// errString rather than just "any error", so this fails if the rejection
// regresses to some unrelated truncated-read error once more file bytes are
// added instead of an actual ceiling.
func TestIndexReadRejectsImplausibleDimension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	var raw []byte
	raw = append(raw, []byte(indexMagic)...)
	embedderID := "builtin-lexical-v1"
	raw = append(raw, putInt32(int32(len(embedderID)))...)
	raw = append(raw, []byte(embedderID)...)
	raw = append(raw, putInt32(0x7fffffff)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read of an index with an implausible dimension succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "invalid embedding dimension") {
		t.Errorf("Read error = %q, want it to reject the dimension itself", err)
	}
}

func putInt32(v int32) []byte {
	u := uint32(v)
	return []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
}

func TestIndexWriteRejectsVectorDimensionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := Index{
		EmbedderID: "builtin-lexical-v1",
		Dim:        3,
		Entries: []Entry{{
			BlobID:   object.ID(object.TypeBlob, []byte("x")),
			Sequence: 0,
			Snippet:  "short",
			Vector:   []float32{1, 2}, // declares dim 3, vector has 2
		}},
	}
	if err := Write(path, idx); err == nil {
		t.Error("Write with a mismatched vector dimension succeeded, want an error")
	}
}

func TestIndexWriteRejectsInvalidBlobID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := Index{
		EmbedderID: "builtin-lexical-v1",
		Dim:        1,
		Entries: []Entry{{
			BlobID:   "not-a-valid-object-id",
			Sequence: 0,
			Snippet:  "short",
			Vector:   []float32{1},
		}},
	}
	if err := Write(path, idx); err == nil {
		t.Error("Write with an invalid blob id succeeded, want an error")
	}
}

func TestIndexWriteRejectsEmptyEmbedderID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embeddings.svi")
	idx := Index{EmbedderID: "", Dim: 3, Entries: nil}
	if err := Write(path, idx); err == nil {
		t.Error("Write with an empty embedder id succeeded, want an error")
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
