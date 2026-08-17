package repo

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatalf("Init = %v", err)
	}
	r.now = func() time.Time { return time.Unix(1700000000, 0) }
	return r
}

func write(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}

func changeStrings(changes []Change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, string(c.Type.Status())+" "+c.Path)
	}
	return out
}

func wantChanges(t *testing.T, got []Change, want ...string) {
	t.Helper()
	gotStrings := changeStrings(got)
	if len(gotStrings) != len(want) {
		t.Fatalf("changes = %v, want %v", gotStrings, want)
	}
	for i := range want {
		if gotStrings[i] != want[i] {
			t.Errorf("changes[%d] = %q, want %q", i, gotStrings[i], want[i])
		}
	}
}

func TestInitCreatesRepositoryLayout(t *testing.T) {
	r := newTestRepo(t)
	format, err := os.ReadFile(filepath.Join(r.Metadata(), "format"))
	if err != nil {
		t.Fatalf("read format: %v", err)
	}
	if string(format) != "snapvault 1\n" {
		t.Errorf("format = %q, want \"snapvault 1\\n\"", format)
	}
	head, err := os.ReadFile(filepath.Join(r.Metadata(), "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	if string(head) != "ref: refs/heads/main\n" {
		t.Errorf("HEAD = %q", head)
	}
	if _, err := Init(r.Root()); err == nil ||
		!strings.Contains(err.Error(), "already initialized") {
		t.Errorf("second Init = %v, want already-initialized error", err)
	}
}

func TestOpenFindsRepositoryFromSubdirectory(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "deep/nested/file.txt", "x")
	opened, err := Open(filepath.Join(r.Root(), "deep", "nested"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if opened.Root() != r.Root() {
		t.Errorf("Open root = %s, want %s", opened.Root(), r.Root())
	}
	if _, err := Open(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "not inside a SnapVault repository") {
		t.Errorf("Open outside = %v, want not-inside error", err)
	}
}

func TestSnapshotAdvancesHeadAndLinksParents(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "one")

	first, err := r.Snapshot("first")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	head, err := r.Head()
	if err != nil || head != first {
		t.Fatalf("Head = %s, %v; want %s", head, err, first)
	}

	write(t, r.Root(), "a.txt", "two")
	second, err := r.Snapshot("second")
	if err != nil {
		t.Fatalf("second Snapshot = %v", err)
	}
	commit, err := r.ReadCommit(second)
	if err != nil {
		t.Fatalf("ReadCommit = %v", err)
	}
	if len(commit.Parents) != 1 || commit.Parents[0] != first {
		t.Errorf("Parents = %v, want [%s]", commit.Parents, first)
	}
	if commit.Message != "second" {
		t.Errorf("Message = %q, want %q", commit.Message, "second")
	}
}

func TestSnapshotRejectsEmptyMessage(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Snapshot("   "); err == nil {
		t.Error("Snapshot accepted a blank message")
	}
}

func TestSnapshotIsDeterministicAcrossWorkerCounts(t *testing.T) {
	build := func(workers int) string {
		r, err := Init(filepath.Join(t.TempDir(), "work"))
		if err != nil {
			t.Fatalf("Init = %v", err)
		}
		r.now = func() time.Time { return time.Unix(1700000000, 0) }
		r.workers = workers
		for i := range 30 {
			write(t, r.Root(), filepath.Join("dir", strings.Repeat("s", i%5), "f"+string(rune('a'+i))),
				strings.Repeat("content", i+1))
		}
		id, err := r.Snapshot("fixed")
		if err != nil {
			t.Fatalf("Snapshot(workers=%d) = %v", workers, err)
		}
		commit, err := r.ReadCommit(id)
		if err != nil {
			t.Fatalf("ReadCommit = %v", err)
		}
		return commit.TreeID
	}
	if one, eight := build(1), build(8); one != eight {
		t.Errorf("tree id differs across worker counts: %s vs %s", one, eight)
	}
}

func TestResolveCommitRevisions(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "1")
	first, err := r.Snapshot("first")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "f", "2")
	second, err := r.Snapshot("second")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	cases := map[string]string{
		"HEAD":      second,
		"@":         second,
		"HEAD~":     first,
		"HEAD~1":    first,
		"HEAD~1~0":  first,
		second:      second,
		second[:7]:  second,
		second[:12]: second,
	}
	for revision, want := range cases {
		got, err := r.ResolveCommit(revision)
		if err != nil {
			t.Errorf("ResolveCommit(%q) = %v", revision, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveCommit(%q) = %s, want %s", revision, got, want)
		}
	}
	// With two commits, ~2 in any spelling walks beyond the first snapshot.
	for _, revision := range []string{"HEAD~2", "HEAD~1~1", "HEAD~99"} {
		if _, err := r.ResolveCommit(revision); err == nil ||
			!strings.Contains(err.Error(), "beyond the beginning of history") {
			t.Errorf("ResolveCommit(%q) = %v, want beyond-history error", revision, err)
		}
	}
	if _, err := r.ResolveCommit(second[:6]); err == nil ||
		!strings.Contains(err.Error(), "at least 7") {
		t.Errorf("short prefix = %v, want at-least-7 error", err)
	}
	if _, err := r.ResolveCommit(strings.Repeat("0", 7)); err == nil ||
		!strings.Contains(err.Error(), "unknown snapshot") {
		t.Errorf("unknown prefix = %v, want unknown-snapshot error", err)
	}
	if _, err := r.ResolveCommit("~1"); err == nil {
		t.Error("ResolveCommit accepted a revision with no starting snapshot")
	}
}

