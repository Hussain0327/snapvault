package search

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkTextEmpty(t *testing.T) {
	if chunks := ChunkText("", DefaultPipeline()); chunks != nil {
		t.Errorf("ChunkText(\"\") = %v, want nil", chunks)
	}
}

func TestChunkTextShort(t *testing.T) {
	const text = "Payment systems process transactions between banks."
	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) != 1 {
		t.Fatalf("ChunkText returned %d chunks, want 1", len(chunks))
	}
	if chunks[0].Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", chunks[0].Sequence)
	}
	if chunks[0].Text != text {
		t.Errorf("Text = %q, want %q", chunks[0].Text, text)
	}
	if chunks[0].Snippet != text {
		t.Errorf("Snippet = %q, want %q", chunks[0].Snippet, text)
	}
	if chunks[0].StartRune != 0 || chunks[0].EndRune != utf8.RuneCountInString(text) {
		t.Errorf("[StartRune:EndRune] = [%d:%d], want [0:%d]",
			chunks[0].StartRune, chunks[0].EndRune, utf8.RuneCountInString(text))
	}
}

func TestChunkTextOverlapAndSequence(t *testing.T) {
	// 300 five-rune words separated by single spaces: 1800 runes total, well
	// past the ~1200-rune chunk size, so this must split into multiple
	// overlapping chunks.
	text := strings.TrimSpace(strings.Repeat("abcde ", 300))

	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) < 2 {
		t.Fatalf("ChunkText produced %d chunks, want at least 2", len(chunks))
	}
	for i, c := range chunks {
		if c.Sequence != i {
			t.Errorf("chunk %d has Sequence %d, want %d", i, c.Sequence, i)
		}
	}

	// Consecutive chunks must overlap: the tail of one chunk reappears at the
	// head of the next.
	first, second := chunks[0].Text, chunks[1].Text
	tail := first[len(first)-40:]
	if !strings.Contains(second, tail) {
		t.Errorf("chunk 1 does not contain the tail of chunk 0 (%q); overlap missing", tail)
	}

	// The final chunk must reach the end of the source text.
	last := chunks[len(chunks)-1].Text
	if !strings.HasSuffix(text, last[len(last)-10:]) {
		t.Errorf("last chunk %q does not reach the end of the source text", last)
	}
}

func TestChunkTextSplitsAtWhitespace(t *testing.T) {
	// Every token is a complete 5-letter word; a whitespace-friendly split
	// point must never cut through the middle of one.
	text := strings.TrimSpace(strings.Repeat("abcde ", 400))

	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) < 2 {
		t.Fatalf("ChunkText produced %d chunks, want at least 2", len(chunks))
	}
	for i, c := range chunks[:len(chunks)-1] {
		for _, word := range strings.Fields(c.Text) {
			if word != "abcde" {
				t.Errorf("chunk %d contains a partial word %q; split point was not whitespace-aligned", i, word)
			}
		}
	}
}

// TestChunkTextOffsetsMatchText forces recomputing offsets after the
// TrimSpace-equivalent step: irregular runs of whitespace (spaces, tabs,
// newlines) between words mean the raw split point and the trimmed chunk
// boundary differ, so StartRune/EndRune must track the trim exactly.
func TestChunkTextOffsetsMatchText(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("abcde \n\t  ", 400))
	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) < 2 {
		t.Fatalf("ChunkText produced %d chunks, want at least 2", len(chunks))
	}

	runes := []rune(text)
	for i, c := range chunks {
		if c.StartRune < 0 || c.EndRune > len(runes) || c.StartRune > c.EndRune {
			t.Fatalf("chunk %d has out-of-range offsets [%d:%d] for %d runes", i, c.StartRune, c.EndRune, len(runes))
		}
		if got := string(runes[c.StartRune:c.EndRune]); got != c.Text {
			t.Errorf("chunk %d: []rune(text)[%d:%d] = %q, want Text %q", i, c.StartRune, c.EndRune, got, c.Text)
		}
	}
}

func TestSnippetFlattensNewlines(t *testing.T) {
	text := "first line\nsecond line\r\nthird line"
	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) != 1 {
		t.Fatalf("ChunkText returned %d chunks, want 1", len(chunks))
	}
	if strings.ContainsAny(chunks[0].Snippet, "\n\r") {
		t.Errorf("Snippet = %q, still contains a newline", chunks[0].Snippet)
	}
}

func TestSnippetTruncatesTo160Chars(t *testing.T) {
	text := strings.Repeat("x", 500)
	chunks := ChunkText(text, DefaultPipeline())
	if len(chunks) != 1 {
		t.Fatalf("ChunkText returned %d chunks, want 1", len(chunks))
	}
	if n := utf8.RuneCountInString(chunks[0].Snippet); n != 160 {
		t.Errorf("Snippet has %d runes, want 160", n)
	}
}

func TestSnippetTruncatesToPipelineSnippetRunes(t *testing.T) {
	p := DefaultPipeline()
	p.SnippetRunes = 20
	text := strings.Repeat("x", 100)
	chunks := ChunkText(text, p)
	if len(chunks) != 1 {
		t.Fatalf("ChunkText returned %d chunks, want 1", len(chunks))
	}
	if n := utf8.RuneCountInString(chunks[0].Snippet); n != 20 {
		t.Errorf("Snippet has %d runes, want 20", n)
	}
}
