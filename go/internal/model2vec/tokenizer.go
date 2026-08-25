package model2vec

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// tokenizerConfig is the subset of tokenizer.json this package understands,
// resolved to concrete values: every default and every null-means-something
// rule from Load's supported component list has already been applied.
type tokenizerConfig struct {
	cleanText               bool
	handleChineseChars      bool
	stripAccents            bool
	lowercase               bool
	vocab                   map[string]int32
	unkToken                string
	unkID                   int32
	continuingSubwordPrefix string
	maxInputCharsPerWord    int
}

// tokenizerDoc is the top-level shape of tokenizer.json that this package
// reads. Every other field (added_tokens, post_processor, decoder,
// truncation, padding, ...) is ignored: post_processor in particular is
// ignored deliberately, since inference never adds [CLS]/[SEP].
type tokenizerDoc struct {
	Normalizer   json.RawMessage `json:"normalizer"`
	PreTokenizer json.RawMessage `json:"pre_tokenizer"`
	Model        json.RawMessage `json:"model"`
}

// componentType reads just the "type" discriminator out of a tokenizer.json
// component, so an unsupported component can be rejected before decoding
// the rest of its fields.
type componentType struct {
	Type string `json:"type"`
}

// bertNormalizerDoc decodes tokenizer.json's normalizer component.
// CleanText, HandleChineseChars and Lowercase are *bool, not bool: absent
// from the JSON (nil) must default to true, matching
// huggingface/tokenizers' BertNormalizer::default(), not Go's false
// zero-value default. StripAccents keeps its own "null means same as
// lowercase" rule per the package spec.
type bertNormalizerDoc struct {
	CleanText          *bool `json:"clean_text"`
	HandleChineseChars *bool `json:"handle_chinese_chars"`
	StripAccents       *bool `json:"strip_accents"`
	Lowercase          *bool `json:"lowercase"`
}

// boolOrDefaultTrue returns *b, or true when b is nil.
func boolOrDefaultTrue(b *bool) bool {
	return b == nil || *b
}

type wordPieceModelDoc struct {
	UnkToken                string           `json:"unk_token"`
	ContinuingSubwordPrefix string           `json:"continuing_subword_prefix"`
	MaxInputCharsPerWord    int              `json:"max_input_chars_per_word"`
	Vocab                   map[string]int32 `json:"vocab"`
}

// decodeTokenizer parses raw tokenizer.json bytes, requiring exactly the
// normalizer/pre_tokenizer/model components this package implements.
// Anything else is an error naming the unsupported component.
func decodeTokenizer(raw []byte) (*tokenizerConfig, error) {
	var doc tokenizerDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid tokenizer.json: %w", err)
	}

	normDoc, err := decodeComponent[bertNormalizerDoc](doc.Normalizer, "normalizer", "BertNormalizer")
	if err != nil {
		return nil, err
	}
	if _, err := decodeComponent[struct{}](doc.PreTokenizer, "pre_tokenizer", "BertPreTokenizer"); err != nil {
		return nil, err
	}
	modelDoc, err := decodeComponent[wordPieceModelDoc](doc.Model, "model", "WordPiece")
	if err != nil {
		return nil, err
	}

	if modelDoc.UnkToken == "" {
		return nil, errors.New("tokenizer.json model has no unk_token")
	}
	unkID, ok := modelDoc.Vocab[modelDoc.UnkToken]
	if !ok {
		return nil, fmt.Errorf("tokenizer.json vocab has no entry for unk_token %q", modelDoc.UnkToken)
	}
	if modelDoc.MaxInputCharsPerWord <= 0 {
		return nil, fmt.Errorf("tokenizer.json model has invalid max_input_chars_per_word: %d", modelDoc.MaxInputCharsPerWord)
	}
	if len(modelDoc.Vocab) == 0 {
		return nil, errors.New("tokenizer.json model has an empty vocab")
	}

	lowercase := boolOrDefaultTrue(normDoc.Lowercase)
	stripAccents := lowercase
	if normDoc.StripAccents != nil {
		stripAccents = *normDoc.StripAccents
	}

	return &tokenizerConfig{
		cleanText:               boolOrDefaultTrue(normDoc.CleanText),
		handleChineseChars:      boolOrDefaultTrue(normDoc.HandleChineseChars),
		stripAccents:            stripAccents,
		lowercase:               lowercase,
		vocab:                   modelDoc.Vocab,
		unkToken:                modelDoc.UnkToken,
		unkID:                   unkID,
		continuingSubwordPrefix: modelDoc.ContinuingSubwordPrefix,
		maxInputCharsPerWord:    modelDoc.MaxInputCharsPerWord,
	}, nil
}

