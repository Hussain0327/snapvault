package model2vec

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// referenceCase is one entry of testdata/reference_vectors.json: an input
// text alongside the token ids and embedding vector the Python model2vec
// package produced for the real potion-base-8M model.
type referenceCase struct {
	Text   string    `json:"text"`
	IDs    []int     `json:"ids"`
	Vector []float64 `json:"vector"`
}

// referenceFixture is the top-level shape of testdata/reference_vectors.json:
// generated once, out of band, with the Python model2vec package (see that
// file's header comment), and compared here against this package's own
// Tokenize and Embed.
type referenceFixture struct {
	Model                  string          `json:"model"`
	Revision               string          `json:"revision"`
	ModelSafetensorsSHA256 string          `json:"model_safetensors_sha256"`
	Normalize              bool            `json:"normalize"`
	Dim                    int             `json:"dim"`
	Cases                  []referenceCase `json:"cases"`
}

// TestRealModelParity compares this package's Tokenize and Embed against
// vectors the Python model2vec package produced for the real
// minishlab/potion-base-8M model. It is skipped cleanly whenever the fixture
// or the installed model is absent, since neither is available in every
// environment this package's tests run in.
func TestRealModelParity(t *testing.T) {
	fixturePath := filepath.Join("testdata", "reference_vectors.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("testdata/reference_vectors.json not present")
		}
		t.Fatalf("reading %s: %v", fixturePath, err)
	}
	var fixture referenceFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing %s: %v", fixturePath, err)
	}

	dir, err := Dir("potion-base-8M")
	if err != nil {
		t.Fatalf("Dir(potion-base-8M) = %v", err)
	}
	for _, name := range []string{"config.json", "tokenizer.json", "model.safetensors"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Skipf("model not installed at %s; run 'snapvault model pull potion-base-8M'", dir)
		}
	}

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s) = %v", dir, err)
	}
	if got, want := m.Dim(), fixture.Dim; got != want {
		t.Fatalf("Dim() = %d, want %d", got, want)
	}
	if got := m.Digest(); got != fixture.ModelSafetensorsSHA256 {
		t.Fatalf("Digest() = %s, want %s", got, fixture.ModelSafetensorsSHA256)
	}

	for i, c := range fixture.Cases {
		c := c
		t.Run(fmt.Sprintf("case_%02d", i), func(t *testing.T) {
			gotIDs := m.Tokenize(c.Text)
			if !idsEqual(gotIDs, c.IDs) {
				t.Errorf("Tokenize(%q) = %v, want %v", c.Text, gotIDs, c.IDs)
			}

			gotVec := m.Embed(c.Text)
			if len(gotVec) != len(c.Vector) {
				t.Fatalf("Embed(%q) has length %d, want %d", c.Text, len(gotVec), len(c.Vector))
			}
			for d, want := range c.Vector {
				if diff := math.Abs(float64(gotVec[d]) - want); diff > 1e-6 {
					t.Errorf("Embed(%q)[%d] = %v, want %v (diff %v)", c.Text, d, gotVec[d], want, diff)
				}
			}
		})
	}
}

// idsEqual reports whether got and want hold the same ids in the same
// order, treating nil and an empty slice as equal: Tokenize returns nil
// when no ids survive, while the JSON fixture encodes the same case as an
// empty array.
func idsEqual(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
