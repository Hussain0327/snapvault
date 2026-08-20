package search

import (
	"strings"
	"unicode"
)

const (
	// chunkRunes is the target chunk size. Chunks land at a whitespace split
	// point near this size rather than exactly on it.
	chunkRunes = 1200
	// chunkOverlapRunes is how far the next chunk's start backs up from the
	// previous chunk's end, so a match spanning a chunk boundary still
	// appears whole in at least one chunk.
	chunkOverlapRunes = 200
	// splitLookbackRunes bounds how far a split point may back up from the
	// target chunk size while searching for whitespace.
	splitLookbackRunes = 200
	// snippetRunes is the length of the human-readable preview stored beside
	// each chunk's embedding.
	snippetRunes = 160
)

// Chunk is one piece of a blob's extracted text: the text itself, its
// position among the blob's chunks, and a short preview for search results.
type Chunk struct {
	Sequence int
	Text     string
	Snippet  string
}

// ChunkText splits text into overlapping pieces of about chunkRunes runes,
// preferring to split at whitespace, and returns nil for empty input. Each
// chunk carries a snippet: its first snippetRunes runes with newlines
// flattened to spaces.
func ChunkText(text string) []Chunk {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	var chunks []Chunk
	for start := 0; start < len(runes); {
		end := min(start+chunkRunes, len(runes))
		if end < len(runes) {
			end = splitPoint(runes, start, end)
		}
		piece := strings.TrimSpace(string(runes[start:end]))
		if piece != "" {
			chunks = append(chunks, Chunk{
				Sequence: len(chunks),
				Text:     piece,
				Snippet:  snippet(piece),
			})
		}
		if end >= len(runes) {
			break
		}
		next := end - chunkOverlapRunes
		if next <= start {
			// A split point right after the previous start would make no
			// progress; fall back to the unoverlapped boundary instead of
			// looping forever.
			next = end
		} else {
			// Nudge forward to the next word boundary so the overlap region
			// does not start mid-word.
			next = nextSplitPoint(runes, next, end)
		}
		start = next
	}
	return chunks
}

// splitPoint looks backward from end, within splitLookbackRunes of it, for a
// whitespace rune to split on. It returns end unchanged when none is found,
// so a single very long word is simply cut.
func splitPoint(runes []rune, start, end int) int {
	limit := max(start, end-splitLookbackRunes)
	for i := end; i > limit; i-- {
		if unicode.IsSpace(runes[i-1]) {
			return i
		}
	}
	return end
}

// nextSplitPoint looks forward from from, within splitLookbackRunes of it,
// for a whitespace rune and returns the index just past it. It returns from
// unchanged when none is found within limit, so a single very long word is
// simply cut.
func nextSplitPoint(runes []rune, from, limit int) int {
	bound := min(from+splitLookbackRunes, limit)
	for i := from; i < bound; i++ {
		if unicode.IsSpace(runes[i]) {
			return i + 1
		}
	}
	return from
}

// snippet returns the first snippetRunes runes of text with newlines
// flattened to single spaces, for display beside a search result.
func snippet(text string) string {
	flattened := strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	runes := []rune(flattened)
	if len(runes) > snippetRunes {
		runes = runes[:snippetRunes]
	}
	return string(runes)
}
