package model2vec

import (
	"strings"
	"testing"
)

func minimalTokenizerDoc(overrides map[string]string) string {
	normalizerType := "BertNormalizer"
	preTokenizerType := "BertPreTokenizer"
	modelType := "WordPiece"
	for k, v := range overrides {
		switch k {
		case "normalizer":
			normalizerType = v
		case "pre_tokenizer":
			preTokenizerType = v
		case "model":
			modelType = v
		}
	}
	return `{
		"normalizer": {"type": "` + normalizerType + `", "clean_text": true, "handle_chinese_chars": true, "strip_accents": null, "lowercase": true},
		"pre_tokenizer": {"type": "` + preTokenizerType + `"},
		"model": {"type": "` + modelType + `", "unk_token": "[UNK]", "continuing_subword_prefix": "##", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0, "hello": 1}}
	}`
}

func TestDecodeTokenizerRejectsUnsupportedNormalizer(t *testing.T) {
	doc := minimalTokenizerDoc(map[string]string{"normalizer": "Sequence"})
	_, err := decodeTokenizer([]byte(doc))
	if err == nil {
		t.Fatal("decodeTokenizer with normalizer type Sequence succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "normalizer") {
		t.Errorf("error = %q, want it to name the normalizer component", err)
	}
}

func TestDecodeTokenizerRejectsUnsupportedPreTokenizer(t *testing.T) {
	doc := minimalTokenizerDoc(map[string]string{"pre_tokenizer": "Whitespace"})
	_, err := decodeTokenizer([]byte(doc))
	if err == nil {
		t.Fatal("decodeTokenizer with pre_tokenizer type Whitespace succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "pre_tokenizer") {
		t.Errorf("error = %q, want it to name the pre_tokenizer component", err)
	}
}

func TestDecodeTokenizerRejectsUnsupportedModel(t *testing.T) {
	doc := minimalTokenizerDoc(map[string]string{"model": "BPE"})
	_, err := decodeTokenizer([]byte(doc))
	if err == nil {
		t.Fatal("decodeTokenizer with model type BPE succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error = %q, want it to name the model component", err)
	}
}

func TestDecodeTokenizerRejectsMissingComponents(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"no normalizer", `{"pre_tokenizer": {"type": "BertPreTokenizer"}, "model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0}}}`},
		{"no pre_tokenizer", `{"normalizer": {"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "lowercase": true}, "model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0}}}`},
		{"no model", `{"normalizer": {"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "lowercase": true}, "pre_tokenizer": {"type": "BertPreTokenizer"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeTokenizer([]byte(tt.doc)); err == nil {
				t.Errorf("decodeTokenizer(%s) succeeded, want an error", tt.name)
			}
		})
	}
}

func TestDecodeTokenizerRejectsUnkTokenNotInVocab(t *testing.T) {
	doc := `{
		"normalizer": {"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "lowercase": true},
		"pre_tokenizer": {"type": "BertPreTokenizer"},
		"model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"hello": 0}}
	}`
	if _, err := decodeTokenizer([]byte(doc)); err == nil {
		t.Error("decodeTokenizer with unk_token missing from vocab succeeded, want an error")
	}
}

func TestDecodeTokenizerStripAccentsNull(t *testing.T) {
	tests := []struct {
		name      string
		lowercase bool
		want      bool
	}{
		{"null defers to lowercase=true", true, true},
		{"null defers to lowercase=false", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := `{
				"normalizer": {"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "strip_accents": null, "lowercase": ` + boolJSON(tt.lowercase) + `},
				"pre_tokenizer": {"type": "BertPreTokenizer"},
				"model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0}}
			}`
			cfg, err := decodeTokenizer([]byte(doc))
			if err != nil {
				t.Fatalf("decodeTokenizer = %v", err)
			}
			if cfg.stripAccents != tt.want {
				t.Errorf("stripAccents = %v, want %v", cfg.stripAccents, tt.want)
			}
		})
	}
}

// TestDecodeTokenizerNormalizerFlagsDefaultToTrueWhenAbsent checks that
// clean_text, handle_chinese_chars and lowercase absent from
// tokenizer.json's normalizer default to true, matching
// huggingface/tokenizers' BertNormalizer::default() (Go's own zero value
// for a bare bool field would default them to false instead, silently
// diverging from the reference implementation: CJK left unsegmented, case
// preserved, control/unassigned code points not dropped).
func TestDecodeTokenizerNormalizerFlagsDefaultToTrueWhenAbsent(t *testing.T) {
	doc := `{
		"normalizer": {"type": "BertNormalizer"},
		"pre_tokenizer": {"type": "BertPreTokenizer"},
		"model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0}}
	}`
	cfg, err := decodeTokenizer([]byte(doc))
	if err != nil {
		t.Fatalf("decodeTokenizer = %v", err)
	}
	if !cfg.cleanText {
		t.Error("cleanText = false, want true (default when clean_text is absent)")
	}
	if !cfg.handleChineseChars {
		t.Error("handleChineseChars = false, want true (default when handle_chinese_chars is absent)")
	}
	if !cfg.lowercase {
		t.Error("lowercase = false, want true (default when lowercase is absent)")
	}
	// strip_accents is also absent, so it defers to lowercase (true) per
	// the existing null-means-same-as-lowercase rule.
	if !cfg.stripAccents {
		t.Error("stripAccents = false, want true (absent strip_accents defers to the now-true lowercase default)")
	}
}

// TestDecodeTokenizerNormalizerFlagsExplicitFalseOverridesDefault checks
// that an explicit false is still honoured, not overridden by the new
// nil-means-true default.
func TestDecodeTokenizerNormalizerFlagsExplicitFalseOverridesDefault(t *testing.T) {
	doc := `{
		"normalizer": {"type": "BertNormalizer", "clean_text": false, "handle_chinese_chars": false, "lowercase": false},
		"pre_tokenizer": {"type": "BertPreTokenizer"},
		"model": {"type": "WordPiece", "unk_token": "[UNK]", "max_input_chars_per_word": 100, "vocab": {"[UNK]": 0}}
	}`
	cfg, err := decodeTokenizer([]byte(doc))
	if err != nil {
		t.Fatalf("decodeTokenizer = %v", err)
	}
	if cfg.cleanText || cfg.handleChineseChars || cfg.lowercase {
		t.Errorf("cfg = %+v, want every explicit false honoured, not overridden by the absent-field default", cfg)
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestIsChineseChar(t *testing.T) {
	tests := []struct {
		r    rune
		want bool
	}{
		{'中', true},
		{'a', false},
		{'1', false},
		{0x4E00, true}, // range start
		{0x9FFF, true}, // range end
		{0x9FFF + 1, false},
	}
	for _, tt := range tests {
		if got := isChineseChar(tt.r); got != tt.want {
			t.Errorf("isChineseChar(%q) = %v, want %v", tt.r, got, tt.want)
		}
	}
}

func TestIsASCIIPunct(t *testing.T) {
	tests := []struct {
		r    rune
		want bool
	}{
		{'.', true},
		{'!', true},
		{'a', false},
		{'0', false},
		{' ', false},
		{'{', true},
	}
	for _, tt := range tests {
		if got := isASCIIPunct(tt.r); got != tt.want {
			t.Errorf("isASCIIPunct(%q) = %v, want %v", tt.r, got, tt.want)
		}
	}
}

func TestPreTokenizeSplitsWhitespaceAndPunctuation(t *testing.T) {
	got := preTokenize("hello, world!")
	want := []string{"hello", ",", "world", "!"}
	if len(got) != len(want) {
		t.Fatalf("preTokenize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("preTokenize()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCleanTextCollapsesWhitespace(t *testing.T) {
	got := cleanText("a\tb\nc\r\nd")
	want := "a b c  d"
	if got != want {
		t.Errorf("cleanText = %q, want %q", got, want)
	}
}

func TestCleanTextDropsNullAndReplacementChar(t *testing.T) {
	got := cleanText("a\x00b�c")
	want := "abc"
	if got != want {
		t.Errorf("cleanText = %q, want %q", got, want)
	}
}

func TestStripAccentsFrom(t *testing.T) {
	got := stripAccentsFrom("café")
	want := "cafe"
	if got != want {
		t.Errorf("stripAccentsFrom(café) = %q, want %q", got, want)
	}
}

// BenchmarkCleanText measures cleanText's cost over ordinary prose, which
// is entirely graphic/space runes: isDroppedControl's fast path should
// resolve every one of them without ever reaching the expensive
// isUnassigned scan over all of unicode.Categories.
func BenchmarkCleanText(b *testing.B) {
	text := strings.Repeat(
		"The quick brown fox jumps over the lazy dog, and the café serves coffee at dawn. ", 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cleanText(text)
	}
}

func TestMedianTokenLength(t *testing.T) {
	tests := []struct {
		name  string
		vocab map[string]int32
		want  int
	}{
		{"empty", map[string]int32{}, 1},
		{"odd count", map[string]int32{"a": 0, "bb": 1, "ccc": 2}, 2},
		{"even count", map[string]int32{"a": 0, "bb": 1, "ccc": 2, "dddd": 3}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := medianTokenLength(tt.vocab); got != tt.want {
				t.Errorf("medianTokenLength = %d, want %d", got, tt.want)
			}
		})
	}
}
