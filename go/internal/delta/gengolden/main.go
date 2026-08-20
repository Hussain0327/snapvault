// Command gengolden regenerates the shared v2 delta golden vectors consumed
// by the Go, Java, and C++ test suites. Run it from the go/ module directory:
//
//	go run ./internal/delta/gengolden
//
// It is deterministic (fixed random seeds throughout) and self-verifying:
// every case is fed through delta.Apply before being written, so a bug in
// delta.Encode fails the run instead of checking in a bad fixture. See
// tests/golden/v2/delta/MANIFEST.md for what each case covers.
package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/Hussain0327/snapvault/go/internal/delta"
)

// outDir is the checked-in fixture directory, relative to the go/ module
// root that `go run ./internal/delta/gengolden` is expected to run from (a
// `go run` binary's working directory is the caller's, not the package's).
const outDir = "../tests/golden/v2/delta"

// goldenCase is one base/delta/target fixture. delta is left nil for cases
// produced by delta.Encode, and set explicitly for hand-crafted streams that
// exercise wire-format shorthands Encode itself never emits.
type goldenCase struct {
	name   string
	base   []byte
	target []byte
	delta  []byte
}

func main() {
	cases := []goldenCase{
		workedExample(),
		multiByteVarint(),
		copy65536(),
		insertChain(),
		binaryContent(),
		mixedEdits(),
	}

	for _, c := range cases {
		d := c.delta
		if d == nil {
			d = delta.Encode(c.base, c.target)
		}
		got, err := delta.Apply(c.base, d)
		if err != nil {
			fatalf("case %s: delta.Apply failed: %v", c.name, err)
		}
		if !bytes.Equal(got, c.target) {
			fatalf("case %s: delta.Apply(base, delta) did not reproduce target", c.name)
		}
		write(c.name, "base", c.base)
		write(c.name, "delta", d)
		write(c.name, "target", c.target)
		fmt.Printf("%s: base=%d delta=%d target=%d\n", c.name, len(c.base), len(d), len(c.target))
	}
}

func write(name, ext string, data []byte) {
	path := filepath.Join(outDir, fmt.Sprintf("%s.%s", name, ext))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatalf("write %s: %v", path, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// appendVarint duplicates the unexported encoder in internal/delta so this
// generator can hand-craft instruction streams that delta.Encode itself
// would never produce (the zero-means-65536 shorthand below).
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// workedExample is 01-worked-example: the exact byte sequence from the v2
// design doc's "Delta instruction format" section, used as a unit test in
// every language.
func workedExample() goldenCase {
	return goldenCase{
		name:   "01-worked-example",
		base:   []byte("blob 12\x00hello world\n"),
		target: []byte("blob 13\x00hello worlds\n"),
		delta: []byte{
			0x14, 0x15, // srcSize=20, tgtSize=21
			0x08, 'b', 'l', 'o', 'b', ' ', '1', '3', 0x00, // insert 8: "blob 13\0"
			0x91, 0x08, 0x0b, // copy(offset=8, size=11)
			0x02, 's', '\n', // insert 2: "s\n"
		},
	}
}

// multiByteVarint is 02-multi-byte-varint: base and target both exceed 127
// bytes, so their srcSize/tgtSize header varints each need two bytes.
func multiByteVarint() goldenCase {
	rng := rand.New(rand.NewSource(1))
	base := randomText(rng, 300)
	target := append([]byte(nil), base...)
	target[50] = 'X'
	target[150] = 'Y'
	target = append(target, []byte(" appended tail text")...)
	return goldenCase{name: "02-multi-byte-varint", base: base, target: target}
}

// copy65536 is 03-copy-65536: a whole 65536-byte base copied in a single
// instruction using the wire format's zero-means-65536 shorthand (opcode
// 0x80, no offset or size bytes present).
func copy65536() goldenCase {
	base := make([]byte, 65536)
	seed := uint32(1)
	for i := range base {
		seed = seed*1664525 + 1013904223 // deterministic LCG, reproducible
		base[i] = byte(seed >> 24)
	}
	target := append([]byte(nil), base...)

	d := appendVarint(nil, uint64(len(base)))
	d = appendVarint(d, uint64(len(base)))
	d = append(d, 0x80) // copy(offset=0, assembled size=0 -> 65536)

	return goldenCase{name: "03-copy-65536", base: base, target: target, delta: d}
}

// insertChain is 04-insert-chain: a target with no 16-byte window in common
// with its base, forcing delta.Encode to chain more than two
// maxInsertBytes-sized insert instructions.
func insertChain() goldenCase {
	rng := rand.New(rand.NewSource(2))
	return goldenCase{
		name:   "04-insert-chain",
		base:   []byte("unrelated"), // 9 bytes: shorter than blockSize, indexes nothing
		target: randomText(rng, 300),
	}
}

// binaryContent is 05-binary-content: base and target span the full byte
// range, including NUL and non-UTF-8 bytes, with a shared run so the delta
// carries at least one copy instruction alongside inserts.
func binaryContent() goldenCase {
	rng := rand.New(rand.NewSource(3))
	shared := make([]byte, 4096)
	for i := range shared {
		shared[i] = byte(i % 256)
	}
	base := append(randomBytes(rng, 200), shared...)
	base = append(base, randomBytes(rng, 200)...)
	target := append(randomBytes(rng, 150), shared...)
	target = append(target, randomBytes(rng, 300)...)
	return goldenCase{name: "05-binary-content", base: base, target: target}
}

// mixedEdits is 06-mixed-edits: a multi-KB text edited in several places,
// exercising a normal chain of copy and insert instructions together.
func mixedEdits() goldenCase {
	rng := rand.New(rand.NewSource(4))
	base := randomText(rng, 5000)
	target := append([]byte(nil), base[:1000]...)
	target = append(target, []byte(" [inserted section] ")...)
	target = append(target, base[1000:2500]...)
	target = append(target, randomText(rng, 100)...) // a replaced-in-place stretch
	target = append(target, base[2600:]...)
	return goldenCase{name: "06-mixed-edits", base: base, target: target}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func randomText(rng *rand.Rand, n int) []byte {
	const words = "the quick brown fox jumps over lazy dog while snapvault stores every blob tree and commit as content addressed objects "
	b := make([]byte, 0, n)
	for len(b) < n {
		b = append(b, words[rng.Intn(len(words))])
	}
	return b[:n]
}
