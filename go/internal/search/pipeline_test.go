package search

import "testing"

func TestDefaultPipelineValidates(t *testing.T) {
	if err := DefaultPipeline().Validate(); err != nil {
		t.Errorf("DefaultPipeline().Validate() = %v, want nil", err)
	}
}

func TestIndexHashStableAndSixteenHexChars(t *testing.T) {
	p := DefaultPipeline()
	first := p.IndexHash()
	second := p.IndexHash()
	if first != second {
		t.Errorf("IndexHash() = %q then %q, want the same value both times", first, second)
	}
	if len(first) != 16 {
		t.Errorf("len(IndexHash()) = %d, want 16", len(first))
	}
}

func TestIndexHashChangesForEachIndexTimeField(t *testing.T) {
	base := DefaultPipeline()
	baseHash := base.IndexHash()

	tests := []struct {
		name   string
		mutate func(p *Pipeline)
	}{
		{"ChunkRunes", func(p *Pipeline) { p.ChunkRunes++ }},
		{"OverlapRunes", func(p *Pipeline) { p.OverlapRunes++ }},
		{"LookbackRunes", func(p *Pipeline) { p.LookbackRunes++ }},
		{"SnippetRunes", func(p *Pipeline) { p.SnippetRunes++ }},
		{"Stopwords", func(p *Pipeline) { p.Stopwords = !p.Stopwords }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			if got := p.IndexHash(); got == baseHash {
				t.Errorf("IndexHash() unchanged at %q after changing %s, want it to differ", got, tt.name)
			}
		})
	}
}

func TestIndexHashUnchangedForEachQueryTimeField(t *testing.T) {
	base := DefaultPipeline()
	baseHash := base.IndexHash()

	tests := []struct {
		name   string
		mutate func(p *Pipeline)
	}{
		{"K1", func(p *Pipeline) { p.K1 += 1 }},
		{"B", func(p *Pipeline) { p.B = 0.5 }},
		{"Fusion", func(p *Pipeline) { p.Fusion = "alpha" }},
		{"RRFK", func(p *Pipeline) { p.RRFK++ }},
		{"Alpha", func(p *Pipeline) { p.Alpha += 0.1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			if got := p.IndexHash(); got != baseHash {
				t.Errorf("IndexHash() = %q after changing %s, want unchanged %q", got, tt.name, baseHash)
			}
		})
	}
}

func TestPipelineValidateRejectsBadRanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *Pipeline)
	}{
		{"zero chunk runes", func(p *Pipeline) { p.ChunkRunes = 0 }},
		{"negative chunk runes", func(p *Pipeline) { p.ChunkRunes = -1 }},
		{"negative overlap runes", func(p *Pipeline) { p.OverlapRunes = -1 }},
		{"overlap equal to chunk", func(p *Pipeline) { p.OverlapRunes = p.ChunkRunes }},
		{"overlap greater than chunk", func(p *Pipeline) { p.OverlapRunes = p.ChunkRunes + 1 }},
		{"negative lookback runes", func(p *Pipeline) { p.LookbackRunes = -1 }},
		{"zero snippet runes", func(p *Pipeline) { p.SnippetRunes = 0 }},
		{"negative snippet runes", func(p *Pipeline) { p.SnippetRunes = -1 }},
		{"negative k1", func(p *Pipeline) { p.K1 = -0.1 }},
		{"negative b", func(p *Pipeline) { p.B = -0.1 }},
		{"b above 1", func(p *Pipeline) { p.B = 1.1 }},
		{"unknown fusion", func(p *Pipeline) { p.Fusion = "bogus" }},
		{"empty fusion", func(p *Pipeline) { p.Fusion = "" }},
		{"zero rrf k", func(p *Pipeline) { p.RRFK = 0 }},
		{"negative rrf k", func(p *Pipeline) { p.RRFK = -1 }},
		{"negative alpha", func(p *Pipeline) { p.Alpha = -0.1 }},
		{"alpha above 1", func(p *Pipeline) { p.Alpha = 1.1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultPipeline()
			tt.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Errorf("Validate() succeeded for %s, want an error", tt.name)
			}
		})
	}
}