func TestHistoryWalksParentsNewestFirst(t *testing.T) {
	r := newTestRepo(t)
	var ids []string
	for i := range 3 {
		write(t, r.Root(), "f", strings.Repeat("x", i+1))
		id, err := r.Snapshot("snap")
		if err != nil {
			t.Fatalf("Snapshot = %v", err)
		}
		ids = append(ids, id)
	}

	history, err := r.History("HEAD", 50)
	if err != nil {
		t.Fatalf("History = %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("History returned %d commits, want 3", len(history))
	}
	for i, info := range history {
		if want := ids[len(ids)-1-i]; info.ID != want {
			t.Errorf("history[%d] = %s, want %s", i, info.ID, want)
		}
	}

	limited, err := r.History("HEAD", 2)
	if err != nil {
		t.Fatalf("History(limit=2) = %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("History(limit=2) returned %d commits", len(limited))
	}
	if _, err := r.History("HEAD", 0); err == nil {
		t.Error("History accepted a non-positive limit")
	}
}

func TestDiffWorkingFromHeadReportsEachChangeKind(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "keep.txt", "same")
	write(t, r.Root(), "modify.txt", "before")
	write(t, r.Root(), "delete.txt", "doomed")
	write(t, r.Root(), "becomes-dir", "flat")
	if _, err := r.Snapshot("base"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	write(t, r.Root(), "modify.txt", "after")
	if err := os.Remove(filepath.Join(r.Root(), "delete.txt")); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	write(t, r.Root(), "add.txt", "new")
	if err := os.Remove(filepath.Join(r.Root(), "becomes-dir")); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	write(t, r.Root(), "becomes-dir/inner.txt", "nested")

	changes, err := r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes,
		"A add.txt",
		"D becomes-dir",
		"A becomes-dir/inner.txt",
		"D delete.txt",
		"M modify.txt",
	)
}

func TestDiffReportsExecutableBitAsModification(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "tool.sh", "#!/bin/sh\n")
	if _, err := r.Snapshot("plain"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if err := os.Chmod(filepath.Join(r.Root(), "tool.sh"), 0o755); err != nil {
		t.Fatalf("Chmod = %v", err)
	}
	changes, err := r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes, "M tool.sh")
}

func TestDiffReportsTypeChanges(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "entry", "file for now")
	if _, err := r.Snapshot("file"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if err := os.Remove(filepath.Join(r.Root(), "entry")); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(r.Root(), "entry")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	changes, err := r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes, "T entry")
}

