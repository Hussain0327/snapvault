package model2vec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirHonoursSnapvaultModelDirEnv(t *testing.T) {
	t.Setenv(modelDirEnv, "/custom/models")
	got, err := Dir("potion-base-8M")
	if err != nil {
		t.Fatalf("Dir = %v", err)
	}
	if want := filepath.Join("/custom/models", "potion-base-8M"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestDirFallsBackToUserCacheDir(t *testing.T) {
	t.Setenv(modelDirEnv, "")
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("os.UserCacheDir unavailable: %v", err)
	}
	got, err := Dir("potion-base-8M")
	if err != nil {
		t.Fatalf("Dir = %v", err)
	}
	want := filepath.Join(cacheDir, "snapvault", "models", "potion-base-8M")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

// testFileServer serves the given file contents at /<repo>/resolve/<revision>/<name>.
func testFileServer(t *testing.T, spec Spec, contents map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for name, data := range contents {
		data := data
		mux.HandleFunc("/"+spec.Repo+"/resolve/"+spec.Revision+"/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Write(data)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func specForContents(contents map[string][]byte) Spec {
	spec := Spec{
		Name:     "tiny",
		Repo:     "acme/tiny",
		Revision: "deadbeef",
		Dim:      2,
	}
	for name, data := range contents {
		sum := sha256.Sum256(data)
		spec.Files = append(spec.Files, FileSpec{
			Name:   name,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		})
	}
	return spec
}

func TestPullDownloadsAndVerifies(t *testing.T) {
	contents := map[string][]byte{
		"config.json":    []byte(`{"normalize":true,"hidden_dim":2}`),
		"tokenizer.json": []byte(`{"model":{"vocab":{}}}`),
	}
	spec := specForContents(contents)
	srv := testFileServer(t, spec, contents)

	dir := t.TempDir()
	if err := Pull(context.Background(), spec, srv.URL, dir, nil); err != nil {
		t.Fatalf("Pull = %v", err)
	}

	for name, want := range contents {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading pulled %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("pulled %s = %q, want %q", name, got, want)
		}
	}
	if err := Verify(spec, dir); err != nil {
		t.Errorf("Verify after Pull = %v", err)
	}

	// No .tmp files should survive a successful pull.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file after successful Pull: %s", e.Name())
		}
	}
}

func TestPullWrongHashLeavesNothingBehind(t *testing.T) {
	realContents := []byte(`{"normalize":true}`)
	spec := specForContents(map[string][]byte{"config.json": realContents})
	// Corrupt the pinned hash so the server's real bytes never match it.
	spec.Files[0].SHA256 = strings.Repeat("0", 64)

	srv := testFileServer(t, spec, map[string][]byte{"config.json": realContents})

	dir := t.TempDir()
	err := Pull(context.Background(), spec, srv.URL, dir, nil)
	if err == nil {
		t.Fatal("Pull with a wrong pinned hash succeeded, want an error")
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("reading dir: %v", readErr)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir has %d leftover entries after a wrong-hash Pull failure: %v", len(entries), names)
	}
}

// TestPullMultiFileFailureLeavesNothingBehind checks the case
// TestPullWrongHashLeavesNothingBehind's single-file Spec cannot catch:
// when a later file in a multi-file Spec fails verification, an earlier
// file that already verified must not be left installed in dir.
func TestPullMultiFileFailureLeavesNothingBehind(t *testing.T) {
	configContents := []byte(`{"normalize":true}`)
	tokenizerContents := []byte(`{"model":{"vocab":{}}}`)
	spec := specForContents(map[string][]byte{
		"config.json":    configContents,
		"tokenizer.json": tokenizerContents,
	})
	// Corrupt only tokenizer.json's pinned hash, so config.json (whichever
	// order Files happens to hold it) verifies while tokenizer.json fails.
	for i, f := range spec.Files {
		if f.Name == "tokenizer.json" {
			spec.Files[i].SHA256 = strings.Repeat("0", 64)
		}
	}

	srv := testFileServer(t, spec, map[string][]byte{
		"config.json":    configContents,
		"tokenizer.json": tokenizerContents,
	})

	dir := t.TempDir()
	if err := Pull(context.Background(), spec, srv.URL, dir, nil); err == nil {
		t.Fatal("Pull with one file's hash wrong succeeded, want an error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir has %d leftover entries after a multi-file Pull failure: %v (config.json must not be "+
			"left installed just because it happened to verify before tokenizer.json failed)", len(entries), names)
	}
}

// TestPullRejectsPathTraversalInFileName checks that a FileSpec.Name
// containing ".." cannot write outside dir.
func TestPullRejectsPathTraversalInFileName(t *testing.T) {
	contents := []byte(`{"evil":true}`)
	sum := sha256.Sum256(contents)
	spec := Spec{
		Name: "tiny", Repo: "acme/tiny", Revision: "deadbeef", Dim: 2,
		Files: []FileSpec{{Name: "../escaped.json", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(contents))}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/acme/tiny/resolve/deadbeef/../escaped.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(contents)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	root := t.TempDir()
	modelsDir := filepath.Join(root, "models")
	if err := Pull(context.Background(), spec, srv.URL, modelsDir, nil); err == nil {
		t.Fatal("Pull with a path-traversing file name succeeded, want an error")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.json")); err == nil {
		t.Error("Pull wrote a file outside its model directory via a path-traversing name")
	}
}

func TestPullWrongSizeFails(t *testing.T) {
	realContents := []byte(`{"normalize":true}`)
	spec := specForContents(map[string][]byte{"config.json": realContents})
	spec.Files[0].Size = int64(len(realContents)) + 1

	srv := testFileServer(t, spec, map[string][]byte{"config.json": realContents})

	dir := t.TempDir()
	if err := Pull(context.Background(), spec, srv.URL, dir, nil); err == nil {
		t.Error("Pull with a wrong pinned size succeeded, want an error")
	}
}

func TestPullServerErrorFails(t *testing.T) {
	spec := specForContents(map[string][]byte{"config.json": []byte("{}")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	if err := Pull(context.Background(), spec, srv.URL, dir, nil); err == nil {
		t.Error("Pull against a 404 server succeeded, want an error")
	}
}

func TestVerifyMissingFile(t *testing.T) {
	spec := specForContents(map[string][]byte{"config.json": []byte("{}")})
	if err := Verify(spec, t.TempDir()); err == nil {
		t.Error("Verify on an empty directory succeeded, want an error")
	}
}

func TestVerifyCorruptedFile(t *testing.T) {
	contents := []byte(`{"normalize":true}`)
	spec := specForContents(map[string][]byte{"config.json": contents})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("writing corrupted file: %v", err)
	}
	if err := Verify(spec, dir); err == nil {
		t.Error("Verify on a corrupted file succeeded, want an error")
	}
}

func TestRegistryPotionBase8M(t *testing.T) {
	spec, ok := Registry["potion-base-8M"]
	if !ok {
		t.Fatal(`Registry["potion-base-8M"] missing`)
	}
	if spec.Repo != "minishlab/potion-base-8M" {
		t.Errorf("Repo = %q, want minishlab/potion-base-8M", spec.Repo)
	}
	if spec.Dim != 256 {
		t.Errorf("Dim = %d, want 256", spec.Dim)
	}
	if len(spec.Revision) != 40 {
		t.Errorf("Revision = %q, want a 40-character git commit hash", spec.Revision)
	}
	wantFiles := map[string]bool{"config.json": true, "tokenizer.json": true, "model.safetensors": true}
	if len(spec.Files) != len(wantFiles) {
		t.Fatalf("Files has %d entries, want %d", len(spec.Files), len(wantFiles))
	}
	for _, f := range spec.Files {
		if !wantFiles[f.Name] {
			t.Errorf("unexpected file %q in Files", f.Name)
		}
		if len(f.SHA256) != 64 {
			t.Errorf("%s SHA256 = %q, want a 64-character hex digest", f.Name, f.SHA256)
		}
		if f.Size <= 0 {
			t.Errorf("%s Size = %d, want > 0", f.Name, f.Size)
		}
	}
}
