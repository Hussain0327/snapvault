package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
)

// UnionRanges sorts rs by Start and merges every pair of ranges that
// overlap or touch (b.Start <= a.End), returning the minimal set of
// disjoint ranges covering the same runes as rs, in ascending order.
func UnionRanges(rs []Range) []Range {
	if len(rs) == 0 {
		return nil
	}

	sorted := append([]Range(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	merged := []Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if r.Start > last.End {
			merged = append(merged, r)
			continue
		}
		if r.End > last.End {
			last.End = r.End
		}
	}
	return merged
}

// OverlapLen returns the number of rune positions covered by both a and b.
// a and b must each already be UnionRanges output: sorted and disjoint.
func OverlapLen(a, b []Range) int {
	var total, i, j int
	for i < len(a) && j < len(b) {
		start := max(a[i].Start, b[j].Start)
		end := min(a[i].End, b[j].End)
		if start < end {
			total += end - start
		}
		if a[i].End < b[j].End {
			i++
		} else {
			j++
		}
	}
	return total
}

// FBeta returns the F-beta score of precision p and recall r, weighting
// recall beta times as heavily as precision. It returns 0 when p and r are
// both 0, rather than dividing by zero.
func FBeta(p, r, beta float64) float64 {
	if p == 0 && r == 0 {
		return 0
	}
	beta2 := beta * beta
	return (1 + beta2) * p * r / (beta2*p + r)
}

// QuestionScore is one question's retrieval scores. Precision, Recall,
// FBeta and IoU come from ScoreQuestion's character-overlap comparison of
// retrieved ranges against reference ranges. Hit and ReciprocalRank are
// block-level: whether the target blob appeared in the top-k blobs, and
// its reciprocal rank there (0 when it did not). ScoreQuestion never sets
// Hit or ReciprocalRank; the repo-driven caller that knows blob identity
// sets them before aggregating.
type QuestionScore struct {
	ID             string  `json:"id"`
	Precision      float64 `json:"precision"`
	Recall         float64 `json:"recall"`
	FBeta          float64 `json:"fBeta"`
	IoU            float64 `json:"iou"`
	Hit            bool    `json:"hit"`
	ReciprocalRank float64 `json:"reciprocalRank"`
}

// ScoreQuestion scores one question's retrieval by rune-level overlap. Both
// refs and retrieved are unioned first, so overlapping retrieved chunks
// (SnapVault's chunks intentionally overlap by design) are never double
// counted:
//
//	matched    = OverlapLen(retrieved, refs)
//	Precision  = matched / |retrieved|   (0 when retrieved is empty)
//	Recall     = matched / |refs|        (0 when refs is empty)
//	IoU        = matched / (|retrieved| + |refs| - matched)
//
// where |x| is the number of runes covered by the unioned ranges x.
func ScoreQuestion(id string, refs, retrieved []Range, beta float64) QuestionScore {
	unionRefs := UnionRanges(refs)
	unionRetrieved := UnionRanges(retrieved)
	matched := OverlapLen(unionRetrieved, unionRefs)

	refsLen := rangesLen(unionRefs)
	retrievedLen := rangesLen(unionRetrieved)

	var precision, recall, iou float64
	if retrievedLen > 0 {
		precision = float64(matched) / float64(retrievedLen)
	}
	if refsLen > 0 {
		recall = float64(matched) / float64(refsLen)
	}
	if union := retrievedLen + refsLen - matched; union > 0 {
		iou = float64(matched) / float64(union)
	}

	return QuestionScore{
		ID:        id,
		Precision: precision,
		Recall:    recall,
		FBeta:     FBeta(precision, recall, beta),
		IoU:       iou,
	}
}

// rangesLen returns the number of runes covered by rs, which must already
// be disjoint (UnionRanges output).
func rangesLen(rs []Range) int {
	var n int
	for _, r := range rs {
		n += r.End - r.Start
	}
	return n
}

// Stat is a metric's mean and population standard deviation over a set of
// questions.
type Stat struct {
	Mean float64 `json:"mean"`
	Std  float64 `json:"std"`
}

// Summary is one metric bundle: N questions it was computed over, and the
// mean/std of each metric across them.
type Summary struct {
	N         int  `json:"n"`
	Precision Stat `json:"precision"`
	Recall    Stat `json:"recall"`
	FBeta     Stat `json:"fBeta"`
	IoU       Stat `json:"iou"`
	RecallAtK Stat `json:"recallAtK"`
	MRR       Stat `json:"mrr"`
}

// Report is a full eval run: the embedder and parameters it ran with, an
// overall Summary, a Summary per tag, and every question's raw score.
type Report struct {
	EmbedderID string             `json:"embedderID"`
	K          int                `json:"k"`
	Beta       float64            `json:"beta"`
	Overall    Summary            `json:"overall"`
	ByTag      map[string]Summary `json:"byTag,omitempty"`
	Questions  []QuestionScore    `json:"questions"`
}

// lowConfidenceN is the sample size below which a tag's Summary is
// annotated as low confidence in WriteTable: with too few questions its
// mean is too noisy to compare across runs.
const lowConfidenceN = 5

// Aggregate summarizes scores into a Report. tags maps a question ID to
// its tags (typically Question.Tags read alongside the score); Report.ByTag
// holds one Summary per tag, computed over the scores whose ID carries
// that tag. Every statistic is macro-averaged: the mean and population
// standard deviation over questions, never a per-rune weighted average.
func Aggregate(scores []QuestionScore, tags map[string][]string, embedderID string, k int, beta float64) Report {
	byTag := make(map[string][]QuestionScore)
	for _, s := range scores {
		for _, tag := range tags[s.ID] {
			byTag[tag] = append(byTag[tag], s)
		}
	}

	report := Report{
		EmbedderID: embedderID,
		K:          k,
		Beta:       beta,
		Overall:    summarize(scores),
		Questions:  scores,
	}
	if len(byTag) > 0 {
		report.ByTag = make(map[string]Summary, len(byTag))
		for tag, ts := range byTag {
			report.ByTag[tag] = summarize(ts)
		}
	}
	return report
}

// summarize computes a Summary over scores.
func summarize(scores []QuestionScore) Summary {
	n := len(scores)
	if n == 0 {
		return Summary{}
	}

	precision := make([]float64, n)
	recall := make([]float64, n)
	fbeta := make([]float64, n)
	iou := make([]float64, n)
	hit := make([]float64, n)
	reciprocalRank := make([]float64, n)
	for i, s := range scores {
		precision[i] = s.Precision
		recall[i] = s.Recall
		fbeta[i] = s.FBeta
		iou[i] = s.IoU
		if s.Hit {
			hit[i] = 1
		}
		reciprocalRank[i] = s.ReciprocalRank
	}

	return Summary{
		N:         n,
		Precision: stat(precision),
		Recall:    stat(recall),
		FBeta:     stat(fbeta),
		IoU:       stat(iou),
		RecallAtK: stat(hit),
		MRR:       stat(reciprocalRank),
	}
}

// stat returns the mean and population standard deviation of vals, which
// must be non-empty.
func stat(vals []float64) Stat {
	n := float64(len(vals))
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / n

	var sumSquares float64
	for _, v := range vals {
		d := v - mean
		sumSquares += d * d
	}
	return Stat{Mean: mean, Std: math.Sqrt(sumSquares / n)}
}

// WriteTable writes a human-readable summary table to w: a header line
// naming the embedder, k and beta the report ran with, the overall
// Summary, then one line per tag in sorted order. A tag summary computed
// over fewer than lowConfidenceN questions is marked "low confidence".
func (r Report) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "embedder=%s k=%d beta=%.2f\n\n", r.EmbedderID, r.K, r.Beta)
	writeSummaryLine(w, "overall", r.Overall)

	if len(r.ByTag) == 0 {
		return
	}
	fmt.Fprintln(w)
	tags := make([]string, 0, len(r.ByTag))
	for tag := range r.ByTag {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		writeSummaryLine(w, tag, r.ByTag[tag])
	}
}

// writeSummaryLine writes one Summary's metrics as a table row labeled
// label, flagging low sample sizes per WriteTable's doc comment.
func writeSummaryLine(w io.Writer, label string, s Summary) {
	flag := ""
	if s.N < lowConfidenceN {
		flag = fmt.Sprintf(" (n=%d, low confidence)", s.N)
	}
	fmt.Fprintf(w, "%-10s n=%-4d precision=%.3f recall=%.3f fbeta=%.3f iou=%.3f recall@k=%.3f mrr=%.3f%s\n",
		label, s.N, s.Precision.Mean, s.Recall.Mean, s.FBeta.Mean, s.IoU.Mean, s.RecallAtK.Mean, s.MRR.Mean, flag)
}

// WriteJSON writes r to w as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
