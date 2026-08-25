// Package model2vec implements pure-Go inference for Model2Vec static
// embedding models (github.com/MinishLab/model2vec): a BERT-style WordPiece
// tokenizer paired with a lookup-table embedding matrix that is mean-pooled
// per document. It also provides a small registry and downloader for
// pinned, hash-verified model files, so a model is only ever fetched once
// and every later run is fully offline.
//
// Known divergences from the reference Python (numpy) and Rust
// (huggingface/tokenizers) implementations:
//
//   - Lowercasing: Rust's char::to_lowercase is one-to-many for a handful
//     of code points (notably U+0130, LATIN CAPITAL LETTER I WITH DOT
//     ABOVE, which lowercases to two runes, "i" followed by a combining
//     dot above). Go's unicode.ToLower is one-to-one and produces only the
//     first rune. This can change WordPiece segmentation for input text
//     that contains those code points.
//   - Mean-pool summation order: numpy uses pairwise (tree) summation once
//     128 or more values are being summed, which accumulates
//     floating-point rounding error differently than naive left-to-right
//     summation. This package instead accumulates every embedding
//     dimension in float64 across all tokens and rounds to float32 once,
//     which is numerically close but not bit-identical to numpy's result.
//   - added_tokens: tokenizer.json's top-level added_tokens list (special
//     tokens such as [PAD]/[UNK]/[CLS]/[SEP]/[MASK], each usually marked
//     "normalized": false) is not read by this package at all. The
//     reference tokenizer extracts a non-normalized added token from the
//     input verbatim, before the normalizer or WordPiece ever runs; this
//     package instead always normalizes and WordPieces the whole input, so
//     a document that literally contains the text of a non-normalized
//     added token (e.g. the literal string "[MASK]") tokenizes differently
//     here than in the reference implementation. This is expected to be
//     rare in real prose and is not corrected for.
//
// Every divergence above is why the real-model integration test in this package
// compares against testdata/reference_vectors.json with a tolerance of
// 1e-6 rather than requiring bit-exact equality.
package model2vec
