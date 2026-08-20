# SnapVault repository format

This document defines the data SnapVault reads and writes, across repository
format 1 (SnapVault 1.x) and format 2. All integer fields in the tree,
commit, and legacy envelope encodings are signed, big-endian Java primitive
values. Length and count fields must be non-negative and satisfy the decoder
limits. Format 2's container header and delta instruction stream, introduced
below, use their own single-byte and variable-length integer encodings,
described where each is introduced.

## Repository layout

```text
.snapvault/
├── format                 # `snapvault 1` or `snapvault 2`
├── HEAD                   # `ref: refs/heads/main`
├── lock                   # process lock; contents are not significant
├── cache                  # optional working-tree cache; see below
├── restore-in-progress    # present only while a restore is applying; see below
├── objects/
│   └── ab/cdef...         # 64-character object id split 2 + 62
├── refs/heads/main        # current commit id, absent before first snapshot
└── index/                 # optional, non-normative search sidecar; see below
```

Ref updates use a temporary file followed by an atomic replace when the
filesystem supports it. Snapshot scanning and ref mutation hold an exclusive
file lock.

## Format versions

The `format` file contains `snapvault 1` or `snapvault 2`, followed by a
trailing newline. Any other content is an unsupported-repository-format
error.

Format 1 is the original, closed format: every object file is the legacy
form defined below, and no other on-disk form is legal.

Format 2 adds exactly one more legal object encoding, the SVO2 container
defined below, and changes nothing else in this document — repository
layout, object ids, and the tree and commit payloads are identical in both
formats. A format 2 repository's object files are a mix of legacy and
container forms; a format 1 repository's are all legacy. A container-form
object is never legal in a format 1 repository; see Compatibility below.

Moving a repository from format 1 to format 2 rewrites only the `format`
file. It never rewrites an object, since a legacy object is legal in format
2 forever. There is no format 2 to format 1 transition.

## Working-tree cache

`.snapvault/cache` is optional. It is not part of object identity: a missing or corrupt file is treated as empty, and readers that ignore it still produce the same ids. Implementations that write it use this layout so Java and Go skip the same unchanged files.

```text
int32  magic = 0x53564443 (`SVDC`)
int32  version = 1
int64  written_at nanoseconds since Unix epoch
int32  entry_count
repeat entry_count times, sorted by Java String path order:
    int32  UTF-8 path byte count
    byte[] UTF-8 relative path with `/` separators
    int64  size in bytes
    int64  mtime nanoseconds since Unix epoch
    int64  device id, or 0 if unknown
    int64  inode, or 0 if unknown
    byte[32] blob SHA-256
```

A cache hit requires the path, size, and mtime to match; the device and inode to match when both sides recorded them; the blob to still exist in the object store; and the file's mtime to be strictly older than `written_at` (the racy-clean rule). Snapshot writes a new cache after hashing. In-place restore deletes it, because `.snapvault` is otherwise preserved.

## Object envelope and id

Every object has one of three ASCII type tokens: `blob`, `tree`, or
`commit`. Its canonical, uncompressed bytes are:

```text
<type> SP <decimal payload byte count> NUL <payload>
```

The object id is the lowercase hexadecimal SHA-256 digest of those canonical
bytes.

In a format 1 repository, the canonical bytes are always zlib-compressed in
the object file: the legacy form. In a format 2 repository, an object file
holds the canonical bytes in one of two forms, legacy or SVO2 container; see
Format v2 object storage below. A read reconstructs the canonical bytes from
whichever form is present, enforces the declared payload length, rejects
trailing bytes, recomputes the digest, and compares it with the requested id
before trusting anything — deltas included.

Because the type and size are part of the digest, the address is unambiguous
and resistant to type confusion. Because the destination path is derived
from the digest and an existing object is reused, equal file bytes occupy
one blob across all paths and snapshots.

## Blob payload

A regular-file blob payload is the file's exact bytes. A symbolic-link blob
payload is its link target encoded as UTF-8. File blobs are streamed when
captured and restored.

## Tree payload

Trees are sorted lexicographically by Java `String` entry name before
encoding. Duplicate names are invalid.

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

