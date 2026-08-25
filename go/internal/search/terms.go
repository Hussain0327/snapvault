package search

// Terms lowercases and splits text like the lexical embedder does,
// dropping stopwords when requested. It is the tokenizer for BM25 and for
// query terms.
func Terms(text string, stopwords bool) []string {
	tokens := tokenize(text)
	if !stopwords {
		return tokens
	}

	var kept []string
	for _, token := range tokens {
		if lexicalStopwords[token] {
			continue
		}
		kept = append(kept, token)
	}
	return kept
}
