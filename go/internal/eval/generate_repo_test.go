package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// walkGenerateHandler replies to every /api/generate call with a fixed
// question/quote pair built from the request's own document text, so the
// test can assert Generate's contract end to end without needing a real
// Ollama server: one quote, copied verbatim from whatever text this
// particular call embedded in its prompt.
func walkGenerateHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body ollamaGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// The prompt embeds the document verbatim after "Document:\n"; pull
		// a short quote out of it so every file gets a distinct, genuinely
		// verbatim pair.
		idx := strings.LastIndex(body.Prompt, "Document:\n")
		doc := body.Prompt[idx+len("Document:\n"):]
		words := strings.Fields(doc)
		quote := doc
		if len(words) > 5 {
			quote = strings.Join(words[:5], " ")
		}
		inner := `{"pairs":[{"question":"What does this say?","quote":"` + quote + `"}]}`
		json.NewEncoder(w).Encode(ollamaGenerateResponse{Response: inner})
	}
}

func TestGenerateWalksHeadTextBlobsAndWritesQuestions(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "one.txt", "The quartz spires rise above the misty valley at dawn every single day.")
	writeFile(t, src, "docs/two.txt", "A different sentence entirely about telescopes and distant galaxies tonight.")
	writeFile(t, src, "binary.bin", "\x00\x01\x02\xff\xfe not valid utf8 \x00")

	_, r, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	defer cleanup()

	server := httptest.NewServer(walkGenerateHandler(t))
	defer server.Close()

	var out strings.Builder
	written, dropped, err := Generate(context.Background(), r, server.URL, "test-model", 1, &out)
	if err != nil {
		t.Fatalf("Generate = %v", err)
	}
	if written != 2 {
		t.Errorf("written = %d, want 2 (one.txt and docs/two.txt, binary.bin skipped)", written)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}

	qs, err := LoadQuestions(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("LoadQuestions(Generate's output) = %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("LoadQuestions found %d questions, want 2", len(qs))
	}
	paths := map[string]bool{}
	for _, q := range qs {
		paths[q.Path] = true
	}
	if !paths["one.txt"] || !paths["docs/two.txt"] {
		t.Errorf("question paths = %v, want one.txt and docs/two.txt", paths)
	}
}

func TestGenerateSkipsBlobsWithNoExtractableText(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "only.bin", "\x00\x01\x02\xff\xfe binary \x00")

	_, r, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	defer cleanup()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	var out strings.Builder
	written, dropped, err := Generate(context.Background(), r, server.URL, "test-model", 1, &out)
	if err != nil {
		t.Fatalf("Generate = %v", err)
	}
	if written != 0 || dropped != 0 {
		t.Errorf("written, dropped = %d, %d, want 0, 0", written, dropped)
	}
	if called {
		t.Error("Generate called the server for a blob with no extractable text, want it skipped")
	}
}

func TestGenerateStopsOnServerError(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "one.txt", "some ordinary extractable text content here")

	_, r, cleanup, err := BuildCorpusRepo(src)
	if err != nil {
		t.Fatalf("BuildCorpusRepo = %v", err)
	}
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	var out strings.Builder
	_, _, err = Generate(context.Background(), r, server.URL, "test-model", 1, &out)
	if err == nil {
		t.Fatal("Generate against a failing server = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "one.txt") {
		t.Errorf("error = %q, want it to name the failing path", err.Error())
	}
}
