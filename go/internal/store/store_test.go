package store

import (
	"bytes"
	"compress/zlib"
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

// TestV1WriteIsByteIdentical pins the exact bytes a format 1 store writes
// for a known payload, captured from this package before format v2 existed.
// A store's format defaults to FormatV1, so this exercises the same code
// path a v1 repository always has, and must never change.
func TestV1WriteIsByteIdentical(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Put(object.TypeBlob, []byte("hello world\n"))
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	const wantID = "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d"
	if id != wantID {
		t.Fatalf("Put id = %s, want %s", id, wantID)
	}
	wantBytes := []byte{
		0x78, 0x9c, 0x4a, 0xca, 0xc9, 0x4f, 0x52, 0x30, 0x34, 0x62, 0xc8, 0x48,
		0xcd, 0xc9, 0xc9, 0x57, 0x28, 0xcf, 0x2f, 0xca, 0x49, 0xe1, 0x02, 0x04,
		0x00, 0x00, 0xff, 0xff, 0x44, 0x11, 0x06, 0x89,
	}
	got, err := os.ReadFile(filepath.Join(s.dir, id[:2], id[2:]))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Errorf("stored bytes = %#v, want %#v", got, wantBytes)
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
