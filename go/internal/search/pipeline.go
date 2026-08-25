package search

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Default values for every Pipeline field. This is the only file the
// autoretrieval loop (docs/autoretrieval/) edits.
const (
	// defaultChunkRunes is the target chunk size. Chunks land at a
	// whitespace split point near this size rather than exactly on it.
	defaultChunkRunes = 1200
	// defaultOverlapRunes is how far the next chunk's start backs up from
	// the previous chunk's end, so a match spanning a chunk boundary still
	// appears whole in at least one chunk.
	defaultOverlapRunes = 200
	// defaultLookbackRunes bounds how far a split point may back up from
	// the target chunk size while searching for whitespace.
	defaultLookbackRunes = 200
	// defaultSnippetRunes is the length of the human-readable preview
	// stored beside each chunk.
	defaultSnippetRunes = 160
	// defaultStopwords reports whether BM25 terms drop lexicalStopwords.
	defaultStopwords = true

	// defaultK1 is BM25's term-frequency saturation parameter.
	defaultK1 = 1.2
	// defaultB is BM25's length-normalization parameter.
	defaultB = 0.75
	// defaultFusion selects reciprocal rank fusion over the alpha-weighted
	// linear combination.
	defaultFusion = "rrf"
	// defaultRRFK is the reciprocal-rank-fusion constant k.
	defaultRRFK = 60
	// defaultAlpha is the weight given to the dense ranking under "alpha"
	// fusion.
	defaultAlpha = 0.5
)

// Pipeline holds every retrieval tunable. Index-time fields are baked into
// an index and hashed into its header; query-time fields apply at search
// time and never require a rebuild. This is the only file the
// autoretrieval loop edits.
type Pipeline struct {
	// Index-time.
	ChunkRunes    int  // target chunk size; default 1200
	OverlapRunes  int  // next chunk backs up this far; default 200
	LookbackRunes int  // whitespace search window; default 200
	SnippetRunes  int  // preview length; default 160
	Stopwords     bool // drop lexicalStopwords from BM25 terms; default true
	// Query-time.
	K1     float64 // BM25 term-frequency saturation; default 1.2
	B      float64 // BM25 length normalisation; default 0.75
	Fusion string  // "rrf" or "alpha"; default "rrf"
	RRFK   int     // RRF constant; default 60
	Alpha  float64 // weight of the dense ranking under "alpha"; default 0.5
}

// DefaultPipeline returns the tunables SnapVault ships with.
func DefaultPipeline() Pipeline {
	return Pipeline{
		ChunkRunes:    defaultChunkRunes,
		OverlapRunes:  defaultOverlapRunes,
		LookbackRunes: defaultLookbackRunes,
		SnippetRunes:  defaultSnippetRunes,
		Stopwords:     defaultStopwords,
		K1:            defaultK1,
		B:             defaultB,
		Fusion:        defaultFusion,
		RRFK:          defaultRRFK,
		Alpha:         defaultAlpha,
	}
}

// Validate reports whether p's fields fall within sane ranges, naming the
// first field that does not.
func (p Pipeline) Validate() error {
	if p.ChunkRunes <= 0 {
		return fmt.Errorf("chunk runes must be positive: %d", p.ChunkRunes)
	}
	if p.OverlapRunes < 0 {
		return fmt.Errorf("overlap runes must not be negative: %d", p.OverlapRunes)
	}
	if p.OverlapRunes >= p.ChunkRunes {
		return fmt.Errorf("overlap runes (%d) must be less than chunk runes (%d)", p.OverlapRunes, p.ChunkRunes)
	}
	if p.LookbackRunes < 0 {
		return fmt.Errorf("lookback runes must not be negative: %d", p.LookbackRunes)
	}
	if p.SnippetRunes <= 0 {
		return fmt.Errorf("snippet runes must be positive: %d", p.SnippetRunes)
	}
	if p.K1 < 0 {
		return fmt.Errorf("k1 must not be negative: %v", p.K1)
	}
	if p.B < 0 || p.B > 1 {
		return fmt.Errorf("b must be within [0, 1]: %v", p.B)
	}
	if p.Fusion != "rrf" && p.Fusion != "alpha" {
		return fmt.Errorf("fusion must be %q or %q: %q", "rrf", "alpha", p.Fusion)
	}
	if p.RRFK <= 0 {
		return fmt.Errorf("rrf k must be positive: %d", p.RRFK)
	}
	if p.Alpha < 0 || p.Alpha > 1 {
		return fmt.Errorf("alpha must be within [0, 1]: %v", p.Alpha)
	}
	return nil
}

// IndexHash returns the first 16 hex characters of the SHA-256 hash of p's
// index-time fields in the canonical form
// "chunk=1200;overlap=200;lookback=200;snippet=160;stopwords=true". It
// changes when any index-time field changes and stays fixed across changes
// to query-time fields, so it can be stored in a search index and compared
// against a freshly loaded Pipeline to detect a chunking-settings mismatch
// without decoding the index's vectors.
func (p Pipeline) IndexHash() string {
	canonical := fmt.Sprintf("chunk=%d;overlap=%d;lookback=%d;snippet=%d;stopwords=%t",
		p.ChunkRunes, p.OverlapRunes, p.LookbackRunes, p.SnippetRunes, p.Stopwords)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}