// decodeComponent reads the "type" field out of a tokenizer.json component
// (normalizer, pre_tokenizer, or model), requires it to equal want, and
// then decodes the component's full fields into T. name identifies the
// component in error messages.
func decodeComponent[T any](raw json.RawMessage, name, want string) (T, error) {
	var zero T
	if len(raw) == 0 || string(raw) == "null" {
		return zero, fmt.Errorf("tokenizer.json has no %s", name)
	}
	var kind componentType
	if err := json.Unmarshal(raw, &kind); err != nil {
		return zero, fmt.Errorf("invalid tokenizer.json %s: %w", name, err)
	}
	if kind.Type != want {
		return zero, fmt.Errorf("unsupported %s type %q, this package only supports %s", name, kind.Type, want)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return zero, fmt.Errorf("invalid tokenizer.json %s: %w", name, err)
	}
	return v, nil
}

// isASCIIPunct reports whether r is one of the four ASCII punctuation
// ranges BertPreTokenizer isolates as single-rune words: the printable
// ASCII ranges that are neither letters, digits, nor space.
func isASCIIPunct(r rune) bool {
	return (r >= 0x21 && r <= 0x2F) ||
		(r >= 0x3A && r <= 0x40) ||
		(r >= 0x5B && r <= 0x60) ||
		(r >= 0x7B && r <= 0x7E)
}

// isChineseChar reports whether r falls in one of the CJK ideograph blocks
// BertNormalizer's handle_chinese_chars option surrounds with spaces.
func isChineseChar(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B920 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}

// isUnassigned reports whether r is not a member of any Unicode general
// category, i.e. Cn. The unicode package stores every assigned category as
// a RangeTable but has no table for "unassigned": a code point is Cn
// exactly when none of those tables claim it.
func isUnassigned(r rune) bool {
	if r < 0 || r > utf8.MaxRune {
		return false
	}
	for _, table := range unicode.Categories {
		if unicode.Is(table, r) {
			return false
		}
	}
	return true
}

// isDroppedControl reports whether cleanText removes r outright: U+0000,
// U+FFFD, and categories Cc, Cf, Cn, Co, except the three whitespace
// control characters that step 2 instead maps to a space.
func isDroppedControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	if r == 0 || r == 0xFFFD {
		return true
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co) {
		return true
	}
	// Fast path: unicode.IsGraphic and unicode.IsSpace together cover
	// essentially every rune in real text (letters, digits, punctuation,
	// symbols, marks, whitespace), and none of those are ever category Cn.
	// Without this, isUnassigned's linear scan over all of
	// unicode.Categories ran for every ordinary rune of every document —
	// BenchmarkCleanText measured a 7x slowdown without this fast path,
	// on prose that is almost entirely graphic and space runes — even
	// though it is false for essentially all of them. Only the rare
	// remainder (an actually unassigned code point) falls through to the
	// full scan.
	if unicode.IsGraphic(r) || unicode.IsSpace(r) {
		return false
	}
	return isUnassigned(r)
}

