# SnapVault repository format v1

This document defines the data written by SnapVault 1.x. All integer fields in binary payloads are signed, big-endian Java primitive values. Length and count fields must be non-negative and satisfy the decoder limits.

## Repository layout

```text
.snapvault/
├── format                 # `snapvault 1`
├── HEAD                   # `ref: refs/heads/main`
├── lock                   # process lock; contents are not significant
├── restore-in-progress    # present only while a restore is applying; see below
├── objects/
│   └── ab/cdef...         # 64-character object id split 2 + 62
└── refs/heads/main        # current commit id, absent before first snapshot
```

Ref updates use a temporary file followed by an atomic replace when the filesystem supports it. Snapshot scanning and ref mutation hold an exclusive file lock.

## Object envelope and id

Every object has one of three ASCII type tokens: `blob`, `tree`, or `commit`. Its canonical, uncompressed bytes are:

```text
<type> SP <decimal payload byte count> NUL <payload>
```

The object id is the lowercase hexadecimal SHA-256 digest of those canonical bytes. The complete canonical bytes are zlib-compressed in the object file. Reads inflate the object, enforce the declared payload length, reject trailing bytes, recompute the digest, and compare it with the requested id.

Because the type and size are part of the digest, the address is unambiguous and resistant to type confusion. Because the destination path is derived from the digest and an existing object is reused, equal file bytes occupy one blob across all paths and snapshots.

## Blob payload

A regular-file blob payload is the file's exact bytes. A symbolic-link blob payload is its link target encoded as UTF-8. File blobs are streamed when captured and restored.

## Tree payload

Trees are sorted lexicographically by Java `String` entry name before encoding. Duplicate names are invalid.

```text
int32  magic = 0x53565431 (`SVT1`)
int32  entry_count
repeat entry_count times:
    int32  UTF-8 name byte count
    byte[] UTF-8 name
    uint8  kind: 1=file, 2=directory, 3=symlink
    uint8  executable boolean: 0 or 1
    byte[32] referenced object SHA-256
```

Directories reference tree objects. Files and symbolic links reference blob objects. Only a regular file may set the executable flag. Entry names cannot be empty, `.`, `..`, contain NUL, or contain either platform path separator.

A tree with zero entries is valid and meaningful: it encodes an empty directory, which restore recreates. Because a directory's id is derived from its children, two trees are identical exactly when their files, symbolic links, and empty directories are identical, which is what lets a working-tree comparison be computed by hashing alone, without storing anything.

## Commit payload

```text
int32  magic = 0x53564331 (`SVC1`)
byte[32] root tree SHA-256
int32  parent_count
repeat parent_count times:
    byte[32] parent commit SHA-256
int64  timestamp epoch second
int32  timestamp nanosecond, 0 through 999999999
int32  UTF-8 message byte count
byte[] UTF-8 message
```

The initial commit has no parents. Normal CLI snapshots have one parent: the prior value of `HEAD`. The format permits multiple parents so graph traversal and a future merge operation do not require a format change.

## Restore invariants

Before a restore mutates its target, SnapVault traverses the entire tree graph, validates tree structure, detects cycles, inflates every unique blob, verifies every object id, and enforces object types. It also probes the target filesystem and refuses a snapshot whose sibling names that filesystem cannot keep apart. Only after preflight succeeds does it clear and materialize the target. The `.snapvault` directory is preserved during an in-place restore, and restore never moves `HEAD`.

Clearing and materializing are many filesystem operations and cannot be made atomic as a unit. Restore therefore writes `restore-in-progress` and forces it to disk before removing anything, and deletes it only after the last entry is materialized. Its first line is the commit id being applied and its second is the absolute target path. While that file exists and names the repository root, `snapshot` and `diff` refuse to run, because the working tree is then a partial materialization rather than the user's content. Re-running the recorded restore is the recovery: it clears the target again and rewrites it in full.
