// Package eval measures the retrieval quality of snapvault find against a
// committed question set. A Question names a file and a passage of it
// (Highlight) that answers Question; LocateHighlight turns that passage into
// a rune range, ScoreQuestion compares it against retrieved ranges by
// character overlap, and Aggregate summarizes many QuestionScores into a
// Report.
//
// Dataset, scoring, and generation logic (this file, score.go,
// generate.go) depend only on the standard library and are exercised
// entirely offline. The repo-driven parts that resolve a Question's path at
// HEAD, rank it through a repo.Searcher, and walk a repository's own text
// blobs (Run, Generate, BuildCorpusRepo, in run.go, generate_repo.go, and
// corpus.go) additionally depend on go/internal/repo and go/internal/search,
// per this package's "eval -> repo, search" position in the dependency
// graph described in the hybrid-search-eval design spec.
package eval

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"unicode/utf8"
)

// Question is one row of a JSON Lines question file: a question about a
// file in the repository, whose answer is Highlight, a passage that must
// occur exactly once in that file's extracted text.
type Question struct {
	ID        string   `json:"id"`
	Question  string   `json:"question"`
	Path      string   `json:"path"`
	Highlight string   `json:"highlight"`
	Tags      []string `json:"tags"`
}

// LoadQuestions parses r as JSON Lines, one Question per non-blank line. It
// requires every question to have a non-empty ID, Question, Path and
// Highlight, and requires every ID to be unique; the first violation is a
// hard error naming the 1-based line number.
func LoadQuestions(r io.Reader) ([]Question, error) {
	var questions []Question
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		var q Question
		if err := json.Unmarshal([]byte(text), &q); err != nil {
			return nil, fmt.Errorf("line %d: invalid JSON: %w", line, err)
		}
		switch {
		case q.ID == "":
			return nil, fmt.Errorf("line %d: missing id", line)
		case q.Question == "":
			return nil, fmt.Errorf("line %d: question %s: missing question text", line, q.ID)
		case q.Path == "":
			return nil, fmt.Errorf("line %d: question %s: missing path", line, q.ID)
		case q.Highlight == "":
			return nil, fmt.Errorf("line %d: question %s: missing highlight", line, q.ID)
		case seen[q.ID]:
			return nil, fmt.Errorf("line %d: duplicate question id %s", line, q.ID)
		}

		seen[q.ID] = true
		questions = append(questions, q)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read questions: %w", err)
	}
	return questions, nil
}

// sampleSeed fixes Sample's pseudo-random selection so the same question
// file always yields the same subset, run after run.
const sampleSeed = 1

// Sample deterministically selects a subset of qs for a faster eval run.
// pct <= 0 or pct >= 100 returns every question, unmodified. Otherwise it
// returns max(1, len(qs)*pct/100) questions, chosen by a fixed-seed
// shuffle and then returned in their original relative order.
func Sample(qs []Question, pct int) []Question {
	if pct <= 0 || pct >= 100 || len(qs) == 0 {
		return qs
	}

	n := len(qs) * pct / 100
	if n < 1 {
		n = 1
	}
	if n >= len(qs) {
		return qs
	}

	indices := make([]int, len(qs))
	for i := range indices {
		indices[i] = i
	}
	rand.New(rand.NewSource(sampleSeed)).Shuffle(len(indices), func(i, j int) {
		indices[i], indices[j] = indices[j], indices[i]
	})
	chosen := indices[:n]
	sort.Ints(chosen)

	out := make([]Question, n)
	for i, idx := range chosen {
		out[i] = qs[idx]
	}
	return out
}

// Range is a half-open rune-offset interval [Start, End) into a text.
type Range struct{ Start, End int }

// ErrHighlightNotFound is returned by LocateHighlight when a question's
// highlight does not occur in the extracted text of its file.
var ErrHighlightNotFound = errors.New("highlight not found in extracted text")

// AmbiguousHighlightError is returned by LocateHighlight when a highlight
// occurs more than once, naming the rune offset of every occurrence found.
type AmbiguousHighlightError struct{ Offsets []int }

// Error implements error.
func (e *AmbiguousHighlightError) Error() string {
	return fmt.Sprintf("highlight occurs %d times in extracted text, at rune offsets %v",
		len(e.Offsets), e.Offsets)
}

// LocateHighlight finds the single occurrence of highlight within text and
// returns it as a rune range. It fails with ErrHighlightNotFound when
// highlight does not occur and with an *AmbiguousHighlightError, naming
// every occurrence's rune offset, when it occurs more than once (including
// overlapping occurrences).
func LocateHighlight(text, highlight string) (Range, error) {
	if highlight == "" {
		return Range{}, ErrHighlightNotFound
	}

	var byteOffsets []int
	from := 0
	for {
		i := strings.Index(text[from:], highlight)
		if i < 0 {
			break
		}
		byteOffsets = append(byteOffsets, from+i)
		from += i + 1 // advance by one byte so overlapping matches still count.
	}

	switch len(byteOffsets) {
	case 0:
		return Range{}, ErrHighlightNotFound
	case 1:
		start := utf8.RuneCountInString(text[:byteOffsets[0]])
		end := start + utf8.RuneCountInString(highlight)
		return Range{Start: start, End: end}, nil
	default:
		runeOffsets := make([]int, len(byteOffsets))
		for i, b := range byteOffsets {
			runeOffsets[i] = utf8.RuneCountInString(text[:b])
		}
		return Range{}, &AmbiguousHighlightError{Offsets: runeOffsets}
	}
}
