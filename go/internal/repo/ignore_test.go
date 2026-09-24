package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newIgnoringStore(t *testing.T, names ...string) *Repository {
	t.Helper()
	base := t.TempDir()
	r, err := OpenDetached(filepath.Join(base, "work"), filepath.Join(base, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetIgnore(names); err != nil {
		t.Fatalf("SetIgnore = %v", err)
	}
	return r
}

func TestIgnoredNamesAreNeitherSnapshottedNorRestoredAway(t *testing.T) {
	r := newIgnoringStore(t, "node_modules")
	root := r.Root()
	write(t, root, "src/main.js", "v1\n")
	write(t, root, "node_modules/dep/index.js", "dep\n")
	write(t, root, "src/node_modules/local.js", "local\n")
	checkpoint, err := r.Snapshot("one")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := r.Diff(checkpoint, checkpoint)
	if err != nil || len(changes) != 0 {
		t.Fatalf("self diff = %v, %v", changes, err)
	}
	commit, _ := r.ReadCommit(checkpoint)
	if entry, _ := r.lookupEntry(commit.TreeID, "node_modules"); entry != nil {
		t.Errorf("node_modules was snapshotted")
	}
	if entry, _ := r.lookupEntry(commit.TreeID, "src/node_modules"); entry != nil {
		t.Errorf("src/node_modules was snapshotted")
	}

	write(t, root, "src/main.js", "v2\n")
	write(t, root, "node_modules/dep/index.js", "dep upgraded\n")
	if dirty, err := r.isWorkingTreeDirty(); err != nil || !dirty {
		t.Fatalf("dirty = %v, %v; the src edit should count", dirty, err)
	}

	if err := r.Restore(checkpoint, "", true); err != nil {
		t.Fatalf("whole-folder Restore = %v", err)
	}
	if got := readFile(t, root, "src/main.js"); got != "v1\n" {
		t.Errorf("src/main.js = %q", got)
	}
	if got := readFile(t, root, "node_modules/dep/index.js"); got != "dep upgraded\n" {
		t.Errorf("ignored node_modules was changed by a restore: %q", got)
	}
	if got := readFile(t, root, "src/node_modules/local.js"); got != "local\n" {
		t.Errorf("nested ignored directory was changed by a restore: %q", got)
	}

	if err := r.RestorePaths(checkpoint, []string{"src"}); err != nil {
		t.Fatalf("RestorePaths(src) = %v", err)
	}
	if got := readFile(t, root, "src/node_modules/local.js"); got != "local\n" {
		t.Errorf("RestorePaths(src) removed an ignored child: %q", got)
	}
	err = r.RestorePaths(checkpoint, []string{"node_modules/dep"})
	if err == nil || !strings.Contains(err.Error(), "ignored") {
		t.Errorf("RestorePaths into an ignored path = %v, want a refusal", err)
	}
}

func TestIgnoreListPersistsAndIsOnlyForCheckpointStores(t *testing.T) {
	r := newIgnoringStore(t, "node_modules", ".git")
	reopened, err := OpenStore(r.Metadata(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.ignored("node_modules") || !reopened.ignored(".git") || reopened.ignored("src") {
		t.Errorf("reopened ignore set = %v", reopened.ignore)
	}

	for _, bad := range []string{"", "a/b", "..", "."} {
		if err := r.SetIgnore([]string{bad}); err == nil {
			t.Errorf("SetIgnore(%q) succeeded", bad)
		}
	}
	plain := newTestRepo(t)
	if err := plain.SetIgnore([]string{"node_modules"}); err == nil {
		t.Error("SetIgnore on an ordinary repository succeeded; it would break cross-language tree ids")
	}
	if _, err := os.Stat(filepath.Join(plain.Metadata(), ignoreFile)); !os.IsNotExist(err) {
		t.Errorf("ordinary repository gained an ignore file (err = %v)", err)
	}
}
