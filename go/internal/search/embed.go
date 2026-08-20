package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Embedder turns text into a fixed-dimension, L2-normalized vector. It is
// the compatibility contract recorded in a search index: find must use the
// embedder named by the index it queries.
type Embedder interface {
	// ID identifies this embedder, stable across process restarts. It is
	// stored in the index and used to detect a mismatched embedder.
	ID() string
	// Dim returns the dimension of vectors this embedder produces. For an
	// embedder that learns its dimension from a remote server, Dim returns 0
	// until Embed has succeeded at least once.
	Dim() int
	// Embed returns the L2-normalized embedding of text.
	Embed(text string) ([]float32, error)
}

// NewEmbedder builds the embedder named by id, the inverse of every
// embedder's ID method. It is how find resumes an index with the embedder
// it was built with: "builtin-lexical-v1" or "ollama:<model>".
func NewEmbedder(id string) (Embedder, error) {
	if id == (LexicalEmbedder{}).ID() {
		return LexicalEmbedder{}, nil
	}
	if model, ok := strings.CutPrefix(id, "ollama:"); ok && model != "" {
		return NewOllamaEmbedder(model), nil
	}
	return nil, fmt.Errorf("unknown embedder: %s", id)
}

// builtinLexicalDim is the fixed feature-hashing width of LexicalEmbedder.
const builtinLexicalDim = 512

// LexicalEmbedder is the builtin-lexical-v1 embedder: a deterministic hashed
// bag-of-words. It ranks shared vocabulary — keyword overlap — not meaning,
// so callers presenting it to users should call it lexical (keyword)
// matching, never semantic search.
type LexicalEmbedder struct{}

// ID implements Embedder.
func (LexicalEmbedder) ID() string { return "builtin-lexical-v1" }

// Dim implements Embedder.
func (LexicalEmbedder) Dim() int { return builtinLexicalDim }

// Embed implements Embedder. Text is lowercased and split into runs of
// Unicode letters and digits; a fixed stopword list is dropped; each
// remaining token is weighted 1+log(tf) and feature-hashed into one of 512
// buckets, with a second hash choosing the bucket's sign; the result is
// L2-normalized.
func (LexicalEmbedder) Embed(text string) ([]float32, error) {
	termFreq := make(map[string]int)
	for _, token := range tokenize(text) {
		if lexicalStopwords[token] {
			continue
		}
		termFreq[token]++
	}

	vec := make([]float32, builtinLexicalDim)
	for token, freq := range termFreq {
		weight := float32(1 + math.Log(float64(freq)))
		vec[hashBucket(token)] += weight * hashSign(token)
	}
	normalize(vec)
	return vec, nil
}

// tokenize lowercases text and splits it into runs of Unicode letters and
// digits, discarding everything else.
func tokenize(text string) []string {
	var tokens []string
	var current strings.Builder
	for _, r := range text {
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
			continue
		}
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// lexicalStopwords is a fixed, small list of common English words dropped
// before hashing so they do not dominate every document's vector.
var lexicalStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "from": true,
	"has": true, "have": true, "he": true, "in": true, "is": true, "it": true,
	"its": true, "of": true, "on": true, "or": true, "that": true,
	"the": true, "this": true, "to": true, "was": true, "were": true,
	"will": true, "with": true,
}

// hashBucket deterministically maps a token to one of builtinLexicalDim
// feature-hashing buckets.
func hashBucket(token string) int {
	h := fnv.New32a()
	h.Write([]byte(token))
	return int(h.Sum32() % builtinLexicalDim)
}

// hashSign deterministically maps a token to +1 or -1, independently of
// hashBucket, so unrelated tokens hashed into the same bucket tend to
// cancel rather than only ever add.
func hashSign(token string) float32 {
	h := fnv.New32a()
	h.Write([]byte("sign:"))
	h.Write([]byte(token))
	if h.Sum32()%2 == 0 {
		return 1
	}
	return -1
}

// normalize scales vec to unit L2 norm in place, leaving an all-zero vector
// unchanged.
func normalize(vec []float32) {
	var sumSquares float64
	for _, v := range vec {
		sumSquares += float64(v) * float64(v)
	}
	if sumSquares == 0 {
		return
	}
	norm := float32(math.Sqrt(sumSquares))
	for i := range vec {
		vec[i] /= norm
	}
}

// defaultOllamaBaseURL is the local-only address the index and find
// commands talk to; nothing an OllamaEmbedder does ever leaves the machine.
const defaultOllamaBaseURL = "http://localhost:11434"

// ollamaTimeout bounds one embedding request against the local server.
const ollamaTimeout = 30 * time.Second

// OllamaEmbedder embeds text by calling a local Ollama server's
// /api/embeddings endpoint. Its dimension is unknown until the server's
// first response, per Dim.
type OllamaEmbedder struct {
	model   string
	baseURL string
	client  *http.Client
	dim     int
}

// NewOllamaEmbedder returns an embedder for model served by the local Ollama
// daemon at its default address.
func NewOllamaEmbedder(model string) *OllamaEmbedder {
	return &OllamaEmbedder{
		model:   model,
		baseURL: defaultOllamaBaseURL,
		client:  &http.Client{Timeout: ollamaTimeout},
	}
}

// WithBaseURL returns a copy of e pointed at a different server. Tests use
// it to target an httptest.Server instead of the real localhost daemon.
func (e *OllamaEmbedder) WithBaseURL(baseURL string) *OllamaEmbedder {
	clone := *e
	clone.baseURL = baseURL
	return &clone
}

// ID implements Embedder.
func (e *OllamaEmbedder) ID() string { return "ollama:" + e.model }

// Dim implements Embedder. It returns 0 until Embed has completed
// successfully at least once.
func (e *OllamaEmbedder) Dim() int { return e.dim }

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// Embed implements Embedder.
func (e *OllamaEmbedder) Embed(text string) ([]float32, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: e.model, Prompt: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama server unreachable at %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var parsed ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("ollama returned an invalid response: %w", err)
	}
	if len(parsed.Embedding) == 0 {
		return nil, errors.New("ollama returned an empty embedding")
	}
	if e.dim == 0 {
		e.dim = len(parsed.Embedding)
	} else if len(parsed.Embedding) != e.dim {
		return nil, fmt.Errorf(
			"ollama returned a %d-dimension embedding, expected %d", len(parsed.Embedding), e.dim)
	}

	vec := make([]float32, len(parsed.Embedding))
	for i, v := range parsed.Embedding {
		vec[i] = float32(v)
	}
	normalize(vec)
	return vec, nil
}
