# SnapVault

Git-style snapshots for any folder.
Point it at a directory, take snapshots, diff them, restore any of them.
The directory doesn't need to be code and nothing leaves your machine.

The interesting part: it's **one binary format with three implementations**
that all read and write each other's repositories, byte for byte.

```text
snapvault/
├── docs/FORMAT.md    the format spec — the contract everything follows
├── java/             the original implementation (Java 21, one pinned dep)
├── go/               full rewrite with concurrent hashing (Go, one pinned dep)
├── cpp/              snapvault-fsck, an integrity checker (C++20, zlib + zstd)
└── tests/interop.sh  the script that proves they actually interoperate
```

I wrote the Java version first, froze the format, then rebuilt it in Go and
wrote a C++ verifier against the same spec.
If the spec is any good, three codebases in three languages should agree on
every byte.
They do, and CI checks that on every push.

## How storage works

Every file becomes a blob in a content-addressed object store.
Same content, same SHA-256, stored once — no matter how many paths or
snapshots contain it.
Trees capture directory structure (including symlinks, executable bits, and
empty directories), and commits chain the history together.

```text
.snapvault/HEAD
      │
      ▼
refs/heads/main ──► commit ──► parent commit ──► ...
                       │
                       ▼
                   root tree
                    /      \
                subtree    blob
                  │          ▲
                  └──────────┘   equal content = one object
```

Objects are stored as `SHA-256("<type> <size>\0" + payload)`,
zlib-compressed.
Every read inflates the object, checks the declared size, rejects trailing
garbage, and recomputes the digest before trusting anything.

Full details and invariants: [docs/FORMAT.md](docs/FORMAT.md).

### Format v2: delta compression and zstd

Format 1 objects are always a zlib stream of the canonical bytes.
Format 2 adds a second, optional encoding for the same object file: a later
version of a file can be stored as a small delta against an earlier
version instead of a full copy, and either form can be compressed with
zstd instead of zlib.
The sharded one-file-per-object layout does not change; only what is
inside a given object file can change.

```text
rev 1 blob ◄──── rev 2 blob ◄──── rev 3 blob ◄──── ...
(SVO2 full,       (SVO2 delta,     (SVO2 delta,
 zstd bytes)       base = rev 1)    base = rev 2)
```

An object file is now one of two things, and a single byte tells them
apart — a zlib stream always starts `0x78`, and the container magic starts
`0x53` ("S"), so the two can never be confused:

```text
legacy (v1 and v2)          container (v2 only)
┌─────────────────┐         ┌──────┬──────┬───────┬──────────────────────┐
│ zlib( canonical │         │ SVO2 │ kind │ codec │ zlib|zstd( payload ) │
│      bytes )    │         └──────┴──────┴───────┴──────────────────────┘
└─────────────────┘           magic  full   zlib    full  → canonical bytes
                                     delta  zstd    delta → base id (32B)
                                                            + instructions
```

A delta is Git's own pack-delta wire format, not a SnapVault invention: a
short list of "copy N bytes from the base at offset O" and "insert these
literal bytes" instructions.
Reading an object still means reconstructing the full canonical bytes
(walking the base chain if needed, capped at depth 32) and recomputing the
SHA-256 before trusting a single byte, so object ids never change.
Format v2 is purely a smaller way to store the same content.

`.snapvault/format` says `snapvault 1` or `snapvault 2`, and a v1
repository never gets a container-form object written into it.
A v2 repository can hold a mix of legacy (zlib) and container (zlib or
zstd, full or delta) objects side by side, because rewriting is opt-in:

- `snapvault upgrade` flips the format marker from 1 to 2 and rewrites
  nothing else — every existing object is still legal v2 storage as-is,
  and running it twice is a no-op.
