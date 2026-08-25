package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// genPair and genPairs mirror the JSON shape GenerateForText expects inside
// an Ollama response's "response" field, independent of the package's own
// unexported generatedPairs type, so the test fixtures describe the wire
// contract rather than reach into the implementation.
type genPair struct {
	Question string `json:"question"`
	Quote    string `json:"quote"`
}
type genPairs struct {
	Pairs []genPair `json:"pairs"`
}

// generateHandler builds an /api/generate responder that asserts the
// request shape GenerateForText must send and replies with response,
// json-encoded into the outer body's "response" field.
func generateHandler(t *testing.T, wantModel string, response genPairs) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/generate" {
			t.Errorf("path = %s, want /api/generate", r.URL.Path)
		}
		var body ollamaGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Model != wantModel {
			t.Errorf("request model = %q, want %q", body.Model, wantModel)
		}
		if body.Format != "json" {
			t.Errorf("request format = %q, want json", body.Format)
		}
		if body.Stream {
			t.Error("request stream = true, want false")
		}

		inner, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal response fixture: %v", err)
		}
		json.NewEncoder(w).Encode(ollamaGenerateResponse{Response: string(inner)})
	}
}

func TestGenerateForTextFiltersPairs(t *testing.T) {
	const text = "The quick brown fox jumps over the lazy dog. The fox is quick."
	const path = "doc.md"
	const model = "testmodel"

	server := httptest.NewServer(generateHandler(t, model, genPairs{Pairs: []genPair{
		{Question: "What does the fox jump over?", Quote: "The quick brown fox jumps over the lazy dog."}, // kept
		{Question: "What does the elephant do?", Quote: "The elephant walks slowly."},                     // dropped: not a substring
		{Question: "How is the fox described at the end?", Quote: "The fox is quick."},                    // kept
		{Question: "Asked again", Quote: "The fox is quick."},                                             // dropped: duplicate quote
	}}))
	defer server.Close()

	got, dropped, err := GenerateForText(context.Background(), server.Client(), server.URL, model, path, text, 4)
	if err != nil {
		t.Fatalf("GenerateForText = %v", err)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}

	want := []Question{
		{ID: "doc.md#0", Question: "What does the fox jump over?", Path: path, Highlight: "The quick brown fox jumps over the lazy dog."},
		{ID: "doc.md#1", Question: "How is the fox described at the end?", Path: path, Highlight: "The fox is quick."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GenerateForText questions = %+v, want %+v", got, want)
	}
}

func TestGenerateForTextDropsEmptyQuestionOrQuote(t *testing.T) {
	const text = "Weather forecasting models predict rainfall using satellite imagery."
	server := httptest.NewServer(generateHandler(t, "m", genPairs{Pairs: []genPair{
		{Question: "", Quote: "predict rainfall"},
		{Question: "Q", Quote: "   "},
	}}))
	defer server.Close()

	got, dropped, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", text, 2)
	if err != nil {
		t.Fatalf("GenerateForText = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("questions = %v, want none", got)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
}

// TestGenerateForTextDropsQuoteThatOccursOverlappingButNotNonOverlapping
// checks that GenerateForText's uniqueness check agrees with
// LocateHighlight's, which eval run uses to score a generated question
// file: strings.Count counts non-overlapping occurrences only, so a
// self-similar quote can look unique to Count while LocateHighlight finds
// several overlapping matches. Before this fix, such a quote survived
// generation and made eval run hard-fail the whole run with an
// AmbiguousHighlightError instead of being dropped here as intended.
func TestGenerateForTextDropsQuoteThatOccursOverlappingButNotNonOverlapping(t *testing.T) {
	text := strings.Repeat("- ", 30)
	quote := strings.Repeat("- ", 25) // occurs once non-overlapping, several times overlapping

	if strings.Count(text, quote) != 1 {
		t.Fatalf("test fixture invariant broken: strings.Count(text, quote) = %d, want 1", strings.Count(text, quote))
	}
	if _, err := LocateHighlight(text, quote); err == nil {
		t.Fatal("test fixture invariant broken: LocateHighlight found no ambiguity, want an AmbiguousHighlightError")
	}

	server := httptest.NewServer(generateHandler(t, "m", genPairs{Pairs: []genPair{
		{Question: "Q", Quote: quote},
	}}))
	defer server.Close()

	got, dropped, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", text, 1)
	if err != nil {
		t.Fatalf("GenerateForText = %v", err)
	}
	if got != nil {
		t.Errorf("questions = %v, want nil (a quote LocateHighlight cannot locate unambiguously must be dropped)", got)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestGenerateForTextAllPairsDropped(t *testing.T) {
	server := httptest.NewServer(generateHandler(t, "m", genPairs{Pairs: []genPair{
		{Question: "Q", Quote: "not present anywhere"},
	}}))
	defer server.Close()

	got, dropped, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", "unrelated text", 1)
	if err != nil {
		t.Fatalf("GenerateForText = %v", err)
	}
	if got != nil {
		t.Errorf("questions = %v, want nil", got)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestGenerateForTextServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, _, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", "text", 1)
	if err == nil {
		t.Fatal("GenerateForText against a 500 response succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the status code", err.Error())
	}
}

func TestGenerateForTextMalformedOuterJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	_, _, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", "text", 1)
	if err == nil {
		t.Fatal("GenerateForText against a non-JSON body succeeded, want an error")
	}
}

func TestGenerateForTextMalformedInnerJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ollamaGenerateResponse{Response: "not the expected shape"})
	}))
	defer server.Close()

	_, _, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", "text", 1)
	if err == nil {
		t.Fatal("GenerateForText against a malformed inner response succeeded, want an error")
	}
}

func TestGenerateForTextUnreachableServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // closed before use: connections are refused immediately.

	_, _, err := GenerateForText(context.Background(), server.Client(), server.URL, "m", "w.md", "text", 1)
	if err == nil {
		t.Error("GenerateForText against a closed server succeeded, want an error")
	}
}
