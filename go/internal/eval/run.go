package eval

import (
	"context"
	"fmt"

	"github.com/Hussain0327/snapvault/go/internal/repo"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

// Run resolves every question's path in r's tree at HEAD, extracts its
// text, locates the question's highlight in it, ranks the question against
// r's search index through a repo.Searcher, and aggregates the results into
// a Report.
//
// r must already carry a search index built with embedder and p (r.Index,
// typically run by the caller just before Run) — Run only opens and queries
// it; it never (re)builds one. embedder identifies that index for the
// returned Report and must match the embedder the index on disk was built
// with. Run enforces that invariant itself (repo.OpenSearcher only stores
// the index's IndexHash for a caller to compare; find's own check of it is
// a soft note on stderr, not an error, which is not strict enough for a
// measurement tool): p must also match the index's IndexHash, or Run fails
// rather than silently scoring against a stale index built with different
// chunking settings.
//
// A question whose path does not exist at HEAD, or whose highlight cannot
// be located unambiguously in that path's extracted text, fails the whole
// run immediately with an error naming the question's id and path — per the
// spec, a question is never silently skipped or scored 0.
//
// Precision, Recall, FBeta and IoU come from the chunk-level ranking,
// filtered to the question's own target blob and truncated to the top k
// chunks (a chunk's StartRune/EndRune are offsets into whatever blob it
// came from, so a decoy blob's chunk must never be scored against the
// target blob's reference range as if the two shared one coordinate
// space); Hit and ReciprocalRank come from the unfiltered ranking grouped
// to one result per blob (search.GroupByBlob) and separately truncated to
// the top k blobs.
func Run(
	ctx context.Context,
	r *repo.Repository,
	p search.Pipeline,
	embedder search.Embedder,
	qs []Question,
	k int,
	beta float64,
) (Report, error) {
	s, err := r.OpenSearcher(p)
	if err != nil {
		return Report{}, err
	}
	defer s.Close()
	if s.EmbedderID() != embedder.ID() {
		return Report{}, fmt.Errorf(
			"eval.Run: index was built with embedder %s, but was asked to evaluate %s",
			s.EmbedderID(), embedder.ID())
	}
	if s.IndexHash() != p.IndexHash() {
		return Report{}, fmt.Errorf(
			"eval.Run: index was built with different chunking settings (index hash %s, pipeline hash %s); "+
				"run 'snapvault index' to rebuild before evaluating",
			s.IndexHash(), p.IndexHash())
	}

	paths, err := r.HeadPaths()
	if err != nil {
		return Report{}, err
	}

	tags := make(map[string][]string, len(qs))
	scores := make([]QuestionScore, 0, len(qs))
	for _, q := range qs {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		tags[q.ID] = q.Tags

		score, err := scoreOneQuestion(r, s, paths, q, k, beta)
		if err != nil {
			return Report{}, err
		}
		scores = append(scores, score)
	}

	return Aggregate(scores, tags, embedder.ID(), k, beta), nil
}

// scoreOneQuestion resolves and scores a single question against s, the
// bulk of Run's per-question work factored out so Run itself reads as the
// loop plus its two failure modes.
func scoreOneQuestion(
	r *repo.Repository, s *repo.Searcher, paths map[string]string, q Question, k int, beta float64,
) (QuestionScore, error) {
	blobID, ok := paths[q.Path]
	if !ok {
		return QuestionScore{}, fmt.Errorf("question %s: path %s not found at HEAD", q.ID, q.Path)
	}
	raw, err := r.BlobBytes(blobID)
	if err != nil {
		return QuestionScore{}, fmt.Errorf("question %s: reading %s: %w", q.ID, q.Path, err)
	}
	text, ok := search.Extract(raw)
	if !ok {
		return QuestionScore{}, fmt.Errorf("question %s: %s has no extractable text", q.ID, q.Path)
	}
	ref, err := LocateHighlight(text, q.Highlight)
	if err != nil {
		return QuestionScore{}, fmt.Errorf("question %s (%s): %w", q.ID, q.Path, err)
	}

	ranked, err := s.Rank(q.Question)
	if err != nil {
		return QuestionScore{}, fmt.Errorf("question %s: %w", q.ID, err)
	}

	chunkRanked := ranked
	if len(chunkRanked) > k {
		chunkRanked = chunkRanked[:k]
	}
	// Only chunks of the question's own target blob contribute to the
	// character-overlap metrics: res.StartRune/EndRune are offsets into
	// whatever blob res came from, not into the target blob's text, so a
	// decoy chunk's range must never be unioned or overlapped against ref
	// as if it lived in the same coordinate space.
	var retrieved []Range
	for _, res := range chunkRanked {
		if res.BlobID != blobID {
			continue
		}
		retrieved = append(retrieved, Range{Start: int(res.StartRune), End: int(res.EndRune)})
	}

	score := ScoreQuestion(q.ID, []Range{ref}, retrieved, beta)

	grouped := search.GroupByBlob(ranked)
	if len(grouped) > k {
		grouped = grouped[:k]
	}
	for i, res := range grouped {
		if res.BlobID == blobID {
			score.Hit = true
			score.ReciprocalRank = 1.0 / float64(i+1)
			break
		}
	}
	return score, nil
}
