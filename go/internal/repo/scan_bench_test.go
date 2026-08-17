package repo

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// BenchmarkWorkingTreeHash measures the concurrent hashing pipeline against
// the same scan pinned to one worker: 240 files of 128 KiB across nested
// directories, hashed without storing, exactly as diff does.
func BenchmarkWorkingTreeHash(b *testing.B) {
	const (
		fileCount = 240
		fileSize  = 128 * 1024
	)
	r, err := Init(filepath.Join(b.TempDir(), "work"))
	if err != nil {
		b.Fatalf("Init = %v", err)
	}
	r.now = func() time.Time { return time.Unix(1700000000, 0) }
	random := rand.New(rand.NewSource(42))
	content := make([]byte, fileSize)
	for i := range fileCount {
		random.Read(content)
		path := filepath.Join(r.Root(), fmt.Sprintf("dir%02d", i%16), fmt.Sprintf("file%03d", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatalf("MkdirAll = %v", err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			b.Fatalf("WriteFile = %v", err)
		}
	}

	for _, workers := range []int{1, runtime.NumCPU()} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			r.workers = workers
			b.SetBytes(fileCount * fileSize)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := r.scanTree(hashingSink{}); err != nil {
					b.Fatalf("scanTree = %v", err)
				}
			}
		})
	}
}