// cleanText implements BertNormalizer's clean_text step: drop stray control
// and unassigned code points, and collapse every whitespace rune to a
// single ASCII space.
func cleanText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isDroppedControl(r):
			continue
		case unicode.IsSpace(r) || r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// spaceOutChineseChars surrounds every CJK ideograph in s with a space on
// each side, so the pre-tokenizer's whitespace split later isolates each
// one as its own word.
func spaceOutChineseChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isChineseChar(r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripAccentsFrom NFD-decomposes s and drops every combining mark (Unicode
// category Mn), the standard "strip accents" transform.
func stripAccentsFrom(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.In(r, unicode.Mn) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lowerText lowercases s one rune at a time. Go's unicode.ToLower is
// one-to-one, unlike Rust's char::to_lowercase used by the reference
// tokenizer; see the package comment.
func lowerText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// normalize runs steps 2 through 5 of the inference algorithm: BertNormalizer's
// clean_text, handle_chinese_chars, strip_accents, and lowercase, each gated
// by cfg's resolved flags.
func (cfg *tokenizerConfig) normalize(text string) string {
	if cfg.cleanText {
		text = cleanText(text)
	}
	if cfg.handleChineseChars {
		text = spaceOutChineseChars(text)
	}
	if cfg.stripAccents {
		text = stripAccentsFrom(text)
	}
	if cfg.lowercase {
		text = lowerText(text)
	}
	return text
}

// preTokenize implements BertPreTokenizer: split on whitespace (discarded),
// then further isolate every ASCII-punctuation or Unicode-category-P rune
// as its own single-rune word.
func preTokenize(text string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isASCIIPunct(r) || unicode.IsPunct(r):
			flush()
			words = append(words, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// wordPiece implements the WordPiece algorithm for a single pre-tokenized
// word: greedy longest-match-first from the left, each candidate after the
// first prefixed with continuing_subword_prefix. A word longer than
// maxInputCharsPerWord, or one where any position fails to match, becomes a
// single [UNK] id.
func (cfg *tokenizerConfig) wordPiece(word []rune) []int32 {
	if len(word) > cfg.maxInputCharsPerWord {
		return []int32{cfg.unkID}
	}

	var ids []int32
	start := 0
	for start < len(word) {
		end := len(word)
		matched := false
		for end > start {
			piece := string(word[start:end])
			if start > 0 {
				piece = cfg.continuingSubwordPrefix + piece
			}
			if id, ok := cfg.vocab[piece]; ok {
				ids = append(ids, id)
				start = end
				matched = true
				break
			}
			end--
		}
		if !matched {
			return []int32{cfg.unkID}
		}
	}
	return ids
}

// tokenize runs the full tokenization pipeline (steps 2 through 8: text is
// assumed already pre-truncated by the caller) and returns vocabulary ids
// with every [UNK] dropped and the result truncated to maxTokens.
func (cfg *tokenizerConfig) tokenize(text string, maxTokens int) []int {
	normalized := cfg.normalize(text)
	words := preTokenize(normalized)

	var ids []int
	for _, w := range words {
		for _, id := range cfg.wordPiece([]rune(w)) {
			if id == cfg.unkID {
				continue
			}
			ids = append(ids, int(id))
		}
	}
	if len(ids) > maxTokens {
		ids = ids[:maxTokens]
	}
	return ids
}

// medianTokenLength returns the integer median of the rune lengths of every
// vocab entry: the true median for an odd count, the floor of the average
// of the two middle values for an even count. It is at least 1, so the
// pre-truncation window in Tokenize is never degenerate.
func medianTokenLength(vocab map[string]int32) int {
	lengths := make([]int, 0, len(vocab))
	for token := range vocab {
		lengths = append(lengths, utf8.RuneCountInString(token))
	}
	sort.Ints(lengths)
	n := len(lengths)
	if n == 0 {
		return 1
	}
	median := lengths[n/2]
	if n%2 == 0 {
		median = (lengths[n/2-1] + lengths[n/2]) / 2
	}
	if median < 1 {
		median = 1
	}
	return median
}
