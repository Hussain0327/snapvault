# SnapVault

SnapVault brings Git-style snapshot, diff, history, and restore to any ordinary directory. It is a dependency-free Java 21 application: the directory does not need to be source code, and it does not need to be a Git repository.

Every regular file is stored as an immutable blob in a content-addressed object database. Equal content produces the same SHA-256 object id, so an unchanged file—or the same file at several paths—is written only once across all snapshots. Recursive tree objects preserve directory structure, symlinks, and executable bits. Commit objects point to a root tree and their parent commits, forming the history graph.

## Quick start

Requirements: JDK 21 or newer and `make`.

```console
$ make test
$ ./snapvault init ~/Documents/my-folder
$ ./snapvault -C ~/Documents/my-folder snapshot -m "Before reorganization"
$ ./snapvault -C ~/Documents/my-folder log --oneline
$ ./snapvault -C ~/Documents/my-folder diff
$ ./snapvault -C ~/Documents/my-folder diff HEAD~1 HEAD
$ ./snapvault -C ~/Documents/my-folder restore HEAD~1 --to /tmp/my-folder-old
```

The launcher builds `build/snapvault.jar` when needed. To build it directly:

```console
$ make jar
$ java -jar build/snapvault.jar help
```

## Commands

```text
snapvault init [directory]
snapvault [-C directory] snapshot [-m message]
snapvault [-C directory] log [revision] [--oneline] [--limit n]
snapvault [-C directory] diff [from [to]]
snapvault [-C directory] restore <revision> [--to directory] [--force]
```

- `init` creates `.snapvault` inside the target directory.
- `snapshot` recursively captures the current filesystem tree, creates a commit whose parent is the current `HEAD`, and atomically advances `refs/heads/main`.
- `log` traverses parent links in the commit graph. Revisions accept `HEAD`, `HEAD~N`, a full SHA-256 id, or an unambiguous prefix of at least seven characters.
- `diff` prints `A`, `M`, `D`, or `T` and each changed path. With no revisions it compares `HEAD` to the working directory; with one it compares that snapshot to the working directory; with two it compares stored snapshots.
- `restore` materializes an exact snapshot. By default it restores in place without moving `HEAD`. Use `--to` to export into another directory.

An in-place restore refuses to overwrite unsnapshotted work unless `--force` is present. An external target must be empty unless forced. SnapVault rejects filesystem-root, home-directory, repository-ancestor, metadata-directory, and symlink restore targets. It also verifies every referenced object before removing any live file.

## Storage model

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
                  └──────────┘  equal content reuses one object id
```

Objects use a typed canonical envelope:

```text
SHA-256("<type> <payload-size>\\0" + payload)
```

The envelope prevents a blob, tree, and commit with the same payload bytes from colliding. It is zlib-compressed and stored at `.snapvault/objects/aa/bb...`, split after the first two hex digits. Writes go through a temporary file and an atomic move; existing ids are never duplicated. Blob ingestion and restore stream in 64 KiB chunks, so file size is not bounded by Java heap size.

See [docs/FORMAT.md](docs/FORMAT.md) for the versioned binary format and repository invariants.

## Filesystem behavior

SnapVault captures regular files, directories, symbolic links, and whether a regular file is executable. It does not follow symlinks. `.snapvault` at the repository root is always excluded. Sockets, devices, and named pipes are rejected instead of being silently omitted.

Snapshots are filesystem reads rather than OS-level atomic volume snapshots. SnapVault detects a regular file whose size or modification time changes while it is being read and aborts that snapshot without advancing `HEAD`.

## Verification

`make test` compiles production and test code with `--release 21`, enables all compiler lint checks, treats warnings as errors, and runs integration tests against real temporary directories. The suite covers:

- deterministic SHA-256 addressing, zlib persistence, integrity checking, and deduplication;
- recursive trees and parent-linked history, including `HEAD~N` traversal;
- working-directory and snapshot-to-snapshot diffs;
- dirty-work protection, exact in-place restore, and external export;
- full object preflight before destructive restore;
- symbolic-link round trips; and
- the complete CLI flow from `init` through `restore`.

## Project layout

```text
src/main/java/io/snapvault/
├── cli/      command parsing and terminal output
├── core/     repository, history, diff, locking, and restore
├── hash/     SHA-256 object-id utilities
├── model/    canonical commit and tree models
└── store/    compressed filesystem object database
```