- `snapvault repack` is what actually shrinks a v2 repository.
  It groups versions of the same file and re-encodes each object as
  whichever of {current bytes, container-full-zstd, best
  container-delta-zstd} is smallest, rewriting an object only when that
  beats the current file by at least 5%.
  Every rewrite goes to a temp file, gets decoded back and digest-checked,
  then is fsync'd and renamed over the original, so an interrupted repack
  never leaves a corrupt object behind.

I measured `repack` myself on a fixture built for this: a 44 KB plain-text
report, snapshotted 30 times with one small realistic edit before each
snapshot (a line reworded, a note added — the way a report actually
evolves), in a directory outside this repo, using the Go CLI built from
this commit.

```console
$ snapvault -C repo upgrade
Upgraded repository to format 2
$ snapvault -C repo repack
repacked 28 objects: 153.6 KB -> 6.4 KB (96% smaller)
```

Total bytes on disk across every object file, before and after:

```text
before repack   174,446 bytes   (90 objects: 30 blobs, 30 trees, 30 commits)
after repack     23,717 bytes   (86.4% smaller overall)
```

The 96% figure repack prints is the shrink on just the 28 blob objects it
rewrote; 86.4% is the shrink across the whole object store, trees and
commits included.
Thirty near-identical copies of one document is the best case for delta
compression, and that is exactly the case this feature exists for — but it
is one fixture on one machine (an M2 Air), not a universal number.
Real savings depend entirely on how similar your successive versions are:
the interop suite's own fixture, with larger edits, shrinks by 50.5%, and
that suite asserts at least 50% on every CI run so a regression here fails
the build.

The repack took 0.41s.
Afterwards, on that same repository: `snapvault-fsck` reported 0 errors
across all 90 objects, a second `repack` printed "nothing to repack.",
`restore` reproduced the file byte-for-byte, and the **Java** CLI read the
Go-repacked repository and printed "No changes." with an identical log.
Repack's output is exercised across all three languages, not just written
and trusted.

zstd is a build dependency in every language, but not the same dependency:

- **Go** links `github.com/klauspost/compress`, a pure-Go implementation,
  pinned in `go.mod` and `go.sum`.
- **C++** links system `libzstd` (`brew install zstd` on macOS,
  `apt-get install libzstd-dev` on Linux); CMake fails loudly if it can't
  find it.
- **Java** only ever decodes zstd, via a pinned build of
  `io.airlift:aircompressor`, a pure-Java implementation.
  `make -C java deps` downloads that exact pinned jar into the gitignored
  `java/lib/` and verifies its SHA-256 before anything links against it,
  so a mismatched or tampered download fails the build instead of
  silently linking.

## Search: `snapvault find`

`snapvault index` builds a search index over every blob reachable from
every ref; `snapvault find <query>` searches it.
The index is a sidecar at `.snapvault/index/embeddings.svi` (format
"SVX2"): it never touches `objects/`, `fsck` ignores it, and deleting it
only means the next `find` tells you to reindex.
Both commands are Go-only.

Ranking is hybrid.
Every chunk is scored two independent ways and the two rankings are fused,
so keyword queries keep working even against a semantic embedder's index:

```text
index   blob ──► Extract ──► ChunkText ──┬──► embedder ──► vector    ─┐
             (text, PDF)   (1200 runes,   └──► Terms ──► term counts ─┴──► SVX2
                            200 overlap)

find    query ──┬──► embed ──► cosine ──► dense ranking   ──┐
                └──► Terms ──► BM25   ──► lexical ranking ──┤
                                                            ▼
              results ◄── best chunk per file ◄── rank fusion
```

Three embedders are available, chosen with `index --embedder`:

- `builtin` (the default): `builtin-lexical-v1`, a deterministic hashed
  bag-of-words.
  It is not semantic — it will not know that "car" and "automobile" are
  related — but it needs nothing installed and never touches the network.