func TestDiffTracksEmptyDirectories(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Snapshot("empty"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	if err := os.Mkdir(filepath.Join(r.Root(), "hollow"), 0o755); err != nil {
		t.Fatalf("Mkdir = %v", err)
	}
	changes, err := r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes, "A hollow")
	if len(changes) != 1 || changes[0].Entry().Kind != object.KindDirectory {
		t.Error("empty directory change does not carry a directory entry")
	}

	if _, err := r.Snapshot("with dir"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	// The added child alone describes the difference; the directory's own
	// deletion as an empty-directory leaf is suppressed.
	write(t, r.Root(), "hollow/child.txt", "no longer empty")
	changes, err = r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes, "A hollow/child.txt")
}

func TestDiffBetweenSnapshotsAndUnbornHead(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "1")
	changes, err := r.DiffWorkingFromHead()
	if err != nil {
		t.Fatalf("unborn DiffWorkingFromHead = %v", err)
	}
	wantChanges(t, changes, "A f")

	if _, err := r.Snapshot("one"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "f", "2")
	if _, err := r.Snapshot("two"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	between, err := r.Diff("HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("Diff = %v", err)
	}
	wantChanges(t, between, "M f")
	same, err := r.Diff("HEAD", "HEAD")
	if err != nil {
		t.Fatalf("Diff(HEAD, HEAD) = %v", err)
	}
	wantChanges(t, same)
}

func TestDiffWritesNoObjects(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "content")
	if _, err := r.Snapshot("base"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "g", "more content")
	before, err := r.ObjectCount()
	if err != nil {
		t.Fatalf("ObjectCount = %v", err)
	}
	if _, err := r.DiffWorkingFromHead(); err != nil {
		t.Fatalf("DiffWorkingFromHead = %v", err)
	}
	after, err := r.ObjectCount()
	if err != nil {
		t.Fatalf("ObjectCount = %v", err)
	}
	if before != after {
		t.Errorf("diff grew the object store from %d to %d objects", before, after)
	}
}

func TestSymlinksRoundTrip(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "target.txt", "pointed at")
	if err := os.Symlink("target.txt", filepath.Join(r.Root(), "link")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := r.Snapshot("with link"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	out := filepath.Join(t.TempDir(), "out")
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	if err := r.Restore(head, out, false); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	target, err := os.Readlink(filepath.Join(out, "link"))
	if err != nil {
		t.Fatalf("Readlink = %v", err)
	}
	if target != "target.txt" {
		t.Errorf("restored link points at %q, want target.txt", target)
	}
}

func TestRestoreInPlaceRevertsAndPreservesMetadata(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "original")
	first, err := r.Snapshot("first")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "f", "changed")
	if _, err := r.Snapshot("second"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	if err := r.Restore(first, "", false); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(r.Root(), "f"))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if string(content) != "original" {
		t.Errorf("restored content = %q, want original", content)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	if head == first {
		t.Error("restore moved HEAD; it must not")
	}
	if _, err := os.Stat(filepath.Join(r.Metadata(), "format")); err != nil {
		t.Errorf("metadata was not preserved: %v", err)
	}
}

func TestRestoreRefusesDirtyWorkingTreeWithoutForce(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "snapshotted")
	head, err := r.Snapshot("base")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	write(t, r.Root(), "f", "unsnapshotted work")

	err = r.Restore(head, "", false)
	if err == nil || !strings.Contains(err.Error(), "unsnapshotted changes") {
		t.Errorf("Restore = %v, want unsnapshotted-changes refusal", err)
	}
	if err := r.Restore(head, "", true); err != nil {
		t.Errorf("forced Restore = %v", err)
	}
}

