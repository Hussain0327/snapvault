# Autoretrieval loop

You are an autonomous agent tuning SnapVault's retrieval pipeline.
This document is the whole of your mandate: follow it exactly, in order,
and stop where it tells you to stop.

Everything below assumes your working directory is the repository root
(the directory containing this `docs/` folder and the top-level
`Makefile`), and that `go/build/snapvault` has already been built (`make
go` if not).

## 1. Read before you touch anything

Read, in full:

- The "Package `search`" > "`pipeline.go`" section of
  `docs/superpowers/specs/2026-08-21-hybrid-search-eval-design.md` — the
  binding contract for what every `Pipeline` field means and its valid
  range.
- `go/internal/search/pipeline.go` itself, as it stands right now.

Do not start editing until you can state, for the change you are about to
try, which field it touches and why you expect that field to move
`FBeta@10` for the `static` embedder.

## 2. Edit only `pipeline.go`

You may change:

- Any default constant's value (`defaultChunkRunes`, `defaultOverlapRunes`,
  `defaultK1`, `defaultRRFK`, and so on).
- New logic, so long as it stays entirely inside `pipeline.go` (for
  example, a smarter `IndexHash` canonical form, if you also update its doc
  comment to match).

You may not touch any other file: not `chunk.go`, `bm25.go`, `fuse.go`,
`search.go`, `index.go`, `embed.go`, nor anything outside
`go/internal/search/`.
If a change you want requires touching another file, it is out of scope
for this loop — stop and leave a note in `experiments.md` explaining what
you wanted and why you couldn't do it here, instead of making the edit
anyway.

Make one change at a time.
Do not batch several untested ideas into one edit — you cannot attribute a
result to a specific change if you did, and neither can the human who
reads `experiments.md` later.

## 3. Capture a baseline, then measure your change

Before editing, capture the current numbers:

```console
$ cd go && go build -o build/snapvault ./cmd/snapvault && cd ..
$ ./go/build/snapvault eval run \
    --corpus tests/golden/search/corpus \
    --questions tests/golden/search/questions.jsonl \
    --embedder static --json > /tmp/autoretrieval-before.json
```

Make your one edit to `go/internal/search/pipeline.go`, then measure again:

```console
$ cd go && go build -o build/snapvault ./cmd/snapvault && cd ..
$ ./go/build/snapvault eval run \
    --corpus tests/golden/search/corpus \
    --questions tests/golden/search/questions.jsonl \
    --embedder static --json > /tmp/autoretrieval-after.json
```

Compare `.overall.fBeta.mean` (the headline `FBeta@10`) and
`.byTag.<tag>.recallAtK.mean` (each tag's `Recall@10`) between the two
JSON files, for example with `jq`:

```console
$ jq '.overall.fBeta.mean,
      (.byTag | to_entries[] | "\(.key): \(.value.recallAtK.mean)")' \
    /tmp/autoretrieval-before.json /tmp/autoretrieval-after.json
```

## 4. The gate

A change is a **keep** only if both hold:

1. `make test-go` exits 0 (gofmt clean, `go vet` clean, every unit test
   passes — this also re-runs `go/internal/search`'s own tests, so a
   `Pipeline.Validate` regression or a broken `IndexHash` canonical form
   is caught here, before you ever look at retrieval numbers).
2. `make eval`'s headline `FBeta@10` for `--embedder static` improved by
   **at least 0.005** over the baseline you captured in step 3, **and** no
   single tag's `Recall@10` (`lexical`, `semantic`, or `hard`) is **more
   than 0.02 lower** than its baseline value.

Run both:

```console
$ make test-go
$ make eval
```

`make test-go` must print no failures.
`make eval` prints both embedders' tables; read the `--embedder static`
one (or reuse the JSON files from step 3) against the thresholds above.

Any other outcome — `make test-go` fails, `FBeta@10` improves by less than
0.005, `FBeta@10` regresses, or any tag's `Recall@10` drops by more than
0.02 — is a **discard**.
Revert `pipeline.go` to the version you started this experiment with
before moving on to the next idea, so every experiment starts from the
last kept state, never from a discarded one.

## 5. Record every experiment

Whether kept or discarded, append one row to `experiments.md`'s table
before doing anything else: date, the change (field and old -> new value,
or a one-line description of new logic), the before and after headline
`FBeta@10` and each tag's `Recall@10`, and `keep` or `discard`.
Record discards too — a documented dead end saves the next run (or the
next human) from repeating it.

## 6. On keep, update the baseline — and never commit

If the gate passed:

- Update `tests/golden/search/baseline.json`'s `"static:..."` entry's
  `"fbeta@10"` to the new measured value, so the next run's floor reflects
  the improvement you just proved.
  The embedder id's digest suffix (`@<12 hex chars>`) comes from `jq -r
  .embedderID /tmp/autoretrieval-after.json` — copy it exactly, since it
  is the installed model's own content hash, not something to type from
  memory.
- Leave the edited `go/internal/search/pipeline.go` in the working tree.

**Never run `git add`, `git commit`, or any other git write command.**
This loop's job ends with a clean, kept change (or a documented discard)
sitting in the working tree; a human reviews and commits it.
If you are ever unsure whether an action counts as a commit, do not take
it — stop and leave the working tree as it is for the human to inspect.