- `static`: a real offline semantic embedder, a pure-Go port of
  [Model2Vec](https://github.com/MinishLab/model2vec)'s
  `minishlab/potion-base-8M` — a static embedding table plus a WordPiece
  tokenizer, no model runtime required.
  One 30 MB download, then everything is local.
- `ollama:<model>`: for a locally running [Ollama](https://ollama.com).
  SnapVault POSTs to `http://localhost:11434/api/embeddings` for each chunk
  and each query; nothing leaves the machine, since Ollama itself is local.

### Using search

```console
$ make go                                          # builds go/build/snapvault
$ go/build/snapvault model pull potion-base-8M     # once; the only network call
$ go/build/snapvault model list
potion-base-8M  installed  /Users/you/Library/Caches/snapvault/models/potion-base-8M

$ go/build/snapvault -C ~/Documents/notes index --embedder static
indexed 412 blobs (1873 chunks) with static:potion-base-8M@f65d0f325faa

$ go/build/snapvault -C ~/Documents/notes find "when does the car insurance renew"
3726b03a0d95  finance/auto-policy.md  (snapshot: "before cleanup", 9b0084d65380)
    The automobile policy renews on 14 March; the premium is due ten days before ...
```

Reindex (`snapvault index`) after taking new snapshots, when switching
`--embedder`, or when `find` prints a note that the index was built with
different chunking settings.
`find` always resolves each hit to the newest snapshot that still contains
it, so an older index never points at a path that no longer exists.

The model lives under `$SNAPVAULT_MODEL_DIR/<name>` when that variable is
set, otherwise under the OS user cache directory
(`os.UserCacheDir()/snapvault/models/<name>`).
Its revision and every file's SHA-256 are pinned in
`go/internal/model2vec/registry.go` and verified before use, the same way
`java/Makefile` pins its one dependency.

Text extraction covers UTF-8 text and simple PDFs (via `pdftotext` when
it's on `PATH`, otherwise a small builtin extractor); anything else is
skipped and counted as skipped.

## Measuring retrieval quality: `snapvault eval`

"Semantic search" is a claim, so there is a harness to test it.
A question set names, for each question, the exact passage (`highlight`)
of a file that answers it.
The harness runs the question through `find`'s ranking and measures how
much of the retrieved text overlaps that passage, character by character,
plus whether the right file was found at all.

```text
questions.jsonl ──► locate each highlight in ──► reference ranges ─┐
                    the file's extracted text                      ├──► character overlap
corpus dir ──► temp repo ──► index ──► Rank ──► retrieved ranges  ─┘           │
      precision / recall / F-beta / IoU   (chunk level)           ◄────────────┤
      Recall@k / MRR                      (file level)            ◄────────────┘
      overall and per tag: lexical, semantic, hard
```

`tests/golden/search/` is a committed fixture: 50 original documents and
100 questions, each tagged `lexical` (the answer shares the question's
words), `semantic` (the question paraphrases — "automobile" vs "car"), or
`hard` (a distractor document on the same topic).
CI runs it as a quality floor for both embedders (`baseline.json`), so a
change that makes retrieval worse fails the build.

The first measurement (k=10, β=2, 100 questions):

```text
                       recall   MRR      semantic-tag recall   semantic-tag MRR
builtin-lexical-v1     0.880    0.784    0.701                 0.593
static:potion-base-8M  0.961    0.891    0.901                 0.802
```

Static embeddings find the paraphrased answers the keyword matcher misses,
with no loss on keyword queries (both score 1.000 on `lexical` and
`hard`).

### Using the harness

Score the golden set with both embedders:

```console
$ make eval
embedder=static:potion-base-8M@f65d0f325faa k=10 beta=2.00

overall    n=100  precision=0.071 recall=0.961 fbeta=0.269 iou=0.071 recall@k=0.990 mrr=0.891
hard       n=20   ...
lexical    n=40   ...
semantic   n=40   ...
```

`precision`, `recall`, `fbeta`, and `iou` are character overlap between
the retrieved chunks and the highlight; `recall@k` and `mrr` are whether,
and how high, the right file appeared in the top `k`.
Precision is low for every embedder because a 1200-rune chunk is much
larger than a one-sentence highlight — read `recall` and `mrr`, and the per
tag rows, to compare embedders.

Score your own files instead of the fixture.
This needs a local Ollama model to write the questions; the scoring itself
never uses one:

```console
$ ollama pull llama3.2:3b
$ go/build/snapvault -C ~/Documents/notes index --embedder static
$ go/build/snapvault -C ~/Documents/notes eval generate \
    --model llama3.2:3b --out ~/notes-questions.jsonl --per-file 2
$ go/build/snapvault -C ~/Documents/notes eval run \
    --questions ~/notes-questions.jsonl --embedder static
```

Generated questions carry no tag; add `"tags": ["semantic"]` and friends by
hand if you want the per-tag rows.
Every `highlight` must occur exactly once in its file's extracted text;
`eval run` refuses a question set that breaks that rule rather than
scoring it wrong.

Let an agent tune the pipeline.
`go/internal/search/pipeline.go` holds every tunable (chunk size and
overlap, BM25 `k1`/`b`, fusion method and weights); the rules for changing
it are in `docs/autoretrieval/program.md`:

```text
        ┌──────────────────────────────────────────────────────┐
        │  edit pipeline.go  ──►  make test-go  ──►  make eval │
        │        ▲                                       │     │
        │        │     keep if FBeta@10 rose by ≥ 0.005 and no │
        │        └──── tag's Recall@10 fell by > 0.02  ◄─────┘ │
        │              (otherwise revert pipeline.go)          │
        └──────────────────────────────────────────────────────┘
           every run is logged to experiments.md; nothing is committed
```

Point a coding agent at that file ("read `docs/autoretrieval/program.md`
and run one experiment") and review `experiments.md` and the working tree
afterwards.

## Commands

The Java and Go CLIs take the same commands and print the same output for
everything both of them implement; `upgrade`, `repack`, `index`, `find`,
`model`, and `eval` are Go-only, per the format v2 design.

```text
snapvault init [directory]
snapvault [-C directory] snapshot [-m message]
snapvault [-C directory] log [revision] [--oneline] [--limit n]
snapvault [-C directory] diff [from [to]]
snapvault [-C directory] restore <revision> [--to directory] [--force]
snapvault [-C directory] upgrade                   # v1 -> v2, idempotent
snapvault [-C directory] repack [--dry-run]        # shrink object storage
snapvault [-C directory] index [--embedder ...]    # build the search index
snapvault [-C directory] find <query> [--limit n]  # search indexed blobs
snapvault model pull <name>                        # the only network call
snapvault model list
snapvault [-C directory] eval run --questions <file> [--corpus <dir>]
    [--embedder ...] [-k n] [--beta f] [--pct n] [--json]
snapvault [-C directory] eval generate --model <ollama-model> --out <file>
    [--per-file n]
```

Revisions are `HEAD`, `HEAD~2`, a full id, or a 7+ character prefix.
Restore verifies every object it needs before deleting anything, records
what it's doing so a crash mid-restore is recoverable, and refuses targets
that would eat your home directory or the repository itself.

## Quick start

```console
$ make            # build and test all three
$ make interop    # watch them read each other's repos

$ ./java/snapvault init ~/Documents/notes
$ go/build/snapvault -C ~/Documents/notes snapshot -m "before cleanup"
$ ./java/snapvault -C ~/Documents/notes log --oneline
$ cpp/build/snapvault-fsck ~/Documents/notes
```

You need JDK 21+, Go 1.24+, CMake, a C++20 compiler, zlib, zstd, and `make`.

## The Go rewrite: concurrent hashing

Snapshotting is mostly hashing files, and hashing parallelizes well.
The Go version walks the tree sequentially, then feeds every file to a
worker pool (one worker per CPU, `--workers n` to override):

```text
  walk (sequential)         hash pool (concurrent)      assemble (sequential)
┌────────────────┐         ┌──────────┐
│ list, sort,    │  files  │ worker 1 │──┐
│ filter entries │────────►│ worker 2 │  ├──► blob ids ──► trees ──► commit
└────────────────┘         │   ...    │──┘
                           └──────────┘
```

Each worker re-checks the file's size and mtime after hashing, so a file
rewritten mid-snapshot aborts the run instead of corrupting it.
Trees are built bottom-up after all hashing finishes, which is why the
resulting ids are identical no matter how many workers ran.

On an M2 (240 files × 128 KiB, hash-only scan):

```text
workers=1    23.4 ms    1.3 GB/s
workers=8     8.5 ms    3.7 GB/s
```

## The C++ verifier

`snapvault-fsck <directory>` is read-only.
It walks every ref, inflates every reachable object, recomputes every
SHA-256, and validates tree and commit payloads against the spec.
Corruption, truncation, or a missing object → exit 1.
Unreachable objects or an interrupted restore → warnings, exit 0.

It's stricter than the CLIs on purpose: it also rejects trees whose entries
aren't sorted, and sorted means *UTF-16 code-unit order* (Java string
order), which a byte-wise port silently gets wrong for characters outside
the Basic Multilingual Plane.
That one detail is pinned by golden test vectors generated from the Java
implementation and shared by all three test suites.

## How I know it works

```text
java writes a repo ──► go diffs it: "No changes."  go restores it: identical
go writes a repo   ──► java diffs it: "No changes."  java restores it: identical
same content       ──► both produce the exact same tree and blob ids
both repos         ──► snapvault-fsck: 0 errors   corrupted copy: rejected
v1 repo            ──► upgrade + repack ──► still reads clean everywhere
corrupted delta    ──► fsck and restore both reject it, in both languages
```

- `make test-java` — a 41-test integration suite, format v2 included
- `make test-go` — gofmt + vet + unit/integration tests, race-detector clean
- `make test-cpp` — NIST SHA-256 vectors, golden payload parsing, and
  integration tests against real repositories
- `make interop` — everything in the diagram above, 34 checks

### Shared golden vectors

Agreeing on well-formed input is the easy half.
`tests/golden/v2/delta/` holds checked-in `base`/`delta`/`target` triples
that all three suites read from disk and apply, so every implementation is
pinned to the same bytes rather than to its own idea of the format.
Alongside them, `reject/` holds malformed deltas — a copy that reaches past
the end of the base, a truncated instruction, a truncated varint header,
the reserved opcode `0x00`, and headers whose declared sizes disagree with
reality — that every implementation must *refuse*.
Those cases ship deliberately without a `.target` file, because there is no
correct output for them.

### Where the format was actually stress-tested

I checked the delta encoding against Git's own `patch-delta.c` rather than
trusting my reading of it, and hand-decoded a golden vector byte by byte to
confirm the copy instruction consumes its offset and size bytes in the
right order — the subtle part, since Git's delta size header uses a
different varint scheme than the offsets elsewhere in a packfile.

I also built a differential fuzzer: it generates malformed objects,
feeds each one to all three implementations, and compares their verdicts.
Across 919 generated cases there were no accept/reject disagreements, no
crashes, and no hangs — 479 malformed cases were rejected identically by
all three.
That campaign, plus an adversarial review pass, found five genuine bugs
worth naming, since they are the kind that a green test suite happily hides:

- `repack` renamed a rewritten object over the original without an fsync.
  It is the only code path that overwrites a *good* copy of an object, so a
  crash at the wrong moment could leave a file that was neither the old
  bytes nor the new ones.
- `repack` could build a delta cycle, destroying both objects involved.
- The v2 writer accepted blobs larger than the 256 MiB read cap, storing
  objects that no reader could ever decode.
- Java accepted v2 container objects inside a v1 repository, where Go and
  the C++ verifier both correctly refused them.
- Java's delta size header decoded into a signed value, so a crafted length
  went negative and skipped the size cap entirely.

All five are fixed, each with a regression test.

## License

[MIT](LICENSE).
