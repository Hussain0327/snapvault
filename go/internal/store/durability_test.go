package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

// recordSyncs replaces the fsync hooks for one test and returns the log of
// synced paths, each prefixed with "file:" or "dir:".
func recordSyncs(t *testing.T) *[]string {
	t.Helper()
	var log []string
	oldFile, oldDir := syncFile, syncDir
	syncFile = func(f *os.File) error {
		log = append(log, "file:"+f.Name())
		return nil
	}
	syncDir = func(path string) error {
		log = append(log, "dir:"+path)
		return nil
	}
	t.Cleanup(func() { syncFile, syncDir = oldFile, oldDir })
	return &log
}

func TestNewObjectsAreSyncedBeforeAndAfterRename(t *testing.T) {
	for _, format := range []Format{FormatV1, FormatV2} {
		s := newTestStore(t)
		s.SetFormat(format)
		log := recordSyncs(t)

		id, err := s.Put(object.TypeBlob, []byte("durable\n"))
		if err != nil {
			t.Fatalf("format %d: Put = %v", format, err)
		}
		destination, err := s.pathFor(id)
		if err != nil {
			t.Fatal(err)
		}

		if len(*log) < 2 {
			t.Fatalf("format %d: syncs = %v, want a file sync then a directory sync", format, *log)
		}
		first := (*log)[0]
		if !strings.HasPrefix(first, "file:") || !strings.Contains(first, "tmp-") {
			t.Errorf("format %d: first sync = %q, want the temporary file", format, first)
		}
		if want := "dir:" + filepath.Dir(destination); !contains(*log, want) {
			t.Errorf("format %d: syncs = %v, want %q after the rename", format, *log, want)
		}
	}
}

func TestDuplicatePutSkipsDirectorySync(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(object.TypeBlob, []byte("same\n")); err != nil {
		t.Fatal(err)
	}
	log := recordSyncs(t)
	if _, err := s.Put(object.TypeBlob, []byte("same\n")); err != nil {
		t.Fatal(err)
	}
	for _, entry := range *log {
		if strings.HasPrefix(entry, "dir:") {
			t.Errorf("deduplicated Put synced a directory: %v", *log)
		}
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
