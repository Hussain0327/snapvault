package model2vec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// modelDirEnv, when set, overrides where Dir looks for and Pull installs
// models, so a user or CI job can point SnapVault at a shared cache without
// touching the OS default.
const modelDirEnv = "SNAPVAULT_MODEL_DIR"

// defaultBaseURL is the model host Pull downloads from when the caller does
// not supply one.
const defaultBaseURL = "https://huggingface.co"

// FileSpec pins one file of a model to its expected size and content hash.
type FileSpec struct {
	Name   string
	SHA256 string
	Size   int64
}

// Spec identifies one registered model: where to download it from, which
// revision is pinned, and the files that make it up.
type Spec struct {
	Name     string
	Repo     string
	Revision string
	Dim      int
	Files    []FileSpec
}

// Registry lists every model SnapVault knows how to download, keyed by
// Spec.Name.
var Registry = map[string]Spec{
	"potion-base-8M": {
		Name:     "potion-base-8M",
		Repo:     "minishlab/potion-base-8M",
		Revision: "bf8b056651a2c21b8d2565580b8569da283cab23",
		Dim:      256,
		Files: []FileSpec{
			{
				Name:   "config.json",
				SHA256: "2a6ac0e9aaa356a68a5688070db78fc3a464fefe85d2f06a1905ce3718687553",
				Size:   202,
			},
			{
				Name:   "tokenizer.json",
				SHA256: "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50",
				Size:   683666,
			},
			{
				Name:   "model.safetensors",
				SHA256: "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2",
				Size:   30236760,
			},
		},
	},
}

// Dir returns the directory a named model is installed into or downloaded
// to: $SNAPVAULT_MODEL_DIR/<name> when that environment variable is set and
// non-empty, otherwise os.UserCacheDir()/snapvault/models/<name>.
func Dir(name string) (string, error) {
	if base := os.Getenv(modelDirEnv); base != "" {
		return filepath.Join(base, name), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "snapvault", "models", name), nil
}

// pullHTTPClient is used for every model file download. It carries a
// generous but finite timeout so a server that accepts the connection and
// then stalls cannot hang `snapvault model pull` forever: the caller's
// context (cli.go passes context.Background(), which carries no deadline of
// its own) is not enough on its own to bound a stalled response.
var pullHTTPClient = &http.Client{Timeout: 10 * time.Minute}

// stagedFile is one file Pull has downloaded and verified but not yet
// installed: its content sits at tmpPath, in dir, until every file in the
// Spec has passed verification.
type stagedFile struct {
	tmpPath, finalPath, name string
}

// Pull downloads every file of spec from baseURL (defaultBaseURL when
// empty) as <repo>/resolve/<revision>/<file> into dir. Every file first
// streams to its own temporary file beside its destination and is verified
// by SHA-256 and size; only once every file in spec has verified does Pull
// rename them all into place. A failure at any point — network, size,
// hash, or an earlier file in the same Spec — removes every temporary file
// this call staged, so a multi-file Spec never leaves a partial install
// (an old file beside a newly verified one) that a later Load could
// silently pair up wrong. progress, when non-nil, receives one line per
// file attempted.
func Pull(ctx context.Context, spec Spec, baseURL, dir string, progress io.Writer) error {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating model directory: %w", err)
	}

	var staged []stagedFile
	cleanup := func() {
		for _, s := range staged {
			os.Remove(s.tmpPath)
		}
	}

	for _, f := range spec.Files {
		s, err := pullFile(ctx, spec, f, baseURL, dir, progress)
		if err != nil {
			cleanup()
			return err
		}
		staged = append(staged, s)
	}

	for _, s := range staged {
		if err := os.Rename(s.tmpPath, s.finalPath); err != nil {
			cleanup()
			return fmt.Errorf("installing %s: %w", s.name, err)
		}
	}
	return nil
}

// pullFile downloads and verifies one file of spec, leaving its content at
// a temporary path inside dir without installing it: Pull only renames a
// file into place once every file in the Spec has verified. Its own
// temporary file is removed before it returns any error.
func pullFile(ctx context.Context, spec Spec, f FileSpec, baseURL, dir string, progress io.Writer) (stagedFile, error) {
	// f.Name becomes a path element below; reject anything that is not a
	// simple base name so a Spec (today only the hardcoded Registry, but
	// Pull is an exported API with no other constraint on Name) cannot
	// write outside dir via a name like "../escaped.json".
	if f.Name == "" || f.Name != filepath.Base(f.Name) || strings.ContainsAny(f.Name, `/\`) {
		return stagedFile{}, fmt.Errorf("invalid model file name %q", f.Name)
	}
	if f.Size <= 0 {
		return stagedFile{}, fmt.Errorf("model file %s has a non-positive size %d in its spec", f.Name, f.Size)
	}

	url := fmt.Sprintf("%s/%s/resolve/%s/%s", baseURL, spec.Repo, spec.Revision, f.Name)
	if progress != nil {
		fmt.Fprintf(progress, "pulling %s\n", f.Name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return stagedFile{}, fmt.Errorf("downloading %s: %w", f.Name, err)
	}
	resp, err := pullHTTPClient.Do(req)
	if err != nil {
		return stagedFile{}, fmt.Errorf("downloading %s: %w", f.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return stagedFile{}, fmt.Errorf("downloading %s: server returned %s", f.Name, resp.Status)
	}

	finalPath := filepath.Join(dir, f.Name)
	tmp, err := os.CreateTemp(dir, ".model-*.tmp")
	if err != nil {
		return stagedFile{}, fmt.Errorf("downloading %s: %w", f.Name, err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	// f.Size is validated positive above, so this also bounds how much a
	// stalling or over-long response body can write to disk before the
	// size check below ever runs: the pinned size is known up front and
	// there is no reason to trust the server past it.
	body := io.LimitReader(resp.Body, f.Size+1)
	size, copyErr := io.Copy(io.MultiWriter(tmp, hasher), body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return stagedFile{}, fmt.Errorf("downloading %s: %w", f.Name, copyErr)
	}
	if closeErr != nil {
		return stagedFile{}, fmt.Errorf("downloading %s: %w", f.Name, closeErr)
	}
	if size != f.Size {
		return stagedFile{}, fmt.Errorf("downloaded %s is %d bytes, want %d", f.Name, size, f.Size)
	}
	gotSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA256 != f.SHA256 {
		return stagedFile{}, fmt.Errorf("downloaded %s has sha256 %s, want %s", f.Name, gotSHA256, f.SHA256)
	}

	removeTmp = false
	if progress != nil {
		fmt.Fprintf(progress, "verified %s (%d bytes)\n", f.Name, size)
	}
	return stagedFile{tmpPath: tmpPath, finalPath: finalPath, name: f.Name}, nil
}

// Verify checks that every file of spec exists in dir with the pinned size
// and SHA-256.
func Verify(spec Spec, dir string) error {
	for _, f := range spec.Files {
		path := filepath.Join(dir, f.Name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("verifying %s: %w", f.Name, err)
		}
		if int64(len(data)) != f.Size {
			return fmt.Errorf("%s is %d bytes, want %d", f.Name, len(data), f.Size)
		}
		sum := sha256.Sum256(data)
		gotSHA256 := hex.EncodeToString(sum[:])
		if gotSHA256 != f.SHA256 {
			return fmt.Errorf("%s has sha256 %s, want %s", f.Name, gotSHA256, f.SHA256)
		}
	}
	return nil
}