Directories reference tree objects. Files and symbolic links reference blob
objects. Only a regular file may set the executable flag. Entry names cannot
be empty, `.`, `..`, contain NUL, or contain either platform path separator.

A tree with zero entries is valid and meaningful: it encodes an empty
directory, which restore recreates. Because a directory's id is derived from
its children, two trees are identical exactly when their files, symbolic
links, and empty directories are identical, which is what lets a
working-tree comparison be computed by hashing alone, without storing
anything.

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

The initial commit has no parents. Normal CLI snapshots have one parent: the
prior value of `HEAD`. The format permits multiple parents so graph
traversal and a future merge operation do not require a format change.

## Format v2 object storage

In a format 2 repository, an object file (path `objects/aa/<62 hex>`, the
same sharded layout as format 1) is exactly one of two forms.

**Legacy form.** A zlib stream of the canonical bytes, byte-identical to
what a format 1 repository stores. Detection: `(first_byte & 0x0F) == 0x08`
— a zlib CMF byte always has `0x08` in its low nibble.

**Container form.** An "SVO2" container:

```text
[0..3]   magic "SVO2" = 0x53 0x56 0x4F 0x32
[4]      kind:  0x01 full   | 0x02 delta
[5]      codec: 0x01 zlib   | 0x02 zstd
kind=full:  [6..]    codec stream of the canonical bytes
kind=delta: [6..37]  base object id, 32 raw (non-hex) bytes
            [38..]   codec stream of delta instructions
```

For `kind=delta`, the base object id names another object in the same
store, read the same way (legacy or container, recursively). Applying the
delta against the base's canonical bytes reconstructs this object's
canonical bytes; see Delta instruction format below.

Sniffing the two forms apart is unambiguous: the container magic's first
byte, `0x53`, has `0x3` in its low nibble and can never be mistaken for a
zlib CMF byte. A reader inspects only the first byte to choose a path, then
validates the rest of the header it commits to. Any of the following is a
corrupt-object error: a first byte matching neither pattern; a partial
magic; an unknown kind or codec byte; a short or empty file. A
container-form object found in a format 1 repository is also an error; see
Compatibility below.

### Caps

Readers must enforce all of the following:

- Reconstructed canonical bytes: at most 256 MiB (the same cap format 1
  always applied to an object read whole into memory).
- Uncompressed delta instruction stream: at most 256 MiB.
- Delta chain depth: at most 32. A full object is depth 0; a delta object's
  depth is one more than its base's. A chain whose resolution would need a
  33rd hop is rejected.

A verifier such as fsck additionally reports a distinct "delta cycle" error
when reconstruction revisits an id already on its own chain stack, rather
than only catching a runaway chain once the depth cap trips.

### zstd streams

A codec-zstd stream is exactly one standard zstd frame; no skippable
frames. The frame's content-size field, if present, is informational only —
a reader must not require it to be present, and must enforce the caps above
regardless of what it claims.

## Delta instruction format

This is Git's pack-delta wire format, reused byte-for-byte. Source is the
base object's canonical bytes; target is this object's canonical bytes.
After reconstruction, the reader verifies SHA-256(target) equals the object
id, exactly as for any other object.

**Varint.** A little-endian base-128 varint: 7 data bits per byte, with the
high bit marking whether another byte follows. 20 encodes as `14`; 300
encodes as `AC 02`.

**Stream layout.** `srcSize` varint, `tgtSize` varint, then instructions
until the stream ends:

- Opcode `0x00` is invalid; a decoder must reject it.
- Opcode `0x01..0x7F` (high bit clear) is an insert: the next `opcode`
  bytes are literal bytes appended to the output.
- Opcode `0x80..0xFF` (high bit set) is a copy from source. Bits 0–3 of the
  opcode say which of 4 offset bytes follow; bits 4–6 say which of 3 size
  bytes follow. Present bytes are little-endian; omitted bytes are zero.
  The bytes appear in the order offset, then size, each in low-byte-first
  order. An assembled size of 0 means 65536. The instruction copies `size`
  bytes from source starting at `offset`.

