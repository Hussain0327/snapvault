package eval

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLoadQuestionsValid(t *testing.T) {
	const input = `{"id":"q001","question":"What temperature?","path":"recipes/sourdough.md","highlight":"Bake at 230 °C","tags":["lexical"]}

{"id":"q002","question":"Where does bread rise?","path":"recipes/sourdough.md","highlight":"in a warm place"}
`
	got, err := LoadQuestions(strings.NewReader(input))
	if err != nil {
		t.Fatalf("LoadQuestions = %v", err)
	}
	want := []Question{
		{ID: "q001", Question: "What temperature?", Path: "recipes/sourdough.md", Highlight: "Bake at 230 °C", Tags: []string{"lexical"}},
		{ID: "q002", Question: "Where does bread rise?", Path: "recipes/sourdough.md", Highlight: "in a warm place"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadQuestions = %+v, want %+v", got, want)
	}
}

func TestLoadQuestionsErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // substring the error must contain
	}{
		{
			name:  "malformed JSON",
			input: `{"id":"q1",` + "\n",
			want:  "line 1",
		},
		{
			name:  "missing id",
			input: `{"question":"q","path":"p.md","highlight":"h"}` + "\n",
			want:  "missing id",
		},
		{
			name:  "missing question",
			input: `{"id":"q1","path":"p.md","highlight":"h"}` + "\n",
			want:  "missing question",
		},
		{
			name:  "missing path",
			input: `{"id":"q1","question":"q","highlight":"h"}` + "\n",
			want:  "missing path",
		},
		{
			name:  "missing highlight",
			input: `{"id":"q1","question":"q","path":"p.md"}` + "\n",
			want:  "missing highlight",
		},
		{
			name: "duplicate id",
			input: `{"id":"q1","question":"q","path":"p.md","highlight":"h"}` + "\n" +
				`{"id":"q1","question":"q2","path":"p.md","highlight":"h2"}` + "\n",
			want: "duplicate question id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadQuestions(strings.NewReader(tt.input))
			if err == nil {
				t.Fatal("LoadQuestions succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadQuestionsEmpty(t *testing.T) {
	got, err := LoadQuestions(strings.NewReader(""))
	if err != nil {
		t.Fatalf("LoadQuestions(empty) = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadQuestions(empty) = %v, want none", got)
	}
}

func sampleQuestions(n int) []Question {
	qs := make([]Question, n)
	for i := range qs {
		qs[i] = Question{ID: string(rune('a' + i)), Question: "q", Path: "p.md", Highlight: "h"}
	}
	return qs
}

func TestSamplePctAtOrBelowZeroReturnsAll(t *testing.T) {
	qs := sampleQuestions(10)
	for _, pct := range []int{0, -1, -100} {
		if got := Sample(qs, pct); !reflect.DeepEqual(got, qs) {
			t.Errorf("Sample(qs, %d) = %v, want the full set", pct, got)
		}
	}
}

func TestSamplePctAtOrAboveHundredReturnsAll(t *testing.T) {
	qs := sampleQuestions(10)
	for _, pct := range []int{100, 101, 1000} {
		if got := Sample(qs, pct); !reflect.DeepEqual(got, qs) {
			t.Errorf("Sample(qs, %d) = %v, want the full set", pct, got)
		}
	}
}

func TestSampleRoundsUpToAtLeastOne(t *testing.T) {
	qs := sampleQuestions(10)
	// 10 * 5 / 100 = 0, which must round up to 1, not return none.
	got := Sample(qs, 5)
	if len(got) != 1 {
		t.Fatalf("len(Sample(qs, 5)) = %d, want 1", len(got))
	}
}

func TestSampleSizeMatchesPercentage(t *testing.T) {
	qs := sampleQuestions(10)
	got := Sample(qs, 50)
	if len(got) != 5 {
		t.Fatalf("len(Sample(qs, 50)) = %d, want 5", len(got))
	}
}

func TestSampleIsDeterministic(t *testing.T) {
	qs := sampleQuestions(20)
	first := Sample(qs, 30)
	second := Sample(qs, 30)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two Sample calls on the same input disagree:\n%v\n%v", first, second)
	}
}

func TestSamplePreservesRelativeOrder(t *testing.T) {
	qs := sampleQuestions(20)
	got := Sample(qs, 40)
	last := -1
	for _, q := range got {
		idx := -1
		for i, orig := range qs {
			if orig.ID == q.ID {
				idx = i
				break
			}
		}
		if idx <= last {
			t.Fatalf("Sample result is not in original relative order: %v", got)
		}
		last = idx
	}
}

func TestLocateHighlightFound(t *testing.T) {
	// The prefix carries multi-byte runes (é, ö, ☕) so a byte-offset bug
	// would disagree with a correct rune-offset implementation.
	const prefix = "café ☕ terrace overlooking the sea. "
	const highlight = "Bake at 230 °C for 35 minutes"
	const suffix = " in a hot oven."
	text := prefix + highlight + suffix

	got, err := LocateHighlight(text, highlight)
	if err != nil {
		t.Fatalf("LocateHighlight = %v", err)
	}
	wantStart := utf8.RuneCountInString(prefix)
	wantEnd := wantStart + utf8.RuneCountInString(highlight)
	want := Range{Start: wantStart, End: wantEnd}
	if got != want {
		t.Errorf("LocateHighlight = %+v, want %+v", got, want)
	}
	if got := []rune(text)[got.Start:got.End]; string(got) != highlight {
		t.Errorf("runes[Start:End] = %q, want %q", string(got), highlight)
	}
}

func TestLocateHighlightNotFound(t *testing.T) {
	_, err := LocateHighlight("café terrace overlooking the sea.", "Bake at 230 °C")
	if !errors.Is(err, ErrHighlightNotFound) {
		t.Errorf("LocateHighlight = %v, want ErrHighlightNotFound", err)
	}
}

func TestLocateHighlightEmptyHighlight(t *testing.T) {
	_, err := LocateHighlight("café terrace", "")
	if !errors.Is(err, ErrHighlightNotFound) {
		t.Errorf("LocateHighlight(_, \"\") = %v, want ErrHighlightNotFound", err)
	}
}

func TestLocateHighlightAmbiguous(t *testing.T) {
	const prefix = "café ☕ "
	const highlight = "Bake at 230 °C"
	const middle = " and stir. "
	text := prefix + highlight + middle + highlight + " Serve warm."

	_, err := LocateHighlight(text, highlight)
	var ambiguous *AmbiguousHighlightError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("LocateHighlight = %v, want *AmbiguousHighlightError", err)
	}
	if len(ambiguous.Offsets) != 2 {
		t.Fatalf("Offsets = %v, want 2 entries", ambiguous.Offsets)
	}

	wantFirst := utf8.RuneCountInString(prefix)
	wantSecond := wantFirst + utf8.RuneCountInString(highlight) + utf8.RuneCountInString(middle)
	if ambiguous.Offsets[0] != wantFirst || ambiguous.Offsets[1] != wantSecond {
		t.Errorf("Offsets = %v, want [%d %d]", ambiguous.Offsets, wantFirst, wantSecond)
	}
	if ambiguous.Error() == "" {
		t.Error("Error() returned an empty string")
	}
}
