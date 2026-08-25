package search

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Hussain0327/snapvault/go/internal/model2vec"
)

// StaticEmbedder is an Embedder backed by a Model2Vec static embedding
// model loaded from local disk. Unlike OllamaEmbedder, it never makes a
// network call: its files must already be installed, by "snapvault model
// pull", before NewStaticEmbedder succeeds.
type StaticEmbedder struct {
	name  string
	model *model2vec.Model
}

// NewStaticEmbedder loads the model named name from model2vec.Dir(name). It
// reports the exact command that installs the model when it is not present
// on disk.
func NewStaticEmbedder(name string) (*StaticEmbedder, error) {
	dir, err := model2vec.Dir(name)
	if err != nil {
		return nil, err
	}
	model, err := model2vec.Load(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("model %s is not installed; run 'snapvault model pull %s'", name, name)
		}
		return nil, err
	}
	return &StaticEmbedder{name: name, model: model}, nil
}

// ID implements Embedder: "static:<name>@<first 12 hex characters of
// Digest()>". This is the compatibility contract an index built with this
// embedder records, so a later mismatched model install — a different
// model.safetensors under the same name — is detected by NewEmbedder rather
// than silently producing vectors incomparable with the ones already in the
// index.
func (e *StaticEmbedder) ID() string {
	return fmt.Sprintf("static:%s@%s", e.name, e.model.Digest()[:12])
}

// Dim implements Embedder.
func (e *StaticEmbedder) Dim() int { return e.model.Dim() }

// Embed implements Embedder. model2vec.Model.Embed already L2-normalizes
// when the model's config asks for it; normalize is applied unconditionally
// here too, so every Embedder honors the "Embed returns the L2-normalized
// embedding" contract regardless of that setting.
func (e *StaticEmbedder) Embed(text string) ([]float32, error) {
	vec := e.model.Embed(text)
	normalize(vec)
	return vec, nil
}

// parseStaticID splits a "static:<name>@<digest12>" embedder id into its
// name and digest parts, reporting ok false when id is not of that shape.
// Bare "static" (no colon) is deliberately not of this shape: it is only a
// CLI alias, resolved before an id ever reaches NewEmbedder.
func parseStaticID(id string) (name, digest12 string, ok bool) {
	rest, ok := strings.CutPrefix(id, "static:")
	if !ok {
		return "", "", false
	}
	name, digest12, found := strings.Cut(rest, "@")
	if !found || name == "" || digest12 == "" {
		return "", "", false
	}
	return name, digest12, true
}
