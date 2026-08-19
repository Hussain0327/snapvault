package repo

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenCacheHex is one file, one record, used as a cross-language fixture.
// Java AllTests decodes the same bytes.
const goldenCacheHex = "535644430000000117979cfe362a00000000000100000005612e747874000000000000000317940f7f9163800000000000000000010000000000000002abababababababababababababababababababababababababababababababab"

func TestDirCacheRoundTripGolden(t *testing.T) {
	want, err := hex.DecodeString(goldenCacheHex)
	if err != nil {
		t.Fatalf("golden hex: %v", err)
	}
	got := encodeDirCache(1_700_000_000_000_000_000, []cacheRecord{{
		path:      "a.txt",
		size:      3,
		mtimeNano: 1_699_000_000_000_000_000,
		dev:       1,
		ino:       2,
		objectID:  "abababababababababababababababababababababababababababababababab",
	}})
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeDirCache = %x, want %x", got, want)
	}
	decoded, err := decodeDirCache(want)
	if err != nil {
		t.Fatalf("decodeDirCache = %v", err)
	}
	if decoded.writtenAt != 1_700_000_000_000_000_000 {
		t.Errorf("writtenAt = %d", decoded.writtenAt)
	}
	rec, ok := decoded.byPath["a.txt"]
	if !ok {
		t.Fatal("missing a.txt")
	}
	if rec.size != 3 || rec.dev != 1 || rec.ino != 2 {
		t.Errorf("record = %+v", rec)
	}
}

func TestSnapshotWritesAndReusesCache(t *testing.T) {
	r := newTestRepo(t)
	path := filepath.Join(r.Root(), "a.txt")
	write(t, r.Root(), "a.txt", "hello")
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("Chtimes = %v", err)
	}

	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("first Snapshot = %v", err)
	}
	cachePath := filepath.Join(r.Metadata(), cacheFileName)
	if _, err := os.Lstat(cachePath); err != nil {
		t.Fatalf("cache was not written: %v", err)
	}

	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("Chmod = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if _, err := r.Snapshot("second"); err != nil {
		t.Fatalf("second Snapshot = %v, want cache to skip hashing an unreadable unchanged file", err)
	}
}

func TestSnapshotHashesWhenContentChanges(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "one")
	first, err := r.Snapshot("first")
	if err != nil {
		t.Fatalf("first Snapshot = %v", err)
	}
	firstCommit, err := r.ReadCommit(first)
	if err != nil {
		t.Fatalf("ReadCommit = %v", err)
	}

	write(t, r.Root(), "a.txt", "two")
	second, err := r.Snapshot("second")
	if err != nil {
		t.Fatalf("second Snapshot = %v", err)
	}
	secondCommit, err := r.ReadCommit(second)
	if err != nil {
		t.Fatalf("ReadCommit second = %v", err)
	}
	if secondCommit.TreeID == firstCommit.TreeID {
		t.Fatal("changed content reused the previous tree id")
	}
}

func TestCorruptCacheIsIgnored(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "hello")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("first Snapshot = %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.Metadata(), cacheFileName), []byte("not a cache"), 0o644); err != nil {
		t.Fatalf("WriteFile cache = %v", err)
	}
	if _, err := r.Snapshot("second"); err != nil {
		t.Fatalf("Snapshot with corrupt cache = %v", err)
	}
}

func TestInPlaceRestoreDeletesCache(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "hello")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "a.txt", "dirty")
	if err := r.Restore("HEAD", "", true); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.Metadata(), cacheFileName)); !os.IsNotExist(err) {
		t.Fatalf("cache after in-place restore = %v, want not exist", err)
	}
}
