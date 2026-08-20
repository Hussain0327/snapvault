package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// buildVersionedFixture writes 30 slightly-different versions of a ~50 KB
// text file, snapshotting after each one, and returns the final working-tree
// content so a caller can check restore fidelity later.
func buildVersionedFixture(t *testing.T, r *Repository) string {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	base := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 1200) // ~54 KB
	lines := strings.Split(strings.TrimRight(base, "\n"), "\n")

	var last string
	for v := 0; v < 30; v++ {
		// Touch a handful of lines so consecutive versions stay similar
		// (delta-friendly) without being identical.
		for k := 0; k < 5; k++ {
			i := rng.Intn(len(lines))
			lines[i] = fmt.Sprintf("line %d changed in version %d - %d", i, v, rng.Int())
		}
		content := strings.Join(lines, "\n") + "\n"
		write(t, r.Root(), "big.txt", content)
		if _, err := r.Snapshot(fmt.Sprintf("version %d", v)); err != nil {
			t.Fatalf("Snapshot(version %d) = %v", v, err)
		}
		last = content
	}
	return last
}

// objectsSnapshot maps every object file's relative path to its bytes, so a
// caller can assert a repack changed (or did not change) anything on disk.
func objectsSnapshot(t *testing.T, metadata string) map[string][]byte {
	t.Helper()
	root := filepath.Join(metadata, "objects")
	out := make(map[string][]byte)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(objects) = %v", err)
	}
	for _, shard := range entries {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			t.Fatalf("ReadDir(shard) = %v", err)
		}
		for _, f := range files {
			rel := filepath.Join(shard.Name(), f.Name())
			raw, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("ReadFile(%s) = %v", rel, err)
			}
			out[rel] = raw
		}
	}
	return out
}

func totalObjectBytes(snapshot map[string][]byte) int64 {
	var total int64
	for _, raw := range snapshot {
		total += int64(len(raw))
	}
	return total
}

