# SnapVault

Git-style snapshots for any folder.
Point it at a directory, take snapshots, diff them, restore any of them.
The directory doesn't need to be code and nothing leaves your machine.

The interesting part: it's **one binary format with three implementations**
that all read and write each other's repositories, byte for byte.

```text
snapvault/
├── docs/FORMAT.md    the format spec — the contract everything follows
├── java/             the original implementation (Java 21, zero deps)
├── go/               full rewrite with concurrent hashing (Go, stdlib only)
├── cpp/              snapvault-fsck, an integrity checker (C++20 + zlib)
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

## Commands

The Java and Go CLIs take the same commands and print the same output:

```text
snapvault init [directory]
snapvault [-C directory] snapshot [-m message]
snapvault [-C directory] log [revision] [--oneline] [--limit n]
snapvault [-C directory] diff [from [to]]
snapvault [-C directory] restore <revision> [--to directory] [--force]
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

You need JDK 21+, Go 1.22+, CMake, a C++20 compiler, zlib, and `make`.

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
```

- `make test-java` — the original 16-test integration suite
- `make test-go` — gofmt + vet + unit/integration tests, race-detector clean
- `make test-cpp` — NIST SHA-256 vectors, golden payload parsing, and
  integration tests against real repositories
- `make interop` — everything in the diagram above, 18 checks

## License

[MIT](LICENSE).
