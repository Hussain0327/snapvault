package repo

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAcceptsFormat2(t *testing.T) {
	r := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(r.Metadata(), "format"), []byte("snapvault 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	opened, err := Open(r.Root())
	if err != nil {
		t.Fatalf("Open(format 2) = %v, want nil error", err)
	}
	if opened.version != 2 {
		t.Errorf("version = %d, want 2", opened.version)
	}
}

func TestOpenRejectsUnknownFormat(t *testing.T) {
	r := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(r.Metadata(), "format"), []byte("snapvault 3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	_, err := Open(r.Root())
	if err == nil || !strings.Contains(err.Error(), "unsupported SnapVault repository format") {
		t.Errorf("Open(format 3) = %v, want unsupported-format error", err)
	}
}

func TestInitStartsAtFormat1(t *testing.T) {
	r := newTestRepo(t)
	if r.version != 1 {
		t.Errorf("Init version = %d, want 1", r.version)
	}
}

func TestUpgradeRewritesFormatFileAndIsIdempotent(t *testing.T) {
	r := newTestRepo(t)

	upgraded, err := r.Upgrade()
	if err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	if !upgraded {
		t.Error("first Upgrade reported no change, want upgraded")
	}
	if r.version != 2 {
		t.Errorf("version after Upgrade = %d, want 2", r.version)
	}
	format, err := os.ReadFile(filepath.Join(r.Metadata(), "format"))
	if err != nil {
		t.Fatalf("ReadFile(format) = %v", err)
	}
	if string(format) != "snapvault 2\n" {
		t.Errorf("format = %q, want %q", format, "snapvault 2\n")
	}

	reopened, err := Open(r.Root())
	if err != nil {
		t.Fatalf("Open after Upgrade = %v", err)
	}
	if reopened.version != 2 {
		t.Errorf("reopened version = %d, want 2", reopened.version)
	}

	upgraded, err = r.Upgrade()
	if err != nil {
		t.Fatalf("second Upgrade = %v", err)
	}
	if upgraded {
		t.Error("second Upgrade reported a change, want idempotent no-op")
	}
	format, err = os.ReadFile(filepath.Join(r.Metadata(), "format"))
	if err != nil {
		t.Fatalf("ReadFile(format) after second Upgrade = %v", err)
	}
	if string(format) != "snapvault 2\n" {
		t.Errorf("format after second Upgrade = %q, want unchanged %q", format, "snapvault 2\n")
	}
}

func TestSnapshotInV2RepositoryWritesContainerObjects(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}
	write(t, r.Root(), "a.txt", "container form after upgrade")

	commitID, err := r.Snapshot("after upgrade")
	if err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(r.Metadata(), "objects", commitID[:2], commitID[2:]))
	if err != nil {
		t.Fatalf("ReadFile(commit object) = %v", err)
	}
	if len(raw) < 4 || string(raw[:4]) != "SVO2" {
		t.Errorf("commit object does not start with the SVO2 magic: %#v", raw[:min(4, len(raw))])
	}

	// The whole pipeline still has to work end to end: restoring a v2
	// snapshot must reproduce the working tree exactly.
	dest := t.TempDir()
	if err := r.Restore(commitID, dest, false); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile(restored) = %v", err)
	}
	if !bytes.Equal(got, []byte("container form after upgrade")) {
		t.Errorf("restored content = %q, want %q", got, "container form after upgrade")
	}
}

func TestUpgradeOnFormat2RepositoryOpenedFreshIsIdempotent(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.Upgrade(); err != nil {
		t.Fatalf("Upgrade = %v", err)
	}

	reopened, err := Open(r.Root())
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	upgraded, err := reopened.Upgrade()
	if err != nil {
		t.Fatalf("Upgrade on a freshly opened format 2 repository = %v", err)
	}
	if upgraded {
		t.Error("Upgrade on an already-format-2 repository reported a change")
	}
}