func TestRepackRejectsFormat1Repository(t *testing.T) {
	r := newTestRepo(t)
	write(t, r.Root(), "a.txt", "hello")
	if _, err := r.Snapshot("first"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	_, err := r.Repack(false)
	if err == nil || !strings.Contains(err.Error(), "snapvault upgrade") {
		t.Errorf("Repack on a format 1 repository = %v, want an 'upgrade' hint", err)
	}
}

func TestRepackOnEmptyRepositoryReportsNothing(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	stats, err := r.Repack(false)
	if err != nil {
		t.Fatalf("Repack = %v", err)
	}
	if stats.RewrittenObjects != 0 {
		t.Errorf("RewrittenObjects = %d, want 0", stats.RewrittenObjects)
	}
}

func TestRepackShrinksVersionedFixtureByHalf(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	finalContent := buildVersionedFixture(t, r)

	before := objectsSnapshot(t, r.Metadata())
	beforeTotal := totalObjectBytes(before)

	stats, err := r.Repack(false)
	if err != nil {
		t.Fatalf("Repack = %v", err)
	}
	if stats.RewrittenObjects == 0 {
		t.Fatal("Repack rewrote no objects, want at least one")
	}
	if stats.BeforeBytes <= stats.AfterBytes {
		t.Errorf("BeforeBytes/AfterBytes = %d/%d, want a real shrink", stats.BeforeBytes, stats.AfterBytes)
	}

	after := objectsSnapshot(t, r.Metadata())
	afterTotal := totalObjectBytes(after)
	if afterTotal > beforeTotal/2 {
		t.Errorf("total object bytes %d -> %d, want at least a 50%% reduction from %d", beforeTotal, afterTotal, beforeTotal)
	}

	// Every reachable object must still decode and verify: walk full
	// history and confirm every blob is still readable end to end.
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	verifyAllHistoryReadable(t, r, head)

	// Restoring HEAD must still reproduce the exact working tree.
	dest := t.TempDir()
	if err := r.Restore(head, dest, false); err != nil {
		t.Fatalf("Restore after repack = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "big.txt"))
	if err != nil {
		t.Fatalf("ReadFile(restored) = %v", err)
	}
	if string(got) != finalContent {
		t.Error("restored content after repack does not match the last snapshotted version")
	}

	// A second repack should find nothing left to do.
	again, err := r.Repack(false)
	if err != nil {
		t.Fatalf("second Repack = %v", err)
	}
	if again.RewrittenObjects != 0 {
		t.Errorf("second Repack rewrote %d objects, want 0 (idempotent)", again.RewrittenObjects)
	}
}

// verifyAllHistoryReadable walks every commit reachable from head and every
// blob its tree references, confirming the object store can still decode
// and integrity-check each one.
func verifyAllHistoryReadable(t *testing.T, r *Repository, head string) {
	t.Helper()
	if head == "" {
		return
	}
	history, err := r.History(head, 1_000_000)
	if err != nil {
		t.Fatalf("History = %v", err)
	}
	for _, info := range history {
		leaves, err := r.flatten(info.Commit.TreeID)
		if err != nil {
			t.Fatalf("flatten(%s) = %v", info.ID, err)
		}
		for path, entry := range leaves {
			if entry.Kind == object.KindDirectory {
				continue
			}
			typ, payload, err := r.store.Get(entry.ObjectID)
			if err != nil {
				t.Fatalf("Get(%s at %s) = %v", entry.ObjectID, path, err)
			}
			if typ != object.TypeBlob {
				t.Fatalf("Get(%s at %s) type = %v, want blob", entry.ObjectID, path, typ)
			}
			sum := sha256.Sum256(append(object.Header(typ, int64(len(payload))), payload...))
			if hex.EncodeToString(sum[:]) != entry.ObjectID {
				t.Fatalf("blob %s at %s failed a manual digest re-check", entry.ObjectID, path)
			}
		}
	}
}

func TestRepackLeavesUnreachableObjectsUntouched(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	write(t, r.Root(), "kept.txt", strings.Repeat("reachable content\n", 2000))
	if _, err := r.Snapshot("reachable"); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	// An object nothing points to: never referenced by any tree.
	orphanID, err := r.store.Put(object.TypeBlob, []byte(strings.Repeat("orphaned content\n", 2000)))
	if err != nil {
		t.Fatalf("Put(orphan) = %v", err)
	}
	orphanPath := filepath.Join(r.Metadata(), "objects", orphanID[:2], orphanID[2:])
	before, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("ReadFile(orphan before) = %v", err)
	}

	if _, err := r.Repack(false); err != nil {
		t.Fatalf("Repack = %v", err)
	}

	after, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("ReadFile(orphan after) = %v", err)
	}
	if string(before) != string(after) {
		t.Error("repack modified an unreachable object")
	}
}

// TestRepackDoesNotBuildAMutualDeltaCycle reproduces a scenario where two
// candidates' rank order flips between repacks: version 1 has a.txt and
// b.txt holding two large, near-identical texts; version 2 swaps their
// contents (an ordinary rename-like edit). A repack after each version must
// never leave the two blobs based on each other, since no implementation can
// decode a cycle.
func TestRepackDoesNotBuildAMutualDeltaCycle(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}

	base := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 400) // ~18 KB
	textX := base + "X marks the spot\n"
	textY := base + "Y marks the spot\n"

	write(t, r.Root(), "a.txt", textX)
	write(t, r.Root(), "b.txt", textY)
	if _, err := r.Snapshot("version 1"); err != nil {
		t.Fatalf("Snapshot(version 1) = %v", err)
	}
	if _, err := r.Repack(false); err != nil {
		t.Fatalf("first Repack = %v", err)
	}

	// Swap the two files' contents, exactly the kind of edit that flips
	// partitionBlobs' (nameHint, size, id) order between repacks.
	write(t, r.Root(), "a.txt", textY)
	write(t, r.Root(), "b.txt", textX)
	if _, err := r.Snapshot("version 2"); err != nil {
		t.Fatalf("Snapshot(version 2) = %v", err)
	}
	if _, err := r.Repack(false); err != nil {
		t.Fatalf("second Repack = %v", err)
	}

	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head = %v", err)
	}
	verifyAllHistoryReadable(t, r, head)

	dest := t.TempDir()
	if err := r.Restore(head, dest, false); err != nil {
		t.Fatalf("Restore after repack = %v", err)
	}
	gotA, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile(a.txt) = %v", err)
	}
	if string(gotA) != textY {
		t.Error("restored a.txt does not match the last snapshotted version")
	}
}

func TestRepackDryRunWritesNothing(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	buildVersionedFixture(t, r)

	before := objectsSnapshot(t, r.Metadata())

	stats, err := r.Repack(true)
	if err != nil {
		t.Fatalf("Repack(dryRun) = %v", err)
	}
	if stats.RewrittenObjects == 0 {
		t.Fatal("Repack(dryRun) reported no candidates, want at least one")
	}
	if stats.BeforeBytes <= stats.AfterBytes {
		t.Errorf("dry-run BeforeBytes/AfterBytes = %d/%d, want a real projected shrink",
			stats.BeforeBytes, stats.AfterBytes)
	}

	after := objectsSnapshot(t, r.Metadata())
	if len(before) != len(after) {
		t.Fatalf("dry-run changed the object count: %d -> %d", len(before), len(after))
	}
	for name, raw := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("dry-run removed object file %s", name)
		}
		if string(got) != string(raw) {
			t.Errorf("dry-run modified object file %s", name)
		}
	}
}
