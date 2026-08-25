package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// generatePromptTemplate asks a local Ollama model for n question/quote
// pairs about a document, replying with JSON only so GenerateForText can
// parse ollamaGenerateResponse.Response directly as generatedPairs.
const generatePromptTemplate = `You are building a retrieval evaluation dataset from the document below.
Generate %d question/quote pairs about it. Each quote must be copied
verbatim from the document text, be between 40 and 300 characters long, and
be answered by its paired question. Reply with JSON only, in exactly this
shape: {"pairs":[{"question":"...","quote":"..."}]}.

Document:
%s`

// ollamaGenerateRequest is the body of a POST to {baseURL}/api/generate.
// Format "json" asks Ollama to constrain the model's output to valid JSON;
// Stream false asks for one complete response object instead of a stream
// of partial ones.
type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Format string `json:"format"`
	Stream bool   `json:"stream"`
}

// ollamaGenerateResponse is the body Ollama returns for a non-streamed
// /api/generate call; Response holds the model's generated text, which
// GenerateForText parses again as generatedPairs.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
}

// generatedPairs is the JSON shape requested by generatePromptTemplate.
type generatedPairs struct {
	Pairs []struct {
		Question string `json:"question"`
		Quote    string `json:"quote"`
	} `json:"pairs"`
}

// GenerateForText asks the Ollama model served at baseURL for n
// question/quote pairs about text, by POSTing {baseURL}/api/generate with
// "format": "json" and "stream": false.
//
// A pair is kept only if its quote, trimmed of surrounding whitespace, is
// both a unique substring of text (occurs exactly once) and not a repeat
// of an earlier pair's quote in the same response; every other pair is
// dropped. Each kept Question's ID is path plus the pair's position among
// the kept pairs ("path#0", "path#1", ...), Path is path, and Tags is nil;
// the caller that writes these to a question file assigns tags.
//
// It returns the kept questions, how many pairs were dropped, and an error
// if the server was unreachable, returned a non-200 status, or returned a
// body that was not the expected JSON shape.
func GenerateForText(ctx context.Context, client *http.Client, baseURL, model, path, text string, n int) ([]Question, int, error) {
	prompt := fmt.Sprintf(generatePromptTemplate, n, text)
	body, err := json.Marshal(ollamaGenerateRequest{
		Model:  model,
		Prompt: prompt,
		Format: "json",
		Stream: false,
	})
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ollama server unreachable at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, 0, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var parsed ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, 0, fmt.Errorf("ollama returned an invalid response: %w", err)
	}

	var pairs generatedPairs
	if err := json.Unmarshal([]byte(parsed.Response), &pairs); err != nil {
		return nil, 0, fmt.Errorf("ollama response was not the requested JSON shape: %w", err)
	}

	var questions []Question
	seenQuotes := make(map[string]bool)
	dropped := 0
	for _, pair := range pairs.Pairs {
		quote := strings.TrimSpace(pair.Quote)
		question := strings.TrimSpace(pair.Question)
		// LocateHighlight, not strings.Count(text, quote) != 1: Count only
		// counts non-overlapping matches, while LocateHighlight (and so
		// eval run, downstream) counts every overlapping occurrence. A
		// self-similar quote (e.g. a repeated refrain) can pass Count's
		// non-overlapping check as "occurs once" while LocateHighlight
		// finds several, so generation and scoring must share one
		// definition of "occurs exactly once" or eval generate can emit a
		// question file that eval run hard-fails on.
		if quote == "" || question == "" || seenQuotes[quote] {
			dropped++
			continue
		}
		if _, err := LocateHighlight(text, quote); err != nil {
			dropped++
			continue
		}
		seenQuotes[quote] = true
		questions = append(questions, Question{
			ID:        fmt.Sprintf("%s#%d", path, len(questions)),
			Question:  question,
			Path:      path,
			Highlight: quote,
		})
	}
	return questions, dropped, nil
}
