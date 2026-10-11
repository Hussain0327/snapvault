# Vector search baseline

Measured on 2026-10-11, before changing the retrieval engine.
This is an exact-search baseline, not a comparison against implemented HNSW,
SIMD, or quantization.

## Reproduce

From `go/`:

```sh
GOMAXPROCS=2 go test ./internal/search ./internal/eval \
    -run '^$' -bench 'Benchmark(ExactSearch|GoldenFind)$' \
    -benchmem -benchtime=200ms -count=3 -cpu=1
```

If the default Go build cache is not writable, set `GOCACHE` to a writable
directory outside the source tree.
For more stable comparisons, increase `-benchtime` and `-count` on an idle
machine and retain the raw output from both implementations.

Environment: Apple M2, darwin/arm64, Go 1.26.5, one benchmark CPU.
Base revision: `ce675a6`.
The working tree contained unrelated Java changes; these benchmarks use Go.

## Synthetic vectors

Each fixture contains normalized Gaussian vectors generated with seed 42.
Queries come from a separate sequence of 32 vectors, selected cyclically.
The dimensions correspond to the static embedder (256) and builtin embedder
(512); the synthetic data does not reproduce either embedder's distribution.
Each entry has one lexical term; half match the hybrid query.
Fixture construction is outside the measured region.

Times below are medians of three benchmark means, in milliseconds per query.
They are not latency percentiles.

| Dimensions | Vectors | Dense scores | Dense ranking | Hybrid ranking |
| --- | ---: | ---: | ---: | ---: |
| 256 | 100 | 0.047 | 0.053 | 0.073 |
| 256 | 1,000 | 0.484 | 0.522 | 0.747 |
| 256 | 10,000 | 4.612 | 5.394 | 8.138 |
| 256 | 50,000 | 23.337 | 45.193 | 42.399 |
| 512 | 100 | 0.100 | 0.104 | 0.125 |
| 512 | 1,000 | 0.991 | 1.063 | 1.439 |
| 512 | 10,000 | 10.196 | 10.601 | 13.037 |
| 512 | 50,000 | 49.650 | 53.854 | 65.959 |

The 256-dimensional, 50,000-vector dense-ranking samples ranged from 28.9 to
63.4 ms, so that median is noisy and should not be compared with the hybrid
median as an algorithmic result.
Short runs at the largest sizes execute only a few queries per sample.
These measurements establish the scale of current costs, not an ANN crossover.

`DenseScores` includes scalar cosine and allocation of the output scores.
`DenseRanking` also sorts every positive score.
`HybridRank` includes BM25 construction and scoring, reciprocal rank fusion,
and result sorting, using a fixed embedder to exclude model inference.
It does not include loading the index, resolving history, or grouping chunks
into files.
Dense ranking returns all positive matches, not top-k; a future ANN comparison
must also include an optimized exact top-k baseline with matching semantics.

At 50,000 vectors, query allocations were approximately 200 KiB for scoring,
1.04 MiB for dense ranking, and 12.0 MiB for hybrid ranking.
These are allocated bytes per operation, not retained index memory or RSS.
The float32 vector payload alone is 48.8 MiB at 256 dimensions and 97.7 MiB at
512 dimensions, excluding Go allocation overhead and search metadata.

## Committed file corpus

`BenchmarkGoldenFind` builds the 50-document golden corpus with the builtin
embedder and cycles through its 100 real questions with a limit of 10 files.
Snapshotting and indexing are outside the measured region.

| Searcher lifecycle | Median ms/query | Allocated MiB/query |
| --- | ---: | ---: |
| Reopen for every query | 3.903 | 2.276 |
| Reuse an open searcher | 0.954 | 0.648 |

Reopen includes decoding the index, acquiring the lock, and walking history.
Reuse keeps those resources open across queries.
Both include embedding the query, ranking, grouping, and resolving results.
The OS file cache is warm; neither includes command process startup.
The fixture has one snapshot, so it does not measure long-history traversal.

## Recommendation

Keep exact search as the default while building an opt-in ANN path.
The small-fixture measurements show little absolute vector-scoring cost;
they do not prove HNSW would be faster or slower.

First compare normalized SIMD scoring and bounded exact top-k selection with
this baseline.
Then compare HNSW using the same query set and top-k contract.
Measure graph construction, index loading, query latency, recall, serialized
bytes, retained memory, and peak build memory separately.
Include both reusable searchers and one-shot queries before choosing any
automatic size threshold.

Int8 scalar quantization is a useful initial compression baseline.
Its vector payload approaches one quarter of float32 storage, but graph links,
scales, lexical metadata, and any retained original vectors must be counted.
Measure actual memory and recall rather than claiming a fourfold total-index
reduction.

## Relationship to TurboQuant

[TurboQuant][turboquant] is a quantization algorithm, while [HNSW][hnsw] is a
graph search algorithm; they address different parts of retrieval.
The TurboQuant paper describes a random rotation and scalar quantization,
with a QJL residual stage for unbiased inner-product estimation.
Ordinary int8 quantization would not be a TurboQuant implementation.

The [Google Research article][article] reports results for particular models,
datasets, and hardware.
Its H100 attention speedups do not predict Apple M2 file-search performance.
Lossy quantization does not guarantee identical nearest-neighbor ordering.
A later paper-faithful implementation needs independent distortion and recall
checks against uncompressed vectors.

## Validation

Both added benchmarks compiled and completed all three repetitions.
`go test ./internal/search ./internal/eval -count=1 -v` passed, including both
golden retrieval floors, with no skipped tests.
`go vet ./internal/search ./internal/eval` and formatting checks passed.
The test run used a writable temporary Go cache and permission to bind local
HTTP test servers; the restricted sandbox run failed on those environment
constraints before completing.
No production search behavior changed.
The Java, C++, and full Go suites were not run for this benchmark-only change.

[turboquant]: https://arxiv.org/abs/2504.19874
[hnsw]: https://arxiv.org/abs/1603.09320
[article]: https://research.google/blog/turboquant-redefining-ai-efficiency-with-extreme-compression/
