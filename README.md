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
- `log` traverses parent links in the commit graph. Revisions accept `HEAD`, a full SHA-256 id, or an unambiguous prefix of at least seven characters, each optionally followed by ancestor steps: `~` means one generation, `~N` means N, and repeated steps accumulate, so `HEAD~1~1` and `HEAD~2` name the same snapshot.
- `diff` prints `A`, `M`, `D`, or `T` and each changed path; a trailing `/` marks a directory. With no revisions it compares `HEAD` to the working directory; with one it compares that snapshot to the working directory; with two it compares stored snapshots. `diff` never writes to the object database, so an empty change list means the two trees are byte-for-byte identical.
- `restore` materializes an exact snapshot. By default it restores in place without moving `HEAD`. Use `--to` to export into another directory.

An in-place restore refuses to overwrite unsnapshotted work unless `--force` is present. An external target must be empty unless forced. SnapVault rejects filesystem-root, home-directory, repository-ancestor, repository-descendant, metadata-directory, and symlink restore targets.

Before removing a single live file, restore verifies every referenced object, inflating each blob and recomputing each id, and refuses any snapshot whose names the target filesystem cannot represent. What that preflight cannot do is make the clear-then-write itself atomic. So restore first records the commit it is applying in `.snapvault/restore-in-progress` and forces that record to disk. If the process dies partway, the record survives: `snapshot` and `diff` then refuse to run rather than treat a half-written directory as your content, and both name the command that finishes the job. Re-running that restore completes it and clears the record.

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

An empty directory is part of the snapshotted state: it is stored, reported by `diff` as `A dir/` or `D dir/`, and recreated by `restore`. Once a directory has contents, only those contents are reported, because they already describe the difference.

A nested SnapVault repository's `.snapvault` directory is skipped at any depth, so snapshotting a directory that contains other repositories captures their files but not their object databases.

Snapshots are filesystem reads rather than OS-level atomic volume snapshots. SnapVault detects a regular file whose size or modification time changes while it is being read and aborts that snapshot without advancing `HEAD`. A file rewritten in place to the same length within one filesystem timestamp tick can still slip past that check; this is the inherent limit of any mtime-based scan.

## Portability

Developed and tested on macOS and Linux; CI runs Linux. Symbolic links and POSIX executable bits are captured on any filesystem that supports them, and the suite skips those assertions where they are unavailable.

A snapshot taken on a case-sensitive filesystem can hold two names that differ only by case, or by Unicode composition. Such a snapshot cannot be represented on macOS or Windows, where both names are one file. Restore probes the target filesystem and refuses the whole operation up front rather than silently writing one entry over the other.

## Verification

`make test` compiles production and test code with `--release 21`, enables all compiler lint checks, treats warnings as errors, and runs integration tests against real temporary directories. The suite covers:

- deterministic SHA-256 addressing, zlib persistence, integrity checking, and deduplication;
- recursive trees and parent-linked history, including `HEAD~N` traversal;
- working-directory and snapshot-to-snapshot diffs;
- dirty-work protection, exact in-place restore, and external export;
- full object preflight before destructive restore;
- symbolic-link round trips;
- empty-directory add and remove, so a clean `diff` always agrees with the dirty-work check;
- that a working-directory `diff` writes no objects;
- chained ancestor revisions;
- refusal of names the target filesystem cannot keep apart, before anything is written;
- rejection of an object whose declared size is implausible, before it is buffered;
- that a nested repository's metadata is never captured as content;
- refusal of a restore target inside the repository;
- that an interrupted restore blocks `snapshot` and `diff` until it is finished; and
- the complete CLI flow from `init` through `restore`, including that a filesystem error names its cause.

## Project layout

```text
src/main/java/io/snapvault/
├── cli/      command parsing and terminal output
├── core/     repository, history, diff, locking, and restore
├── hash/     SHA-256 object-id utilities
├── model/    canonical commit and tree models
└── store/    compressed filesystem object database
```

## License

[MIT](LICENSE).
