package search

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func TestLexicalEmbedderID(t *testing.T) {
	if got, want := (LexicalEmbedder{}).ID(), "builtin-lexical-v1"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
}

func TestLexicalEmbedderDim(t *testing.T) {
	e := LexicalEmbedder{}
	if got, want := e.Dim(), 512; got != want {
		t.Errorf("Dim() = %d, want %d", got, want)
	}
	vec, err := e.Embed("payment systems process transactions")
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	if len(vec) != 512 {
		t.Errorf("len(Embed(...)) = %d, want 512", len(vec))
	}
}

func TestLexicalEmbedderDeterministic(t *testing.T) {
	e := LexicalEmbedder{}
	const text = "Payment systems process transactions between banks and merchants."
	first, err := e.Embed(text)
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	second, err := e.Embed(text)
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Embed is not deterministic: component %d differs (%v vs %v)", i, first[i], second[i])
		}
	}
}

func TestLexicalEmbedderNormalized(t *testing.T) {
	e := LexicalEmbedder{}
	vec, err := e.Embed("Payment systems process transactions between banks and merchants.")
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	norm := math.Sqrt(dot(vec, vec))
	if math.Abs(norm-1) > 1e-5 {
		t.Errorf("||Embed(...)|| = %v, want 1", norm)
	}
}

// TestLexicalEmbedderGoldenDeterminism guards the hashed bag-of-words
// algorithm against silent drift: the embedder id is the on-disk
// compatibility contract, so its output for fixed input must never change
// without a new embedder id.
func TestLexicalEmbedderGoldenDeterminism(t *testing.T) {
	e := LexicalEmbedder{}
	vec, err := e.Embed("Payment systems process transactions between banks and merchants.")
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	if len(vec) != 512 {
		t.Fatalf("len(Embed(...)) = %d, want 512", len(vec))
	}

	var raw [512 * 4]byte
	for i, v := range vec {
		bits := math.Float32bits(v)
		raw[4*i] = byte(bits >> 24)
		raw[4*i+1] = byte(bits >> 16)
		raw[4*i+2] = byte(bits >> 8)
		raw[4*i+3] = byte(bits)
	}
	sum := sha256.Sum256(raw[:])
	got := hex.EncodeToString(sum[:])

	const wantGolden = "5279deff44d95d45d84cb5a8e906ae4d6ae4d693b74721385e8fefc60d95acdf"
	if got != wantGolden {
		t.Fatalf("embedding vector golden hash = %s, want %s (update the golden once the change is intentional)", got, wantGolden)
	}
}

func TestLexicalEmbedderKeywordOverlapRanksHigher(t *testing.T) {
	e := LexicalEmbedder{}
	query, err := e.Embed("payment systems")
	if err != nil {
		t.Fatalf("Embed(query) = %v", err)
	}
	related, err := e.Embed("Payment systems process transactions between banks and merchants.")
	if err != nil {
		t.Fatalf("Embed(related) = %v", err)
	}
	unrelated, err := e.Embed("Weather forecasting models predict rainfall using satellite imagery.")
	if err != nil {
		t.Fatalf("Embed(unrelated) = %v", err)
	}

	relatedScore := dot(query, related)
	unrelatedScore := dot(query, unrelated)
	if relatedScore <= unrelatedScore {
		t.Errorf("related score %v not greater than unrelated score %v", relatedScore, unrelatedScore)
	}
}

func TestNewEmbedder(t *testing.T) {
	builtin, err := NewEmbedder("builtin-lexical-v1")
	if err != nil {
		t.Fatalf("NewEmbedder(builtin-lexical-v1) = %v", err)
	}
	if _, ok := builtin.(LexicalEmbedder); !ok {
		t.Errorf("NewEmbedder(builtin-lexical-v1) = %T, want LexicalEmbedder", builtin)
	}

	ollama, err := NewEmbedder("ollama:llama2")
	if err != nil {
		t.Fatalf("NewEmbedder(ollama:llama2) = %v", err)
	}
	o, ok := ollama.(*OllamaEmbedder)
	if !ok {
		t.Fatalf("NewEmbedder(ollama:llama2) = %T, want *OllamaEmbedder", ollama)
	}
	if got, want := o.ID(), "ollama:llama2"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}

	if _, err := NewEmbedder("nonsense"); err == nil {
		t.Error("NewEmbedder(nonsense) succeeded, want an error")
	}
	if _, err := NewEmbedder("ollama:"); err == nil {
		t.Error("NewEmbedder(ollama:) succeeded, want an error")
	}
}

// ollamaHandler builds a minimal /api/embeddings responder for tests: it
// records prompts and returns embedding for each requested prompt, erroring
// with 500 for any prompt not present in embedding.
func ollamaHandler(t *testing.T, model string, embedding map[string][]float64) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("path = %s, want /api/embeddings", r.URL.Path)
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Model != model {
			t.Errorf("request model = %q, want %q", body.Model, model)
		}
		vec, ok := embedding[body.Prompt]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(struct {
			Embedding []float64 `json:"embedding"`
		}{Embedding: vec})
	}
}

func TestOllamaEmbedderCallsLocalServer(t *testing.T) {
	server := httptest.NewServer(ollamaHandler(t, "llama2", map[string][]float64{
		"hello world": {3, 4, 0, 0},
	}))
	defer server.Close()

	e := NewOllamaEmbedder("llama2").WithBaseURL(server.URL)
	if got, want := e.ID(), "ollama:llama2"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	if got := e.Dim(); got != 0 {
		t.Errorf("Dim() before Embed = %d, want 0", got)
	}

	vec, err := e.Embed("hello world")
	if err != nil {
		t.Fatalf("Embed = %v", err)
	}
	if got, want := e.Dim(), 4; got != want {
		t.Errorf("Dim() after Embed = %d, want %d", got, want)
	}
	want := []float32{0.6, 0.8, 0, 0}
	for i := range want {
		if math.Abs(float64(vec[i]-want[i])) > 1e-6 {
			t.Errorf("Embed(...)[%d] = %v, want %v", i, vec[i], want[i])
		}
	}
}

func TestOllamaEmbedderUnreachableServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // closed before use: connections are refused immediately.

	e := NewOllamaEmbedder("llama2").WithBaseURL(server.URL)
	if _, err := e.Embed("hello"); err == nil {
		t.Error("Embed against a closed server succeeded, want an error")
	}
}

func TestOllamaEmbedderServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusInternalServerError)
	}))
	defer server.Close()

	e := NewOllamaEmbedder("missing-model").WithBaseURL(server.URL)
	_, err := e.Embed("hello")
	if err == nil {
		t.Fatal("Embed against a 500 response succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the status code", err.Error())
	}
}

func TestOllamaEmbedderDimensionMismatch(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		vec := []float64{1, 2, 3, 4}
		if calls == 2 {
			vec = []float64{1, 2}
		}
		json.NewEncoder(w).Encode(struct {
			Embedding []float64 `json:"embedding"`
		}{Embedding: vec})
	}))
	defer server.Close()

	e := NewOllamaEmbedder("llama2").WithBaseURL(server.URL)
	if _, err := e.Embed("first"); err != nil {
		t.Fatalf("first Embed = %v", err)
	}
	if _, err := e.Embed("second"); err == nil {
		t.Error("second Embed with a different dimension succeeded, want an error")
	}
}
