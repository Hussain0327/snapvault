package store

import (
	"bytes"
	"compress/zlib"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("hello world\n")

	id, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	if want := object.ID(object.TypeBlob, payload); id != want {
		t.Errorf("Put returned id %s, want %s", id, want)
	}

	typ, got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if typ != object.TypeBlob {
		t.Errorf("Get type = %v, want TypeBlob", typ)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get payload = %q, want %q", got, payload)
	}
}

func TestPutDeduplicatesAndLeavesNoTempFiles(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("stored twice, kept once")

	first, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("first Put = %v", err)
	}
	second, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("second Put = %v", err)
	}
	if first != second {
		t.Errorf("ids differ: %s vs %s", first, second)
	}

	count, err := s.Count()
	if err != nil {
		t.Fatalf("Count = %v", err)
	}
	if count != 1 {
		t.Errorf("Count = %d, want 1", count)
	}
	matches, err := filepath.Glob(filepath.Join(s.dir, "tmp-*"))
	if err != nil {
		t.Fatalf("Glob = %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestPutBlobFileMatchesInMemoryPut(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("streamed in 64 KiB chunks\n"), 10_000)
	source := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	fromFile, err := s.PutBlobFile(source)
	if err != nil {
		t.Fatalf("PutBlobFile = %v", err)
	}
	if want := object.ID(object.TypeBlob, content); fromFile != want {
		t.Errorf("PutBlobFile id = %s, want %s", fromFile, want)
	}

	var sink bytes.Buffer
	if err := s.CopyPayload(fromFile, object.TypeBlob, &sink); err != nil {
		t.Fatalf("CopyPayload = %v", err)
	}
	if !bytes.Equal(sink.Bytes(), content) {
		t.Error("restored payload differs from the source file")
	}
}

func TestBlobFileIDHashesWithoutStoring(t *testing.T) {
	s := newTestStore(t)
	content := []byte("addressed, not kept")
	source := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	id, err := BlobFileID(source)
	if err != nil {
		t.Fatalf("BlobFileID = %v", err)
	}
	if want := object.ID(object.TypeBlob, content); id != want {
		t.Errorf("BlobFileID = %s, want %s", id, want)
	}
	count, err := s.Count()
	if err != nil {
		t.Fatalf("Count = %v", err)
	}
	if count != 0 {
		t.Errorf("Count = %d after hashing only, want 0", count)
	}
}

func TestGetRejectsMissingObject(t *testing.T) {
	s := newTestStore(t)
	missing := strings.Repeat("ab", 32)
	if _, _, err := s.Get(missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Get(missing) = %v, want does-not-exist error", err)
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Put(object.TypeBlob, []byte("soon to be damaged"))
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	path := filepath.Join(s.dir, id[:2], id[2:])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, _, err := s.Get(id); err == nil {
		t.Error("Get returned a corrupted object without error")
	}
}

func TestCopyPayloadRejectsWrongType(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Put(object.TypeBlob, []byte("i am a blob"))
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	var sink bytes.Buffer
	err = s.CopyPayload(id, object.TypeTree, &sink)
	if err == nil || !strings.Contains(err.Error(), "expected tree") {
		t.Errorf("CopyPayload with wrong type = %v, want type mismatch error", err)
	}
}

func TestGetRejectsTrailingData(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("payload")
	id := object.ID(object.TypeBlob, payload)
	writeRawObject(t, s, id, append(append([]byte(nil),
		object.Header(object.TypeBlob, int64(len(payload)))...), append(payload, "junk"...)...))

	if _, _, err := s.Get(id); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("Get = %v, want trailing-data error", err)
	}
}

func TestGetRejectsImplausibleDeclaredSize(t *testing.T) {
	s := newTestStore(t)
	id := strings.Repeat("cd", 32)
	writeRawObject(t, s, id, []byte("blob 999999999999999999\x00"))

	if _, _, err := s.Get(id); err == nil || !strings.Contains(err.Error(), "implausible") {
		t.Errorf("Get = %v, want implausible-size error", err)
	}
}

func TestFindByPrefix(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Put(object.TypeBlob, []byte("findable"))
	if err != nil {
		t.Fatalf("Put = %v", err)
	}

	matches, err := s.FindByPrefix(id[:7])
	if err != nil {
		t.Fatalf("FindByPrefix = %v", err)
	}
	if len(matches) != 1 || matches[0] != id {
		t.Errorf("FindByPrefix = %v, want [%s]", matches, id)
	}

	none, err := s.FindByPrefix("0000000")
	if err != nil {
		t.Fatalf("FindByPrefix(no match) = %v", err)
	}
	if len(none) != 0 {
		t.Errorf("FindByPrefix(no match) = %v, want empty", none)
	}

	for _, bad := range []string{"a", "xyzzyzz", strings.Repeat("a", 65)} {
		if _, err := s.FindByPrefix(bad); err == nil {
			t.Errorf("FindByPrefix(%q) accepted an invalid prefix", bad)
		}
	}

	// Java lowercases the prefix before validating, so uppercase hex matches.
	upper, err := s.FindByPrefix(strings.ToUpper(id[:7]))
	if err != nil {
		t.Fatalf("FindByPrefix(uppercase) = %v", err)
	}
	if len(upper) != 1 || upper[0] != id {
		t.Errorf("FindByPrefix(uppercase) = %v, want [%s]", upper, id)
	}
}

func TestContainsToleratesInvalidIDs(t *testing.T) {
	s := newTestStore(t)
	if s.Contains("not-an-id") {
		t.Error(`Contains("not-an-id") = true, want false`)
	}
	if s.Contains(strings.Repeat("ab", 32)) {
		t.Error("Contains(absent id) = true, want false")
	}
}

// TestV1WriteIsLegacyForm pins the on-disk FORM a format 1 store writes: a
// bare zlib stream of the canonical bytes, never a v2 container. It
// deliberately does not pin the exact compressed bytes. compress/flate does
// not promise identical output across Go releases, and the original v1
// implementation used the same package, so "the exact bytes v1 wrote" was
// never a stable value in the first place. What a v1 repository actually
// guarantees is that any zlib reader -- including the Java and C++
// implementations -- inflates the object back to these canonical bytes.
func TestV1WriteIsLegacyForm(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Put(object.TypeBlob, []byte("hello world\n"))
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	const wantID = "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d"
	if id != wantID {
		t.Fatalf("Put id = %s, want %s", id, wantID)
	}
	got, err := os.ReadFile(filepath.Join(s.dir, id[:2], id[2:]))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("stored object is empty")
	}
	// A zlib stream's CMF byte carries compression method 8 in its low
	// nibble; the v2 container magic starts with 'S', which never can.
	if got[0]&0x0f != 0x08 {
		t.Errorf("stored first byte = %#x, want a zlib CMF byte", got[0])
	}
	if bytes.HasPrefix(got, []byte("SVO2")) {
		t.Error("format 1 store wrote a v2 container object")
	}
	r, err := zlib.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("zlib.NewReader = %v", err)
	}
	defer r.Close()
	inflated, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("inflate = %v", err)
	}
	if want := []byte("blob 12\x00hello world\n"); !bytes.Equal(inflated, want) {
		t.Errorf("inflated = %q, want %q", inflated, want)
	}
}

// writeRawObject compresses canonical bytes and places them at id's shard
// path, bypassing Put so tests can plant malformed objects.
func writeRawObject(t *testing.T, s *Store, id string, canonical []byte) {
	t.Helper()
	shard := filepath.Join(s.dir, id[:2])
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(canonical); err != nil {
		t.Fatalf("zlib write = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close = %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, id[2:]), compressed.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}
