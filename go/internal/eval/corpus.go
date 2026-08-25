package eval

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Hussain0327/snapvault/go/internal/repo"
)

// evalCorpusMessage is the snapshot message BuildCorpusRepo commits with,
// pinned as a constant so golden_test.go and the CLI's "eval run --corpus"
// path always produce the same commit regardless of which one calls it.
const evalCorpusMessage = "eval corpus"

// BuildCorpusRepo copies dir's regular files and directories into a new
// temporary directory, initializes a SnapVault repository there, and
// snapshots it with the message "eval corpus". Both "eval run --corpus" and
// the golden tests (TestGoldenFloorLexical, TestGoldenFloorStatic) call this
// so they build their repository the same way.
//
// The caller still owns indexing: BuildCorpusRepo only walks dir, inits, and
// snapshots, so the caller can pick whichever embedder and Pipeline it wants
// to index with before ranking.
//
// cleanup removes the temporary directory. It is nil whenever err is
// non-nil: every failure path already removes the temporary directory
// itself before returning, so the caller must check err before deferring
// cleanup — deferring it unconditionally panics on a nil func when err is
// set.
func BuildCorpusRepo(dir string) (tmpDir string, r *repo.Repository, cleanup func(), err error) {
	tmpDir, err = os.MkdirTemp("", "snapvault-eval-corpus-*")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup = func() { os.RemoveAll(tmpDir) }

	if err := copyTree(dir, tmpDir); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("copying corpus %s: %w", dir, err)
	}
	r, err = repo.Init(tmpDir)
	if err != nil {
		cleanup()
		return "", nil, nil, err
	}
	if _, err := r.Snapshot(evalCorpusMessage); err != nil {
		cleanup()
		return "", nil, nil, err
	}
	return tmpDir, r, cleanup, nil
}

// copyTree copies every regular file and directory under src into dst,
// preserving relative paths. Anything that is neither a regular file nor a
// directory (a symlink, device, or similar) is skipped rather than copied,
// since the golden and generated corpora this feeds are plain text trees.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0o644)
		default:
			return nil
		}
	})
}
