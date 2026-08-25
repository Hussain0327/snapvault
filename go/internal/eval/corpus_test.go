package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCorpusRepoCopiesFilesAndSnapshots(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", "alpha content")
	writeFile(t, src, "nested/b.txt", "beta content")

	tmpDir, r, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	defer cleanup()

	// r.Root() has been through filepath.EvalSymlinks (repo.Init's own
	// normalization), so compare against tmpDir the same way: on macOS
	// os.MkdirTemp's default parent (/tmp) is itself a symlink to
	// /private/tmp, and a literal byte comparison would fail on that
	// platform even though both paths name the same directory.
	wantRoot, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(tmpDir) = %v", err)
	}
	if r.Root() != wantRoot {
		t.Errorf("r.Root() = %q, want the returned tmpDir %q", r.Root(), wantRoot)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "a.txt"))
	if err != nil || string(got) != "alpha content" {
		t.Errorf("a.txt in the built repo = %q, %v, want %q, nil", got, err, "alpha content")
	}
	gotNested, err := os.ReadFile(filepath.Join(tmpDir, "nested", "b.txt"))
	if err != nil || string(gotNested) != "beta content" {
		t.Errorf("nested/b.txt in the built repo = %q, %v, want %q, nil", gotNested, err, "beta content")
	}

	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	if head == "" {
		t.Fatal("Head() is empty, want a snapshot commit")
	}
	commit, err := r.ReadCommit(head)
	if err != nil {
		t.Fatalf("ReadCommit = %v", err)
	}
	if commit.Message != "eval corpus" {
		t.Errorf("commit message = %q, want %q", commit.Message, "eval corpus")
	}
}

func TestBuildCorpusRepoCleanupRemovesTempDir(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", "content")

	tmpDir, _, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	cleanup()

	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("Stat(tmpDir) after cleanup = %v, want IsNotExist", err)
	}
}

func TestBuildCorpusRepoRejectsMissingSourceDir(t *testing.T) {
	_, _, cleanup, err := BuildCorpusRepo(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("BuildCorpusRepo on a missing directory = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, want it to name the missing directory", err.Error())
	}
	// cleanup is documented nil whenever err is non-nil: every failure
	// path already removes the temporary directory itself. A caller that
	// deferred cleanup unconditionally, before checking err, would panic
	// on this nil func value.
	if cleanup != nil {
		t.Error("cleanup != nil on a BuildCorpusRepo failure, want nil per its documented contract")
	}
}

// writeFile is a small test helper shared by this package's _test.go files
// that exercise BuildCorpusRepo and Run against a real repository.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}