The delta byte stream's own length is the only terminator for the
instruction loop — there is no explicit instruction count. Once the stream
is exhausted, the output's accumulated length must equal `tgtSize` exactly;
that comparison is the only check for a stream that decoded cleanly but
produced the wrong amount of output.

**Decoder requirements.** A decoder must:

- Check `srcSize == len(base)` before applying any instruction.
- Bounds-check every copy against the base (`offset + size` must not exceed
  the base length).
- Reject a truncated varint, a truncated literal run, or a truncated copy
  operand — any read that runs past the end of the delta bytes.
- Reject a stream whose final output length is not exactly `tgtSize`.

**Encoder conventions.** An encoder emits insert opcodes covering at most
127 literal bytes each and copy instructions covering at most `0xFFFFFF`
bytes each, splitting a larger run across multiple instructions; it should
not emit an explicit zero size. Different encoders may produce different
valid deltas for the same (base, target) pair; a conforming decoder
reproduces the same target from any of them.

**Worked example.**

```text
base canonical bytes:   "blob 12\0hello world\n"   (20 bytes)
    id: 0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d
target canonical bytes: "blob 13\0hello worlds\n"  (21 bytes)

one valid delta (17 bytes, hex):
    14 15 08 62 6c 6f 62 20 31 33 00 91 08 0b 02 73 0a

  = srcSize 20, tgtSize 21,
    insert 8 bytes ("blob 13\0"),
    copy(offset=8, size=11) -> "hello world"
        (opcode 0x91 = 1 offset byte + 1 size byte),
    insert 2 bytes ("s\n")
```

## Compatibility

- A legacy-form object is legal in a format 1 repository and stays legal
  forever in a format 2 repository derived from it.
- A container-form object is never legal in a format 1 repository: a
  verifier such as fsck reports it as an error, and any reader that
  encounters one there must surface a corrupt- or unsupported-object error
  rather than decoding it.
- A format 1 repository's on-disk bytes — object files, refs, `HEAD`,
  `format` — are identical to what SnapVault 1.x wrote for the same
  content. Format 2 changes nothing about how a format 1 repository is read
  or written.
- Which on-disk forms an implementation writes is its own choice, so long
  as every form it produces is one this document defines and every object
  it writes verifies under the rules above. As implemented in this
  repository: the Java command-line interface writes only the legacy form,
  in either repository format, and reads both forms. The Go command-line
  interface writes the legacy form in a format 1 repository and, in a
  format 2 repository, writes new objects as container/full/zstd; its
  `repack` operation additionally rewrites eligible blobs as
  container/delta/zstd when that is smaller, but never trees or commits.
  The Go CLI reads both forms. The C++ integrity verifier (`snapvault-fsck`)
  only reads; it never writes an object.

## Search index sidecar (non-normative)

`.snapvault/index/` holds an optional, rebuildable search index that the Go
command-line interface maintains alongside a repository (its `index` and
`find` operations). It is not part of the object store: no object identity,
digest, or reachability rule depends on its presence, contents, or absence,
and a verifier such as fsck ignores it entirely. Deleting it loses nothing
but the index; rebuilding it never rewrites an object.

## Restore invariants

Before a restore mutates its target, SnapVault traverses the entire tree
graph, validates tree structure, detects cycles, inflates every unique
blob, verifies every object id, and enforces object types. It also probes
the target filesystem and refuses a snapshot whose sibling names that
filesystem cannot keep apart. Only after preflight succeeds does it clear
and materialize the target. The `.snapvault` directory is preserved during
an in-place restore, and restore never moves `HEAD`.

Clearing and materializing are many filesystem operations and cannot be
made atomic as a unit. Restore therefore writes `restore-in-progress` and
forces it to disk before removing anything, and deletes it only after the
last entry is materialized. Its first line is the commit id being applied
and its second is the absolute target path. While that file exists and
names the repository root, `snapshot` and `diff` refuse to run, because the
working tree is then a partial materialization rather than the user's
content. Re-running the recorded restore is the recovery: it clears the
target again and rewrites it in full.
