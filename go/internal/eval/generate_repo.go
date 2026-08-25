package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/repo"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

// generateTimeout bounds one GenerateForText call against the local Ollama
// server. It is longer than search's own embedding-request timeout because
// generating question/quote pairs is a much larger completion than an
// embedding lookup.
const generateTimeout = 120 * time.Second

// Generate walks every text blob reachable at r's HEAD, in path order, and
// calls GenerateForText on each one, writing every kept Question to out as
// one JSON Lines record per line — the same format LoadQuestions reads. A
// blob with no extractable text (per search.Extract) is skipped, the same
// way Repository.Index skips it when building a search index.
//
// It returns the total number of questions written and dropped across every
// blob, or an error naming the path being processed when the Ollama server
// at baseURL fails or returns something unexpected.
func Generate(
	ctx context.Context, r *repo.Repository, baseURL, model string, perFile int, out io.Writer,
) (written, dropped int, err error) {
	paths, err := r.HeadPaths()
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)

	client := &http.Client{Timeout: generateTimeout}
	enc := json.NewEncoder(out)
	for _, path := range names {
		if err := ctx.Err(); err != nil {
			return written, dropped, err
		}

		raw, err := r.BlobBytes(paths[path])
		if err != nil {
			return written, dropped, fmt.Errorf("reading %s: %w", path, err)
		}
		text, ok := search.Extract(raw)
		if !ok {
			continue
		}

		qs, n, err := GenerateForText(ctx, client, baseURL, model, path, text, perFile)
		if err != nil {
			return written, dropped, fmt.Errorf("generating questions for %s: %w", path, err)
		}
		dropped += n
		for _, q := range qs {
			if err := enc.Encode(q); err != nil {
				return written, dropped, fmt.Errorf("writing question for %s: %w", path, err)
			}
			written++
		}
	}
	return written, dropped, nil
}