func TestRestoreToExternalTarget(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "dir/f", "exported")
	head, err := r.Snapshot("base")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	out := filepath.Join(t.TempDir(), "export")
	if err := r.Restore(head, out, false); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(out, "dir", "f"))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if string(content) != "exported" {
		t.Errorf("exported content = %q", content)
	}

	occupied := t.TempDir()
	write(t, occupied, "already-here", "x")
	if err := r.Restore(head, occupied, false); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Errorf("Restore into occupied dir = %v, want not-empty refusal", err)
	}
	if err := r.Restore(head, occupied, true); err != nil {
		t.Errorf("forced Restore into occupied dir = %v", err)
	}
}

func TestRestoreRefusesUnsafeTargets(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "x")
	head, err := r.Snapshot("base")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	inside := filepath.Join(r.Root(), "sub")
	if err := r.Restore(head, inside, false); err == nil ||
		!strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("Restore inside repo = %v, want refusal", err)
	}
	if err := r.Restore(head, r.Metadata(), false); err == nil {
		t.Error("Restore over metadata accepted")
	}
	if err := r.Restore(head, filepath.Dir(r.Root()), false); err == nil {
		t.Error("Restore over repository ancestor accepted")
	}

	linked := filepath.Join(t.TempDir(), "link-target")
	if err := os.Mkdir(linked, 0o755); err != nil {
		t.Fatalf("Mkdir = %v", err)
	}
	link := filepath.Join(t.TempDir(), "the-link")
	if err := os.Symlink(linked, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := r.Restore(head, link, false); err == nil ||
		!strings.Contains(err.Error(), "symbolic-link target") {
		t.Errorf("Restore through symlink = %v, want refusal", err)
	}
}

func TestInterruptedRestoreBlocksSnapshotAndDiff(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "f", "x")
	head, err := r.Snapshot("base")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	marker := head + "\n" + r.Root() + "\n"
	markerPath := filepath.Join(r.Metadata(), "restore-in-progress")
	if err := os.WriteFile(markerPath, []byte(marker), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	if _, err := r.Snapshot("blocked"); err == nil ||
		!strings.Contains(err.Error(), "was interrupted") {
		t.Errorf("Snapshot during interrupted restore = %v, want refusal", err)
	}
	if _, err := r.DiffWorkingFromHead(); err == nil ||
		!strings.Contains(err.Error(), "was interrupted") {
		t.Errorf("Diff during interrupted restore = %v, want refusal", err)
	}

	if err := r.Restore(head, "", true); err != nil {
		t.Fatalf("finishing Restore = %v", err)
	}
	if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
		t.Error("restore marker survived a completed restore")
	}
	if _, err := r.Snapshot("unblocked"); err != nil {
		t.Errorf("Snapshot after finished restore = %v", err)
	}
}

func TestNestedRepositoryMetadataIsSkipped(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "nested/data.txt", "captured")
	write(t, r.Root(), "nested/.snapvault/format", "snapvault 1\n")
	write(t, r.Root(), "nested/.snapvault/objects/aa/bb", "must not appear")

	if _, err := r.Snapshot("outer"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}
	out := filepath.Join(t.TempDir(), "out")
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	if err := r.Restore(head, out, false); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "nested", "data.txt")); err != nil {
		t.Errorf("nested data missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out, "nested", ".snapvault")); !os.IsNotExist(err) {
		t.Error("nested repository metadata was captured")
	}
}

func TestUnsupportedFilesystemEntriesAreRejected(t *testing.T) {
	r := newTestRepo(t)
	fifo := filepath.Join(r.Root(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	if _, err := r.Snapshot("with fifo"); err == nil ||
		!strings.Contains(err.Error(), "unsupported filesystem entry") {
		t.Errorf("Snapshot with fifo = %v, want unsupported-entry error", err)
	}
}

func TestLockExcludesConcurrentOperations(t *testing.T) {
	r := newTestRepo(t)
	lock, err := acquireLock(filepath.Join(r.Metadata(), "lock"))
	if err != nil {
		t.Fatalf("acquireLock = %v", err)
	}
	defer lock.close()

	if _, err := r.Snapshot("blocked"); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("Snapshot under held lock = %v, want already-running error", err)
	}
}
