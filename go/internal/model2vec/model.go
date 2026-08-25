package model2vec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// maxTokens bounds a tokenized document to the model's trained sequence
// length, per step 8 of the inference algorithm.
const maxTokens = 512

// Model is a loaded Model2Vec static embedding model: a WordPiece tokenizer
// paired with a lookup-table embedding matrix. A Model is safe for
// concurrent use by multiple goroutines, since Tokenize and Embed never
// mutate it.
type Model struct {
	dim               int
	normalize         bool
	embeddings        []float32 // len == vocabSize*dim, row-major
	tokenizer         *tokenizerConfig
	medianTokenLength int
	digest            string
}

// modelConfig is the subset of config.json this package reads.
type modelConfig struct {
	Normalize *bool `json:"normalize"`
	HiddenDim *int  `json:"hidden_dim"`
}

// Load reads config.json, tokenizer.json, and model.safetensors from dir
// and returns the model they describe.
func Load(dir string) (*Model, error) {
	cfg, err := loadModelConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("loading config.json: %w", err)
	}

	tokenizerRaw, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("loading tokenizer.json: %w", err)
	}
	tok, err := decodeTokenizer(tokenizerRaw)
	if err != nil {
		return nil, fmt.Errorf("loading tokenizer.json: %w", err)
	}

	tensorsRaw, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("loading model.safetensors: %w", err)
	}
	vocabSize, dim, embeddings, err := parseSafetensorsEmbeddings(tensorsRaw)
	if err != nil {
		return nil, fmt.Errorf("loading model.safetensors: %w", err)
	}
	if cfg.HiddenDim != nil && *cfg.HiddenDim != dim {
		return nil, fmt.Errorf(
			"config.json hidden_dim %d does not match model.safetensors embeddings dimension %d", *cfg.HiddenDim, dim)
	}
	// A tokenizer.json vocab id past the embeddings matrix's row count (or
	// negative) would otherwise slice out of range only once Embed is
	// called on text that happens to reach it — a crash on a mismatched
	// tokenizer/tensor pair (e.g. an interrupted model re-pull) rather than
	// an error naming the file at load time.
	for token, id := range tok.vocab {
		if id < 0 || int(id) >= vocabSize {
			return nil, fmt.Errorf(
				"tokenizer.json vocab entry %q has id %d, out of range for model.safetensors's %d-row embeddings tensor",
				token, id, vocabSize)
		}
	}

	sum := sha256.Sum256(tensorsRaw)

	return &Model{
		dim:               dim,
		normalize:         cfg.Normalize != nil && *cfg.Normalize,
		embeddings:        embeddings,
		tokenizer:         tok,
		medianTokenLength: medianTokenLength(tok.vocab),
		digest:            hex.EncodeToString(sum[:]),
	}, nil
}

// loadModelConfig reads and decodes config.json. normalize defaults to
// false when absent; apply_pca and apply_zipf are ignored as
// distillation-time provenance, per the package spec.
func loadModelConfig(path string) (modelConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return modelConfig{}, err
	}
	var cfg modelConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return modelConfig{}, fmt.Errorf("invalid config.json: %w", err)
	}
	return cfg, nil
}

// Dim returns the dimension of vectors Embed produces.
func (m *Model) Dim() int {
	return m.dim
}

// Digest returns the sha256 hex digest of model.safetensors, computed by
// Load.
func (m *Model) Digest() string {
	return m.digest
}

// Tokenize converts text to vocabulary ids: normalized, pre-tokenized,
// WordPiece-encoded, with every [UNK] id dropped and the result truncated
// to 512 ids. The input is first truncated to 512 * medianTokenLength runes
// so a pathologically long document cannot make tokenization itself
// unbounded.
func (m *Model) Tokenize(text string) []int {
	runes := []rune(text)
	limit := maxTokens * m.medianTokenLength
	if len(runes) > limit {
		text = string(runes[:limit])
	}
	return m.tokenizer.tokenize(text, maxTokens)
}

// Embed returns the mean-pooled embedding of text: the mean of the
// embedding rows for its tokenized ids, L2-normalized when the model's
// config says to. An input that tokenizes to no ids (empty, whitespace
// only, or entirely [UNK]) returns an all-zero vector of Dim() length. So
// does an input whose pooled vector happens to be exactly zero under a
// normalizing model: the zero vector is returned unnormalized rather than
// dividing by a zero norm, which would otherwise produce an all-NaN result.
func (m *Model) Embed(text string) []float32 {
	ids := m.Tokenize(text)
	vec := make([]float32, m.dim)
	if len(ids) == 0 {
		return vec
	}

	acc := make([]float64, m.dim)
	for _, id := range ids {
		row := m.embeddings[id*m.dim : (id+1)*m.dim]
		for i, v := range row {
			acc[i] += float64(v)
		}
	}
	n := float64(len(ids))
	for i, v := range acc {
		vec[i] = float32(v / n)
	}

	if m.normalize {
		var sumSquares float64
		for _, v := range vec {
			sumSquares += float64(v) * float64(v)
		}
		// The spec's inference algorithm assumes the pooled vector is
		// non-zero and so needs no epsilon, and every id Load lets through
		// now has a validated row — but a model whose embeddings tensor
		// happens to be exactly zero for every token in the input is not
		// impossible, and dividing by a zero norm would return an all-NaN
		// vector that then poisons every cosine score it touches, silently
		// rather than as an error naming the cause.
		if sumSquares == 0 {
			return vec
		}
		norm := float32(math.Sqrt(sumSquares))
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}
