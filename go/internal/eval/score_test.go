package eval

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestUnionRangesEmpty(t *testing.T) {
	if got := UnionRanges(nil); got != nil {
		t.Errorf("UnionRanges(nil) = %v, want nil", got)
	}
}

func TestUnionRangesMerges(t *testing.T) {
	tests := []struct {
		name string
		in   []Range
		want []Range
	}{
		{
			name: "already disjoint, unsorted",
			in:   []Range{{20, 25}, {0, 5}},
			want: []Range{{0, 5}, {20, 25}},
		},
		{
			name: "touching ranges merge",
			in:   []Range{{0, 5}, {5, 10}, {20, 25}},
			want: []Range{{0, 10}, {20, 25}},
		},
		{
			name: "overlapping ranges merge",
			in:   []Range{{0, 10}, {5, 15}},
			want: []Range{{0, 15}},
		},
		{
			name: "one range fully contains another",
			in:   []Range{{0, 20}, {5, 10}},
			want: []Range{{0, 20}},
		},
		{
			name: "single range",
			in:   []Range{{3, 7}},
			want: []Range{{3, 7}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnionRanges(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UnionRanges(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestOverlapLen(t *testing.T) {
	tests := []struct {
		name string
		a, b []Range
		want int
	}{
		{name: "no overlap", a: []Range{{0, 5}}, b: []Range{{10, 15}}, want: 0},
		{name: "both empty", a: nil, b: nil, want: 0},
		{name: "one empty", a: []Range{{0, 5}}, b: nil, want: 0},
		{name: "identical", a: []Range{{0, 10}}, b: []Range{{0, 10}}, want: 10},
		{name: "partial", a: []Range{{0, 10}}, b: []Range{{5, 15}}, want: 5},
		{
			name: "multiple disjoint spans on each side",
			a:    []Range{{0, 10}, {20, 30}},
			b:    []Range{{5, 25}},
			want: 10, // 5 runes from [5,10) plus 5 runes from [20,25)
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OverlapLen(tt.a, tt.b); got != tt.want {
				t.Errorf("OverlapLen(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
			if got := OverlapLen(tt.b, tt.a); got != tt.want {
				t.Errorf("OverlapLen(%v, %v) = %d, want %d (not symmetric)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestFBeta(t *testing.T) {
	tests := []struct {
		name       string
		p, r, beta float64
		want       float64
	}{
		{name: "zero precision and recall", p: 0, r: 0, beta: 1, want: 0},
		{name: "perfect, beta 1", p: 1, r: 1, beta: 1, want: 1},
		{name: "balanced, beta 1 (F1)", p: 0.5, r: 0.5, beta: 1, want: 0.5},
		{name: "beta 2 weights recall higher", p: 1, r: 0.5, beta: 2, want: 5 * 0.5 / 4.5},
		{name: "precision only, beta 2", p: 1, r: 0, beta: 2, want: 0},
		{name: "recall only, beta 2", p: 0, r: 1, beta: 2, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FBeta(tt.p, tt.r, tt.beta)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("FBeta(%v, %v, %v) = %v, want %v", tt.p, tt.r, tt.beta, got, tt.want)
			}
		})
	}
}

func TestScoreQuestion(t *testing.T) {
	const beta = 2.0

	tests := []struct {
		name                               string
		refs, retrieved                    []Range
		wantPrecision, wantRecall, wantIoU float64
	}{
		{
			name:          "overlapping retrieved chunks are not double counted",
			refs:          []Range{{0, 20}},
			retrieved:     []Range{{0, 15}, {10, 25}}, // union is [0,25); naive sum would be 30
			wantPrecision: 20.0 / 25.0,
			wantRecall:    20.0 / 20.0,
			wantIoU:       20.0 / 25.0, // union(retrieved,refs) = [0,25), matched 20
		},
		{
			name:          "zero retrieved",
			refs:          []Range{{0, 10}},
			retrieved:     nil,
			wantPrecision: 0,
			wantRecall:    0,
			wantIoU:       0,
		},
		{
			name:          "zero refs",
			refs:          nil,
			retrieved:     []Range{{0, 10}},
			wantPrecision: 0,
			wantRecall:    0,
			wantIoU:       0,
		},
		{
			name:          "perfect match",
			refs:          []Range{{5, 15}},
			retrieved:     []Range{{5, 15}},
			wantPrecision: 1,
			wantRecall:    1,
			wantIoU:       1,
		},
		{
			name:          "disjoint spans, no overlap",
			refs:          []Range{{0, 10}},
			retrieved:     []Range{{20, 30}},
			wantPrecision: 0,
			wantRecall:    0,
			wantIoU:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScoreQuestion("q1", tt.refs, tt.retrieved, beta)
			if got.ID != "q1" {
				t.Errorf("ID = %q, want q1", got.ID)
			}
			if math.Abs(got.Precision-tt.wantPrecision) > 1e-9 {
				t.Errorf("Precision = %v, want %v", got.Precision, tt.wantPrecision)
			}
			if math.Abs(got.Recall-tt.wantRecall) > 1e-9 {
				t.Errorf("Recall = %v, want %v", got.Recall, tt.wantRecall)
			}
			if math.Abs(got.IoU-tt.wantIoU) > 1e-9 {
				t.Errorf("IoU = %v, want %v", got.IoU, tt.wantIoU)
			}
			wantFBeta := FBeta(tt.wantPrecision, tt.wantRecall, beta)
			if math.Abs(got.FBeta-wantFBeta) > 1e-9 {
				t.Errorf("FBeta = %v, want %v", got.FBeta, wantFBeta)
			}
			if got.Hit || got.ReciprocalRank != 0 {
				t.Errorf("ScoreQuestion set Hit/ReciprocalRank (%v, %v), want the zero values", got.Hit, got.ReciprocalRank)
			}
		})
	}
}

// aggregateFixture returns five hand-scored questions and a tag map
// covering an overlapping ("hard" is a subset of "lexical") tag structure,
// used by both TestAggregate and TestReportWriteTable.
func aggregateFixture() ([]QuestionScore, map[string][]string) {
	scores := []QuestionScore{
		{ID: "q1", Precision: 1, Recall: 1, FBeta: 1, IoU: 1, Hit: true, ReciprocalRank: 1},
		{ID: "q2", Precision: 0, Recall: 0, FBeta: 0, IoU: 0, Hit: false, ReciprocalRank: 0},
		{ID: "q3", Precision: 0.5, Recall: 0.5, FBeta: 0.5, IoU: 0.5, Hit: true, ReciprocalRank: 0.5},
		{ID: "q4", Precision: 1, Recall: 0, FBeta: 0, IoU: 0, Hit: false, ReciprocalRank: 0},
		{ID: "q5", Precision: 0, Recall: 1, FBeta: 0, IoU: 0, Hit: true, ReciprocalRank: 1},
	}
	tags := map[string][]string{
		"q1": {"lexical"},
		"q2": {"lexical"},
		"q3": {"lexical", "hard"},
		"q4": {"semantic"},
		"q5": {"semantic"},
	}
	return scores, tags
}

// meanStd computes a Stat the same way a reader double-checking the numbers
// by hand would: independently of package eval's own stat helper.
func meanStd(vals []float64) Stat {
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var sumSquares float64
	for _, v := range vals {
		d := v - mean
		sumSquares += d * d
	}
	return Stat{Mean: mean, Std: math.Sqrt(sumSquares / float64(len(vals)))}
}

func checkStat(t *testing.T, label string, got Stat, want Stat) {
	t.Helper()
	if math.Abs(got.Mean-want.Mean) > 1e-9 {
		t.Errorf("%s.Mean = %v, want %v", label, got.Mean, want.Mean)
	}
	if math.Abs(got.Std-want.Std) > 1e-9 {
		t.Errorf("%s.Std = %v, want %v", label, got.Std, want.Std)
	}
}

func TestAggregateOverall(t *testing.T) {
	scores, tags := aggregateFixture()
	report := Aggregate(scores, tags, "builtin-lexical-v1", 10, 2.0)

	if report.EmbedderID != "builtin-lexical-v1" || report.K != 10 || report.Beta != 2.0 {
		t.Errorf("report header = %+v, want embedderID=builtin-lexical-v1 k=10 beta=2", report)
	}
	if report.Overall.N != 5 {
		t.Fatalf("Overall.N = %d, want 5", report.Overall.N)
	}
	checkStat(t, "Overall.Precision", report.Overall.Precision, meanStd([]float64{1, 0, 0.5, 1, 0}))
	checkStat(t, "Overall.Recall", report.Overall.Recall, meanStd([]float64{1, 0, 0.5, 0, 1}))
	checkStat(t, "Overall.FBeta", report.Overall.FBeta, meanStd([]float64{1, 0, 0.5, 0, 0}))
	checkStat(t, "Overall.IoU", report.Overall.IoU, meanStd([]float64{1, 0, 0.5, 0, 0}))
	checkStat(t, "Overall.RecallAtK", report.Overall.RecallAtK, meanStd([]float64{1, 0, 1, 0, 1}))
	checkStat(t, "Overall.MRR", report.Overall.MRR, meanStd([]float64{1, 0, 0.5, 0, 1}))
}

func TestAggregateByTag(t *testing.T) {
	scores, tags := aggregateFixture()
	report := Aggregate(scores, tags, "builtin-lexical-v1", 10, 2.0)

	if len(report.ByTag) != 3 {
		t.Fatalf("ByTag has %d tags, want 3 (lexical, semantic, hard): %v", len(report.ByTag), report.ByTag)
	}

	lexical, ok := report.ByTag["lexical"]
	if !ok {
		t.Fatal("ByTag missing lexical")
	}
	if lexical.N != 3 {
		t.Errorf("lexical.N = %d, want 3", lexical.N)
	}
	checkStat(t, "lexical.Precision", lexical.Precision, meanStd([]float64{1, 0, 0.5}))
	checkStat(t, "lexical.RecallAtK", lexical.RecallAtK, meanStd([]float64{1, 0, 1}))

	semantic, ok := report.ByTag["semantic"]
	if !ok {
		t.Fatal("ByTag missing semantic")
	}
	if semantic.N != 2 {
		t.Errorf("semantic.N = %d, want 2", semantic.N)
	}
	checkStat(t, "semantic.Precision", semantic.Precision, meanStd([]float64{1, 0}))

	hard, ok := report.ByTag["hard"]
	if !ok {
		t.Fatal("ByTag missing hard")
	}
	if hard.N != 1 {
		t.Errorf("hard.N = %d, want 1", hard.N)
	}
	checkStat(t, "hard.Precision", hard.Precision, Stat{Mean: 0.5, Std: 0})
}

func TestAggregateNoTags(t *testing.T) {
	scores, _ := aggregateFixture()
	report := Aggregate(scores, nil, "builtin-lexical-v1", 10, 2.0)
	if report.ByTag != nil {
		t.Errorf("ByTag = %v, want nil when no tags are supplied", report.ByTag)
	}
}

func TestReportWriteTable(t *testing.T) {
	scores, tags := aggregateFixture()
	report := Aggregate(scores, tags, "builtin-lexical-v1", 10, 2.0)

	var buf bytes.Buffer
	report.WriteTable(&buf)
	out := buf.String()

	lines := strings.Split(out, "\n")
	find := func(prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), prefix) {
				return l
			}
		}
		t.Fatalf("no line starting with %q in table:\n%s", prefix, out)
		return ""
	}

	overall := find("overall")
	if strings.Contains(overall, "low confidence") {
		t.Errorf("overall line flagged low confidence at N=5: %q", overall)
	}
	for _, tag := range []string{"lexical", "semantic", "hard"} {
		line := find(tag)
		if !strings.Contains(line, "low confidence") {
			t.Errorf("%s line (N<5) missing low-confidence flag: %q", tag, line)
		}
	}
}

func TestReportWriteJSONRoundTrip(t *testing.T) {
	scores, tags := aggregateFixture()
	report := Aggregate(scores, tags, "static:potion-base-8M@abc123def456", 10, 2.0)

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON = %v", err)
	}

	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal round trip = %v", err)
	}
	if !reflect.DeepEqual(got, report) {
		t.Errorf("round trip mismatch:\n got:  %+v\nwant: %+v", got, report)
	}
}

// TestReportWriteJSONQuestionsUseCamelCaseKeys checks that QuestionScore
// carries json tags matching the rest of Report: reflect.DeepEqual's
// round-trip test above would pass even with no tags at all, since both
// sides decode with the same Go field casing, so this asserts the actual
// wire keys instead.
func TestReportWriteJSONQuestionsUseCamelCaseKeys(t *testing.T) {
	scores, tags := aggregateFixture()
	report := Aggregate(scores, tags, "builtin-lexical-v1", 10, 2.0)

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	questions, ok := raw["questions"].([]any)
	if !ok || len(questions) == 0 {
		t.Fatalf("raw[\"questions\"] = %v, want a non-empty array", raw["questions"])
	}
	first, ok := questions[0].(map[string]any)
	if !ok {
		t.Fatalf("questions[0] = %v, want an object", questions[0])
	}
	for _, key := range []string{"id", "precision", "recall", "fBeta", "iou", "hit", "reciprocalRank"} {
		if _, ok := first[key]; !ok {
			t.Errorf("questions[0] is missing camelCase key %q; got keys %v", key, first)
		}
	}
	for _, key := range []string{"ID", "Precision", "Recall", "FBeta", "IoU", "Hit", "ReciprocalRank"} {
		if _, ok := first[key]; ok {
			t.Errorf("questions[0] has untagged Go-cased key %q, want it replaced by the camelCase tag", key)
		}
	}
}

func TestReportWriteJSONEmpty(t *testing.T) {
	report := Aggregate(nil, nil, "builtin-lexical-v1", 10, 2.0)

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON = %v", err)
	}
	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal round trip = %v", err)
	}
	if !reflect.DeepEqual(got, report) {
		t.Errorf("round trip mismatch:\n got:  %+v\nwant: %+v", got, report)
	}
}
