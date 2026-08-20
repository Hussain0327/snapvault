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

`snapvault index` builds a local search index over every blob reachable
from every ref, at `.snapvault/index/embeddings.svi`.
`snapvault find <query>` searches it.
Both are Go-only and both treat the index as a sidecar: it never touches
`objects/`, `fsck` ignores it, and deleting it just means the next `find`
tells you to reindex.

Be clear about what kind of "search" this is by default.
`snapvault index` with no `--embedder` flag uses `builtin-lexical-v1`: a
deterministic hashed bag-of-words keyword matcher.
It is not semantic search — it will not know that "car" and "automobile"
are related.
For real semantic embeddings, run a local [Ollama](https://ollama.com) and
pass `--embedder ollama:<model>`; SnapVault then POSTs to
`http://localhost:11434/api/embeddings` for each chunk and each query.
Either way nothing leaves the machine: the builtin embedder is pure
computation, and the Ollama path only ever talks to localhost.
Text extraction covers UTF-8 text and simple PDFs (via `pdftotext` when
it's on `PATH`, otherwise a small builtin extractor); anything else is
skipped and counted as skipped.

## Commands

The Java and Go CLIs take the same commands and print the same output for
everything both of them implement; `upgrade`, `repack`, `index`, and
`find` are Go-only, per the format v2 design.

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
