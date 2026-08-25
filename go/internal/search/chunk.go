package search

import (
	"strings"
	"unicode"
)

// Chunk is one piece of a blob's extracted text: its position among the
// blob's chunks, its offsets into that text, the text itself, and a short
// preview for search results.
type Chunk struct {
	Sequence  int
	StartRune int // offset into the extracted text, inclusive
	EndRune   int // exclusive; []rune(text)[StartRune:EndRune] == Text
	Text      string
	Snippet   string
}

// ChunkText splits text into overlapping pieces of about p.ChunkRunes
// runes, preferring to split at whitespace, and returns nil for empty
// input. Each chunk carries a snippet: its first p.SnippetRunes runes with
// newlines flattened to spaces. StartRune and EndRune are recomputed after
// trimming surrounding whitespace from the raw split, so
// []rune(text)[c.StartRune:c.EndRune] == c.Text holds for every chunk c.
func ChunkText(text string, p Pipeline) []Chunk {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	var chunks []Chunk
	for start := 0; start < len(runes); {
		end := min(start+p.ChunkRunes, len(runes))
		if end < len(runes) {
			end = splitPoint(runes, start, end, p.LookbackRunes)
		}
		left, right := trimRuneRange(runes, start, end)
		if left < right {
			text := string(runes[left:right])
			chunks = append(chunks, Chunk{
				Sequence:  len(chunks),
				StartRune: left,
				EndRune:   right,
				Text:      text,
				Snippet:   snippet(text, p.SnippetRunes),
			})
		}
		if end >= len(runes) {
			break
		}
		next := end - p.OverlapRunes
		if next <= start {
			// A split point right after the previous start would make no
			// progress; fall back to the unoverlapped boundary instead of
			// looping forever.
			next = end
		} else {
			// Nudge forward to the next word boundary so the overlap
			// region does not start mid-word.
			next = nextSplitPoint(runes, next, end, p.LookbackRunes)
		}
		start = next
	}
	return chunks
}

// trimRuneRange returns the [left, right) sub-range of runes[start:end]
// with leading and trailing whitespace trimmed, equivalent to applying
// strings.TrimSpace to string(runes[start:end]) but reporting offsets into
// runes rather than a new string.
func trimRuneRange(runes []rune, start, end int) (left, right int) {
	left = start
	for left < end && unicode.IsSpace(runes[left]) {
		left++
	}
	right = end
	for right > left && unicode.IsSpace(runes[right-1]) {
		right--
	}
	return left, right
}

// splitPoint looks backward from end, within lookback runes of it, for a
// whitespace rune to split on. It returns end unchanged when none is
// found, so a single very long word is simply cut.
func splitPoint(runes []rune, start, end, lookback int) int {
	limit := max(start, end-lookback)
	for i := end; i > limit; i-- {
		if unicode.IsSpace(runes[i-1]) {
			return i
		}
	}
	return end
}

// nextSplitPoint looks forward from from, within lookback runes of it, for
// a whitespace rune and returns the index just past it. It returns from
// unchanged when none is found within limit, so a single very long word is
// simply cut.
func nextSplitPoint(runes []rune, from, limit, lookback int) int {
	bound := min(from+lookback, limit)
	for i := from; i < bound; i++ {
		if unicode.IsSpace(runes[i]) {
			return i + 1
		}
	}
	return from
}

// snippet returns the first snippetRunes runes of text with newlines
// flattened to single spaces, for display beside a search result.
func snippet(text string, snippetRunes int) string {
	flattened := strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	runes := []rune(flattened)
	if len(runes) > snippetRunes {
		runes = runes[:snippetRunes]
	}
	return string(runes)
}
