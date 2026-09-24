package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, root string, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", rel, err)
	}
	return string(data)
}

func TestRestorePathsRevertsOnlyTheNamedPaths(t *testing.T) {
	r := newTestRepo(t)
	root := r.Root()
	write(t, root, "keep.txt", "original\n")
	write(t, root, "edit.txt", "original\n")
	write(t, root, "docs/a.md", "a\n")
	write(t, root, "docs/b.md", "b\n")
	checkpoint, err := r.Snapshot("before agent")
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, "keep.txt", "human edit\n")
	write(t, root, "edit.txt", "agent edit\n")
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "new.txt", "agent created\n")

	if err := r.RestorePaths(checkpoint, []string{"edit.txt", "docs", "new.txt"}); err != nil {
		t.Fatalf("RestorePaths = %v", err)
	}
	if got := readFile(t, root, "edit.txt"); got != "original\n" {
		t.Errorf("edit.txt = %q, want the checkpoint's content", got)
	}
	if got := readFile(t, root, "docs/b.md"); got != "b\n" {
		t.Errorf("docs/b.md = %q, want it restored", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt still exists (err = %v); it was absent at the checkpoint", err)
	}
	if got := readFile(t, root, "keep.txt"); got != "human edit\n" {
		t.Errorf("keep.txt = %q; an unnamed path must be left alone", got)
	}
}

func TestRestorePathsRejectsPathsOutsideTheFolder(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "a\n")
	checkpoint, err := r.Snapshot("one")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/etc/passwd", "", ".", "a/../../x", MetadataDirName + "/HEAD"} {
		if err := r.RestorePaths(checkpoint, []string{bad}); err == nil {
			t.Errorf("RestorePaths(%q) succeeded, want a refusal", bad)
		}
	}
}

func TestRestorePathsChecksEveryPathBeforeChangingAny(t *testing.T) {
	r := newTestRepo(t)
	root := r.Root()
	write(t, root, "a.txt", "a\n")
	checkpoint, err := r.Snapshot("one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "changed\n")

	err = r.RestorePaths(checkpoint, []string{"a.txt", "../escape"})
	if err == nil {
		t.Fatal("RestorePaths succeeded with an invalid path in the list")
	}
	if got := readFile(t, root, "a.txt"); got != "changed\n" {
		t.Errorf("a.txt = %q; nothing may change when any path is invalid", got)
	}
}

func TestRestorePathsRefusesToWriteThroughASymlinkedDirectory(t *testing.T) {
	r := newTestRepo(t)
	root := r.Root()
	write(t, root, "dir/a.txt", "a\n")
	checkpoint, err := r.Snapshot("one")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}

	err = r.RestorePaths(checkpoint, []string{"dir/a.txt"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("RestorePaths = %v, want a symbolic-link refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("restore wrote outside the folder (err = %v)", err)
	}
}
