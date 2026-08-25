# Hybrid search, retrieval eval, and the autoretrieval loop

Status: approved design, 2026-08-21.
This is the binding contract for the implementation.
Where code and this document disagree during implementation, fix one of them
and say which.

## Goals

1. Measure retrieval quality of `snapvault find` with a character-overlap
   precision / recall / F-beta harness, a committed golden corpus and question
   set that runs in CI, and an Ollama-backed generator for a user's own files.
2. Add real semantic retrieval that is offline and pure Go: a Model2Vec static
   embedding model (`minishlab/potion-base-8M`, MIT) ported as tokenizer +
   lookup table + mean pool, fused with BM25 by reciprocal rank fusion.
3. Scaffold an autonomous tuning loop: `make eval`, one tunables file, and
   `docs/autoretrieval/program.md`.

## Non-goals

- No change to the object store, `docs/FORMAT.md` normative sections, Java,
  or C++.
  The index stays a non-normative Go sidecar that `fsck` ignores.
- No cgo, no Python, no daemon requirement.
  `golang.org/x/text` is the only new module dependency.
- No approximate nearest-neighbour index, incremental indexing, re-ranker,
  or Java port of the static embedder.
- No bit-exact parity with numpy or the Rust tokenizer's one-to-many lower
  casing; see "Known divergences".
- The default embedder stays `builtin` until the eval justifies a change.

## Packages and ownership

| Path | Status | Owner during the parallel phase |
| --- | --- | --- |
| `go/internal/search/` | extended | agent A1 (sole writer of this package) |
| `go/internal/model2vec/` | new | agent A2 |
| `go/internal/eval/` | new | agent A4 (pure parts), integration for repo-driven parts |
| `tests/golden/search/` | new | agent A3 |
| `go/internal/repo/search.go`, `go/internal/cli/cli.go`, docs, Makefile, CI | changed | integration phase only |

Dependency direction: `cli -> repo -> search -> model2vec`; `eval -> repo, search`.
`search` never imports `repo`; `model2vec` imports nothing from this module.
`go.mod` is edited only by A2 (adding `golang.org/x/text`).

All Go follows the Google Go style guide
(`~/.claude/skills/google-go/SKILL.md`) and the repo's existing conventions:
lowercase `errors.New` / `fmt.Errorf("...: %w")` messages without trailing
punctuation, hand-rolled CLI flag parsing (`--x v` and `--x=v`),
`usageError` exits 2, other errors exit 1, gofmt and `go vet` clean.

## Package `search` (A1)

### `pipeline.go`

```go
// Pipeline holds every retrieval tunable. Index-time fields are baked into an
// index and hashed into its header; query-time fields apply at search time
// and never require a rebuild. This is the only file the autoretrieval loop
// edits.
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

func DefaultPipeline() Pipeline
func (p Pipeline) Validate() error   // ranges: ChunkRunes > OverlapRunes >= 0, etc.
func (p Pipeline) IndexHash() string // 16 hex chars of sha256 over the
                                     // index-time fields in the canonical form
                                     // "chunk=1200;overlap=200;lookback=200;snippet=160;stopwords=true"
```

`IndexHash` must change when any index-time field changes and must not change
when any query-time field changes.

### `chunk.go`

```go
type Chunk struct {
    Sequence  int
    StartRune int // offset into the extracted text, inclusive
    EndRune   int // exclusive; []rune(text)[StartRune:EndRune] == Text
    Text      string
    Snippet   string
}

func ChunkText(text string, p Pipeline) []Chunk
```

