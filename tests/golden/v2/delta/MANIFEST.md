# v2 delta golden vectors

These `base` / `delta` / `target` triples are the shared cross-language test
fixtures for format v2 delta compression (see the v2 design document, section
"Delta instruction format").
Each triple must satisfy `Apply(base, delta) == target` in every
implementation that decodes SnapVault deltas: the Go, Java, and C++ suites
each read these exact bytes from disk and assert that.

`base`, `delta`, and `target` are raw binary files, not text: do not
line-ending-normalize or otherwise touch them when checking this directory
out.

## Cases

- `01-worked-example` — the byte-for-byte example from the design doc: a
  17-byte delta turning `"blob 12\0hello world\n"` into
  `"blob 13\0hello worlds\n"` via one insert, one copy, one insert. Every
  language's suite treats this case as a required unit test, not just a
  fixture loop.
- `02-multi-byte-varint` — base and target both exceed 127 bytes, so their
  `srcSize`/`tgtSize` header varints each need two bytes, exercising the
  continuation-bit varint decoder beyond the single-byte case.
- `03-copy-65536` — a whole 65536-byte base copied by a single instruction
  using the wire format's zero-means-65536 shorthand: opcode `0x80` with no
  offset or size bytes present. `delta.Encode` never emits this shorthand
  itself (it always writes explicit size bytes), so this case is
  hand-crafted to cover the shorthand's decode path specifically.
- `04-insert-chain` — a target with no 16-byte window in common with its
  base, forcing the encoder to chain three insert instructions (127 + 127 +
  46 literal bytes) past the single-instruction 127-byte cap.
- `05-binary-content` — base and target span the full byte range, including
  NUL and non-UTF-8 bytes, with one shared 4096-byte run so the delta mixes
  copy and insert instructions over binary content.
- `06-mixed-edits` — a multi-KB text edited in several places (an insertion,
  a replaced stretch, and a shared tail), the shape a real blob revision
  looks like.

## Regenerating

All six cases are produced deterministically (fixed random seeds, no
wall-clock or environment dependence) by the generator in
`go/internal/delta/gengolden`. From the `go/` module directory:

```sh
go run ./internal/delta/gengolden
```

The generator feeds every case through `delta.Apply` before writing it, so a
bug in `delta.Encode` fails the run instead of checking in a bad fixture.
Re-running it is expected to reproduce byte-identical files; if it doesn't,
something about the generator changed and this manifest's case descriptions
may need updating too.

`go/internal/delta/golden_test.go` reads every case listed above out of this
directory and asserts `Apply(base, delta) == target`, so `go test
./internal/delta/` fails if a checked-in fixture is ever corrupted or goes
stale against the current decoder.

## Reject cases (`reject/`)

The six cases above prove the three decoders *agree on well-formed input*;
they say nothing about whether the decoders agree on how to refuse
*malformed* input, which is exactly the kind of gap a differential fuzzing
campaign is built to find. The `reject/` subdirectory holds one malformed
`base`/`delta` pair per distinct failure class docs/FORMAT.md's "Decoder
requirements" names. Every implementation's decoder MUST refuse every case
here.

A reject case is a `base` and a `delta` file only — never a `target`, since
there is no valid reconstruction to check against. The subdirectory is the
one and only marker that distinguishes these from the accept corpus above:
a reject case is never given a bare `NN-name` stem directly under this
directory, and an accept case is never placed under `reject/`.

- `01-copy-past-end` — a copy instruction whose `offset + size` reaches past
  the end of a 10-byte base (`offset=5, size=10`).
- `02-truncated-instruction` — an insert opcode declares 5 literal bytes but
  the stream ends after only 2 of them.
- `03-truncated-varint-header` — `srcSize` decodes cleanly (and matches the
  base), but the `tgtSize` varint that follows sets its continuation bit and
  the stream ends before a following byte arrives.
- `04-reserved-opcode-zero` — the instruction stream is the single reserved
  byte `0x00`.
- `05-src-size-mismatch` — `srcSize` declares 11 against a 10-byte base.
- `06-tgt-size-mismatch` — `tgtSize` declares 10, but the one insert
  instruction in the stream only produces 3 bytes before the stream ends.

### Regenerating the reject cases

Unlike the accept corpus, these are not produced by `gengolden`: each is a
handful of hand-assembled bytes, built directly from the varint and opcode
encoding in docs/FORMAT.md's "Delta instruction format" rather than through
`delta.Encode` (which never emits invalid streams). Reproduce them with:

```python
def varint(v):
    out = b""
    while v >= 0x80:
        out += bytes([v & 0x7f | 0x80])
        v >>= 7
    return out + bytes([v])

# 01-copy-past-end
base = b"0123456789"
delta = varint(10) + varint(10) + bytes([0x91, 0x05, 0x0A])

# 02-truncated-instruction
base = b"hello"
delta = varint(5) + varint(5) + bytes([0x05]) + b"ab"

# 03-truncated-varint-header
base = b"test"
delta = bytes([0x04, 0x80])

# 04-reserved-opcode-zero
base = b"abc"
delta = varint(3) + varint(0) + bytes([0x00])

# 05-src-size-mismatch
base = b"0123456789"
delta = varint(11) + varint(0)

# 06-tgt-size-mismatch
base = b"hello"
delta = varint(5) + varint(10) + bytes([0x03]) + b"abc"
```

Each was verified by hand against all three decoders' source
(`go/internal/delta/delta.go`, `java/.../store/DeltaApplier.java`,
`cpp/src/delta.cc`) before being checked in: every one is rejected, and for
the reason its case name claims, not by accident.

`go/internal/delta/golden_test.go`, `AllTests.java`'s
`deltaGoldenVectorsRejectMalformed`, and `cpp/tests/unit_tests.cc`'s
`TestGoldenDeltaVectorsRejectMalformed` each read every case listed above
out of `reject/` and assert that applying it fails *and* that the error
names the expected defect (a language-appropriate substring match, since
the three decoders don't share exact wording) — so a decoder that silently
starts accepting a malformed stream, or starts rejecting it for the wrong
reason, fails that language's suite.
