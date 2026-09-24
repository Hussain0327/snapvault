package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDetachedKeepsMetadataOutsideTheWorkTree(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	storeDir := filepath.Join(base, "stores", "work")
	write(t, work, "notes.txt", "hello\n")

	r, err := OpenDetached(work, storeDir)
	if err != nil {
		t.Fatalf("OpenDetached = %v", err)
	}
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(work, MetadataDirName)); !os.IsNotExist(err) {
		t.Errorf("work tree gained %s (err = %v); a detached store must stay outside it", MetadataDirName, err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "objects")); err != nil {
		t.Errorf("store has no objects directory: %v", err)
	}

	reopened, err := OpenDetached(work, storeDir)
	if err != nil {
		t.Fatalf("reopen = %v", err)
	}
	history, err := reopened.History("HEAD", 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("History = %v, %v; want one snapshot", history, err)
	}
	if changes, err := reopened.DiffWorkingFromHead(); err != nil || len(changes) != 0 {
		t.Errorf("DiffWorkingFromHead = %v, %v; want no changes", changes, err)
	}
}

func TestOpenDetachedRejectsAStoreForAnotherFolder(t *testing.T) {
	base := t.TempDir()
	storeDir := filepath.Join(base, "store")
	if _, err := OpenDetached(filepath.Join(base, "a"), storeDir); err != nil {
		t.Fatalf("OpenDetached(a) = %v", err)
	}
	_, err := OpenDetached(filepath.Join(base, "b"), storeDir)
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Errorf("OpenDetached(b) = %v, want a 'belongs to' error", err)
	}
}

func TestOpenDetachedRejectsAStoreInsideTheWorkTree(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	_, err := OpenDetached(work, filepath.Join(work, "checkpoints"))
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("OpenDetached = %v, want an error saying the store must be outside the folder", err)
	}
}

func TestSnapshotIfChangedSkipsAnUnchangedTree(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "one\n")

	first, created, err := r.SnapshotIfChanged("auto")
	if err != nil || !created {
		t.Fatalf("first SnapshotIfChanged = %s, %v, %v; want a new commit", first, created, err)
	}
	again, created, err := r.SnapshotIfChanged("auto")
	if err != nil || created || again != first {
		t.Fatalf("unchanged SnapshotIfChanged = %s, %v, %v; want %s, false", again, created, err, first)
	}

	write(t, r.Root(), "a.txt", "two\n")
	third, created, err := r.SnapshotIfChanged("auto")
	if err != nil || !created || third == first {
		t.Fatalf("changed SnapshotIfChanged = %s, %v, %v; want a new commit", third, created, err)
	}
	history, err := r.History("HEAD", 10)
	if err != nil || len(history) != 2 {
		t.Errorf("History = %d entries, %v; want 2", len(history), err)
	}
}

func TestOpenStoreUsesTheRecordedFolder(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	storeDir := filepath.Join(base, "store")
	write(t, work, "a.txt", "a\n")
	if _, err := OpenDetached(work, storeDir); err != nil {
		t.Fatal(err)
	}

	r, err := OpenStore(storeDir, "")
	if err != nil {
		t.Fatalf("OpenStore = %v", err)
	}
	if want, _ := filepath.EvalSymlinks(work); r.Root() != want {
		t.Errorf("Root = %s, want %s", r.Root(), want)
	}
	if _, err := OpenStore(storeDir, filepath.Join(base, "elsewhere")); err == nil {
		t.Error("OpenStore with a different folder succeeded")
	}
	if _, err := OpenStore(filepath.Join(base, "missing"), ""); err == nil || !strings.Contains(err.Error(), "not a checkpoint store") {
		t.Errorf("OpenStore(missing) = %v, want a not-a-checkpoint-store error", err)
	}
}