Offsets are recomputed after the existing `strings.TrimSpace`, so the
invariant above holds exactly.
Offsets are into the **extracted** text (`Extract`'s output), never blob
bytes.

### Terms

```go
// Terms lowercases and splits text like the lexical embedder does, dropping
// stopwords when requested. It is the tokenizer for BM25 and for query terms.
func Terms(text string, stopwords bool) []string
```

It reuses the existing `tokenize` and `lexicalStopwords`.

### `index.go` — SVX2

Same path as today: `.snapvault/index/embeddings.svi`.
All integers are big-endian; all strings are `int32 byteLen` + UTF-8 bytes.

```text
magic      "SVX2"
embedderID string
dim        int32            1 <= dim <= 65536
indexHash  string           Pipeline.IndexHash() at build time
termCount  int32            0 <= termCount <= 1<<24
terms      string * termCount          (the string table, unique, in first-seen order)
chunkCount int32            0 <= chunkCount; bounded by remaining bytes
chunk * chunkCount:
  blobID    byte[32]
  sequence  int32
  startRune int32           0 <= startRune <= endRune
  endRune   int32
  snippet   string
  tfCount   int32           0 <= tfCount <= termCount
  tf * tfCount:
    term    int32           0 <= term < termCount
    count   int32           count >= 1
  vector    float32 * dim
```

Decoder limits match SVX1's discipline: every length is checked against the
remaining input before any allocation, every string must be valid UTF-8 of at
most `maxTextBytes`, and any violation is an error naming the field.
A file whose magic is `"SVX1"` decodes to the exported sentinel
`ErrIndexOutdated`; any other magic is "not a SnapVault search index".

Worked example, an index with one term and one chunk, dim 2, vector
`(1.0, 0.0)`:

```text
53 56 58 32                      "SVX2"
00 00 00 06  62 75 69 6c 74 69 6e  embedderID "builtin" (shortened for the example)
00 00 00 02                      dim 2
00 00 00 10  <16 ASCII hex>      indexHash
00 00 00 01                      termCount 1
00 00 00 03  63 61 74            terms[0] "cat"
00 00 00 01                      chunkCount 1
<32 bytes>                       blobID
00 00 00 00                      sequence 0
00 00 00 00                      startRune 0
00 00 00 03                      endRune 3
00 00 00 03  63 61 74            snippet "cat"
00 00 00 01                      tfCount 1
00 00 00 00  00 00 00 01         term 0, count 1
3f 80 00 00  00 00 00 00         vector (1.0, 0.0)
```

Go types:

```go
type TermFreq struct{ Term, Count int32 }

type Entry struct {
    BlobID    string
    Sequence  int32
    StartRune int32
    EndRune   int32
    Snippet   string
    Terms     []TermFreq
    Vector    []float32
}

type Index struct {
    EmbedderID string
    Dim        int32
    IndexHash  string
    Terms      []string
    Entries    []Entry
}

func NewIndex(embedderID string, dim int, indexHash string) *Index
// Add interns terms into idx.Terms and appends one Entry.
func (idx *Index) Add(blobID string, c Chunk, vector []float32, terms []string)
func Write(path string, idx *Index) error   // atomic temp + rename, as today
func Read(path string) (*Index, error)
var ErrIndexOutdated = errors.New("search index was built by an older SnapVault; run 'snapvault index' to rebuild")
```

Tests: round trip, SVX1 bytes -> `ErrIndexOutdated`, each bound violated in
isolation, a `FuzzDecodeIndex` fuzz target that asserts no panic and no
allocation larger than the input.

### `bm25.go`

```go
type BM25 struct{ /* postings per term, chunk lengths, average length */ }
func NewBM25(idx *Index, k1, b float64) *BM25
// Scores returns one score per idx.Entries; chunks sharing no term score 0.
func (m *BM25) Scores(queryTerms []string) []float32
```

Standard BM25: `idf = ln(1 + (N - df + 0.5) / (df + 0.5))`,
`score = sum idf * tf*(k1+1) / (tf + k1*(1 - b + b*len/avglen))` where `len`
is the chunk's total term count (sum of `Count`).
Tested by hand against a three-chunk toy index.

### `fuse.go`

```go
// DenseScores returns cosine(query, entry.Vector) per entry.
func DenseScores(idx *Index, query []float32) []float32
// RankByScore returns entry indices with score > 0 in descending score order,
// ties broken by ascending index.
func RankByScore(scores []float32) []int
// FuseRRF returns per-entry fused scores: sum over rankings of 1/(k + rank),
// rank counted from 1; entries absent from a ranking contribute 0 for it.
func FuseRRF(n, k int, rankings ...[]int) []float32
// FuseAlpha min-max normalises each input over entries with score > 0 and
// returns alpha*dense + (1-alpha)*lexical.
func FuseAlpha(alpha float64, dense, lexical []float32) []float32
```

### `search.go`

```go
type Result struct {
    BlobID    string
    Sequence  int32
    StartRune int32
    EndRune   int32
    Snippet   string
    Score     float32
}

// Rank embeds query, scores every entry densely and lexically per p, fuses,
// and returns every entry with a fused score > 0 in descending order.
// Ties break by ascending BlobID then Sequence. allow, when non-nil, drops
// entries before ranking (find uses it to hide unreachable blobs).
func Rank(p Pipeline, embedder Embedder, idx *Index, query string, allow func(blobID string) bool) ([]Result, error)
// GroupByBlob keeps the first (best) result per blob, preserving order.
func GroupByBlob(results []Result) []Result
```

The old `Search`/`TopK` are removed; `find` is `Rank` -> `GroupByBlob` ->
truncate to `limit`, and `eval` consumes `Rank` directly.

### `embed.go`

`NewEmbedder` additionally accepts `"static:<name>@<digest12>"` and builds a
`StaticEmbedder` (added in `static.go` during integration, wrapping
`model2vec.Model`).
It errors if the installed model's digest prefix differs from the id's.

## Package `model2vec` (A2)

Pure Go Model2Vec inference plus the model registry and downloader.
Adds `golang.org/x/text/unicode/norm` to `go.mod`.

```go
type Model struct{ /* vocab, embeddings, config */ }

// Load reads config.json, tokenizer.json and model.safetensors from dir.
func Load(dir string) (*Model, error)
func (m *Model) Dim() int
func (m *Model) Digest() string      // sha256 hex of model.safetensors, computed by Load
func (m *Model) Tokenize(text string) []int  // ids after [UNK] removal and truncation
func (m *Model) Embed(text string) []float32 // mean-pooled; L2-normalised when config says
```

### Model files

- `config.json`: read `normalize` (bool, default false) and `hidden_dim`
  (must equal the tensor's second dimension).
  Ignore `apply_pca` and `apply_zipf` (distillation-time provenance).
- `tokenizer.json`: require `normalizer.type == "BertNormalizer"`,
  `pre_tokenizer.type == "BertPreTokenizer"`, `model.type == "WordPiece"`.
  Read `clean_text`, `handle_chinese_chars`, `strip_accents` (null means
  "same as lowercase"), `lowercase`, `model.vocab` (token -> id),
  `model.unk_token`, `model.continuing_subword_prefix`,
  `model.max_input_chars_per_word`.
  Anything else is an error naming the unsupported component.
  `post_processor` is ignored: inference never adds `[CLS]`/`[SEP]`.
- `model.safetensors`: 8-byte little-endian header length, JSON header, raw
  data.
  Require exactly one tensor named `embeddings`, dtype `F32`, shape
  `[vocabSize, dim]`, little-endian floats.
  Error if `weights` or `mapping` tensors are present (vocabulary-quantised
  models are unsupported).
  The real file's header is
  `{"embeddings":{"dtype":"F32","shape":[29528,256],"data_offsets":[0,30236672]}}`
  (header length 80); unit-test the parser against those literal bytes.

### Inference algorithm

1. Pre-truncate the input to `512 * medianTokenLength` runes, where
   `medianTokenLength` is the integer median of the rune lengths of all vocab
   strings.
2. Normalise, if `clean_text`: drop U+0000, U+FFFD and every rune in Unicode
   categories Cc, Cf, Cn, Co except `\t`, `\n`, `\r`; map every whitespace
   rune (`unicode.IsSpace` or `\t\n\r`) to a single space.
3. If `handle_chinese_chars`: surround each rune in U+4E00–9FFF, 3400–4DBF,
   20000–2A6DF, 2A700–2B73F, 2B740–2B81F, 2B920–2CEAF, F900–FAFF,
   2F800–2FA1F with a space on each side.
4. If `strip_accents` (or `lowercase` when it is null): NFD-decompose and drop
   runes in category Mn.
5. If `lowercase`: `unicode.ToLower` per rune.
6. Pre-tokenise: split on whitespace (removed), then isolate every rune that
   is ASCII punctuation (0x21–0x2F, 0x3A–0x40, 0x5B–0x60, 0x7B–0x7E) or in
   Unicode category P as its own word.
7. WordPiece per word: if the word has more than `max_input_chars_per_word`
   runes it becomes `[UNK]`; otherwise greedy longest-match from the left,
   candidates after the first prefixed with `##`; if any position fails to
   match, the whole word becomes a single `[UNK]`.
8. Drop every `[UNK]` id; truncate to 512 ids.
9. If no ids remain return a zero vector of `Dim()` length.
   Otherwise mean-pool the rows in token order, accumulating each dimension in
   float64 and rounding to float32 once.
10. If `normalize`, divide by the L2 norm (no epsilon; the vector is non-zero).

### Known divergences

Rust's `char::to_lowercase` is one-to-many for a few code points (U+0130);
Go's is one-to-one.
numpy uses pairwise summation for 128 or more tokens; Go accumulates in
float64.
Both are documented in the package comment and are why the integration test
uses a tolerance of 1e-6 rather than equality.

### Registry and download

```go
type FileSpec struct{ Name, SHA256 string; Size int64 }
type Spec struct {
    Name     string // "potion-base-8M"
    Repo     string // "minishlab/potion-base-8M"
    Revision string // pinned commit hash
    Dim      int
    Files    []FileSpec // config.json, tokenizer.json, model.safetensors
}
var Registry = map[string]Spec{ /* potion-base-8M */ }

// Dir returns $SNAPVAULT_MODEL_DIR/<name> or os.UserCacheDir()/snapvault/models/<name>.
func Dir(name string) (string, error)
// Pull downloads every file of spec from baseURL (default https://huggingface.co)
// as <repo>/resolve/<revision>/<file> into dir, streaming to a .tmp file,
// verifying SHA-256 before renaming, and leaving nothing behind on failure.
func Pull(ctx context.Context, spec Spec, baseURL, dir string, progress io.Writer) error
// Verify checks every file of spec exists in dir with the pinned SHA-256.
func Verify(spec Spec, dir string) error
```

The pinned revision and every SHA-256 are obtained by downloading the files
during implementation and hashing them; they are never typed from memory.
`Pull` is tested against an `httptest.Server`; a wrong hash must leave no
file in `dir`.

### Tests

All unit tests are offline and build a **tiny model in `t.TempDir()`** with a
hand-written vocab (about 50 entries including `[PAD] [UNK] [CLS] [SEP] [MASK]`
at ids 0–4, plain words, `##` continuations, an accented word, one CJK
character) and an 8-dimensional `embeddings` tensor written by a test helper.
Table-driven cases: empty, whitespace only, lone punctuation, a word longer
than `max_input_chars_per_word`, an all-`[UNK]` sentence (zero vector),
mixed CJK and ASCII, accented input, subword splitting, and determinism (two
calls return identical bytes).
A separate test, skipped when the real model is not installed, loads
`Dir("potion-base-8M")` and compares `Embed` against
`testdata/reference_vectors.json` (generated once, out of band, with the
Python `model2vec` package; the file's header comment says so) within 1e-6,
and checks the sha256 of the concatenated outputs against a golden hash.

## Package `eval` (A4 for the pure parts)

### Question file

JSON Lines, one object per line:

```json
{"id": "q001", "question": "What temperature does the bread bake at?", "path": "recipes/sourdough.md", "highlight": "Bake at 230 °C for 35 minutes", "tags": ["lexical"]}
```

`path` is relative to the repository root at `HEAD`.
`highlight` must occur **exactly once** in the extracted text of that file.

```go
type Question struct {
    ID        string   `json:"id"`
    Question  string   `json:"question"`
    Path      string   `json:"path"`
    Highlight string   `json:"highlight"`
    Tags      []string `json:"tags"`
}

func LoadQuestions(r io.Reader) ([]Question, error) // required fields, unique ids
func Sample(qs []Question, pct int) []Question        // fixed seed; pct 100 returns qs

type Range struct{ Start, End int } // rune offsets, half-open

var ErrHighlightNotFound = errors.New("highlight not found in extracted text")
type AmbiguousHighlightError struct{ Offsets []int } // rune offsets of every match

func LocateHighlight(text, highlight string) (Range, error)
```

Both location failures are hard errors reported with the question id and path
before any retrieval runs; a question is never silently skipped or scored 0.

### Metrics

```go
func UnionRanges(rs []Range) []Range                   // sorted, merged (touching ranges merge)
func OverlapLen(a, b []Range) int                      // both already unioned
func FBeta(p, r, beta float64) float64                 // 0 when p == r == 0

type QuestionScore struct {
    ID                                string
    Precision, Recall, FBeta, IoU     float64
    Hit                               bool    // target blob in top-k blobs
    ReciprocalRank                    float64 // 1/rank or 0
}

// ScoreQuestion unions both sides, then
//   matched = OverlapLen(retrieved, refs)
//   P = matched/len(retrieved), R = matched/len(refs), IoU = matched/(|retrieved|+|refs|-matched).
func ScoreQuestion(id string, refs, retrieved []Range, beta float64) QuestionScore

type Stat struct{ Mean, Std float64 }
type Summary struct {
    N                                          int
    Precision, Recall, FBeta, IoU, RecallAtK, MRR Stat
}
type Report struct {
    EmbedderID string
    K          int
    Beta       float64
    Overall    Summary
    ByTag      map[string]Summary
    Questions  []QuestionScore
}

func Aggregate(scores []QuestionScore, tags map[string][]string, embedderID string, k int, beta float64) Report
func (r Report) WriteTable(w io.Writer)  // overall, then per tag with N; tags with N < 5 marked
func (r Report) WriteJSON(w io.Writer) error
```

Macro-averaging only: every statistic is the mean and population standard
deviation over questions.
Defaults: `k = 10`, `beta = 2.0`.
Unioning **both** sides is deliberate: the Python reference sums raw retrieved
lengths and double-counts SnapVault's 200-rune chunk overlap.

### Generator (pure part)

```go
// GenerateForText asks a local Ollama model for n question/quote pairs about
// text (POST {baseURL}/api/generate with "format": "json"), keeps only pairs
// whose quote is a unique substring of text, and returns them with path set.
func GenerateForText(ctx context.Context, client *http.Client, baseURL, model, path, text string, n int) ([]Question, int /* dropped */, error)
```

The prompt asks for JSON `{"pairs":[{"question":..., "quote":...}]}` where
each quote is copied verbatim from the text, 40–300 characters.
Tested with `httptest`.

### Repo-driven parts (integration phase)

```go
// Run resolves every question's path at HEAD, extracts text, locates
// highlights, ranks with search.Rank through a repo.Searcher, and aggregates.
func Run(ctx context.Context, r *repo.Repository, p search.Pipeline, embedder search.Embedder, qs []Question, k int, beta float64) (Report, error)
// Generate walks HEAD and calls GenerateForText per text blob.
func Generate(ctx context.Context, r *repo.Repository, baseURL, model string, perFile int, out io.Writer) (written, dropped int, err error)
```

`Run` computes chunk metrics from the chunk-level ranking, filtered to
chunks belonging to the question's own target blob and then truncated to
`k` chunks, and `Hit`/`ReciprocalRank` from `GroupByBlob` of the
unfiltered ranking truncated to `k` blobs. The filter is load-bearing: a
retrieved chunk's `StartRune`/`EndRune` are offsets into whatever blob it
came from, not into the target blob's text, so scoring an unfiltered
ranking would union and overlap ranges from unrelated documents as if they
shared one coordinate space — inflating recall (even to 1.0) for a
question whose target document was never retrieved at all.

## Package `repo` (integration)

```go
func (r *Repository) Index(embedder search.Embedder, p search.Pipeline) (IndexStats, error)

// Searcher holds the lock, the decoded index and the reachable-blob map for
// many queries; eval runs a hundred queries without re-walking history.
type Searcher struct{ /* ... */ }
func (r *Repository) OpenSearcher(p search.Pipeline) (*Searcher, error)
func (s *Searcher) EmbedderID() string
func (s *Searcher) IndexHash() string
func (s *Searcher) Rank(query string) ([]search.Result, error) // reachable blobs only
func (s *Searcher) Find(query string, limit int) ([]FindResult, error)
func (s *Searcher) Locate(blobID string) (BlobLocation, bool)
func (s *Searcher) Close() error

type FindResult struct {
    BlobID, Path, CommitID, Message, Snippet string
    Sequence int32
    Score    float32
}
```

`Repository.Find` stays as a thin wrapper (open, find, close).
`OpenSearcher` maps `search.ErrIndexOutdated` and a missing file to errors
whose text ends in "run 'snapvault index' first".
When the index's `IndexHash` differs from `p.IndexHash()`, `find` prints one
line to stderr: `note: index was built with different chunking settings; run 'snapvault index' to rebuild`.

## CLI (integration)

```text
snapvault index [--embedder builtin|static|static:<name>|ollama:<model>]
snapvault find <query> [--limit N]
snapvault model pull <name>
snapvault model list
snapvault eval run --questions <file> [--corpus <dir>] [--embedder X] [-k 10] [--beta 2.0] [--pct 100] [--json]
snapvault eval generate --model <ollama-model> --out <file> [--per-file 2]
```

`--embedder static` means `static:potion-base-8M`.
If the model is not installed the error is
`model potion-base-8M is not installed; run 'snapvault model pull potion-base-8M'`.
`eval run --corpus <dir>` copies the directory into a temporary directory,
runs `repo.Init`, `Snapshot("eval corpus")` and `Index`, evaluates, and
removes the temporary directory.
`model pull` is the only command that opens a network connection; `index`,
`find` and `eval` never do.

## Golden set `tests/golden/search/` (A3)

- `corpus/<domain>/<slug>.md` or `.txt`: about 50 original documents of
  200–700 words across at least eight domains (cooking, meeting notes,
  travel, personal finance, home maintenance, software how-tos, health and
  fitness, letters, product manuals, gardening).
  Original prose written for this fixture; no copied text, no PDFs (PDF
  extraction differs between `pdftotext` and the builtin fallback, which
  would make highlights machine-dependent).
  Several documents per domain share vocabulary so there are distractors.
- `questions.jsonl`: about 100 questions, each tagged exactly one of
  `lexical` (the answer passage shares its key terms with the question),
  `semantic` (the question paraphrases: "automobile" vs "car", "physician"
  vs "doctor"; no content word of the highlight appears in the question),
  `hard` (two documents on the same topic and only one contains the answer,
  or the answer sits mid-document after a long unrelated preamble).
  Roughly 40 / 40 / 20.
  Every `highlight` is copied verbatim from its file and occurs exactly once
  in it; A3 verifies this mechanically before finishing.
- `MANIFEST.md`: what each tag means, how to add a document or question, the
  uniqueness rule, and the line-ending rule.
- `baseline.json`: `{"<embedderID>": {"fbeta@10": <float>}}`, initially
  `{}`; filled in by the first real run.
- `.gitattributes` at the repo root gains `tests/golden/search/** text eol=lf`.

## Tests and CI

- `go/internal/eval/golden_test.go`: `TestGoldenFloorLexical` builds the
  corpus into a temp repo with `builtin`, runs the harness, and asserts
  `FBeta@10 >= baseline["builtin-lexical-v1"]["fbeta@10"]` (skips the
  comparison while the baseline is absent but still asserts every highlight
  locates).
  `TestGoldenFloorStatic` does the same with `static`; it skips when the
  model is not installed unless `SNAPVAULT_REQUIRE_MODEL=1`, in which case a
  missing model fails the test.
- `.github/workflows/ci.yml` gains an `eval` job: `actions/cache` keyed on
  the pinned revision for the model directory, `snapvault model pull`,
  `SNAPVAULT_REQUIRE_MODEL=1 go test ./internal/eval/`, then `make eval`.
  The existing `go` job is unchanged and model-free.
- `Makefile` gains `eval`: build the Go CLI, then run `eval run --corpus
  tests/golden/search/corpus --questions tests/golden/search/questions.jsonl`
  for `--embedder builtin` and `--embedder static`.

## Autoretrieval loop `docs/autoretrieval/`

`program.md` tells an autonomous agent:

1. Read this spec's Pipeline section and `go/internal/search/pipeline.go`.
2. Edit only `pipeline.go` (values, or new logic that stays inside it).
3. Gate: `make test-go` green and `make eval` headline `FBeta@10` for
   `static` improved by at least 0.005 with no tag's `Recall@10` lower by
   more than 0.02.
4. Append every experiment to `experiments.md`: date, change, before and
   after per tag, keep or discard.
5. On keep, update `baseline.json`.
6. Never commit; leave kept changes in the working tree for the human.

## Documentation

- `README.md` search section: hybrid ranking, `model pull`, `eval`, the one
  network call, `SNAPVAULT_MODEL_DIR`.
- `docs/FORMAT.md` "Search index sidecar (non-normative)": mention SVX2 and
  that `fsck` still ignores the directory.
- The version string is not changed.
