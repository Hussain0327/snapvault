package delta

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"testing"
)

// Worked example from the v2 design doc, "Delta instruction format": the
// same bytes must decode to the exact target on every conforming decoder.
const (
	workedBase   = "blob 12\x00hello world\n"
	workedTarget = "blob 13\x00hello worlds\n"
)

func TestApplyWorkedExample(t *testing.T) {
	// Hex straight from the spec: 14 15 08 62 6c 6f 62 20 31 33 00 91 08 0b 02 73 0a
	deltaHex := "14150862 6c6f6220 31330091 080b0273 0a"
	deltaHex = removeSpaces(deltaHex)
	delta, err := hex.DecodeString(deltaHex)
	if err != nil {
		t.Fatalf("hex.DecodeString: %v", err)
	}
	got, err := Apply([]byte(workedBase), delta)
	if err != nil {
		t.Fatalf("Apply(worked example) = %v, want nil error", err)
	}
	if string(got) != workedTarget {
		t.Errorf("Apply(worked example) = %q, want %q", got, workedTarget)
	}
}

func removeSpaces(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			b = append(b, s[i])
		}
	}
	return string(b)
}

func TestApplyRejectsOpcodeZero(t *testing.T) {
	base := []byte("hello world\n")
	delta := append(appendVarint(nil, uint64(len(base))), appendVarint(nil, 1)...)
	delta = append(delta, 0x00)
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with opcode 0x00 = nil error, want error")
	}
}

func TestApplyRejectsSrcSizeMismatch(t *testing.T) {
	base := []byte("hello world\n")
	delta := appendVarint(nil, uint64(len(base))+1)
	delta = appendVarint(delta, 0)
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with wrong srcSize = nil error, want error")
	}
}

func TestApplyRejectsTgtSizeMismatchShort(t *testing.T) {
	base := []byte("hello world\n")
	// Declares a target of 5 bytes but the only instruction inserts 3.
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 5)
	delta = append(delta, 3, 'a', 'b', 'c')
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with undersized reconstruction = nil error, want error")
	}
}

func TestApplyRejectsTgtSizeMismatchLong(t *testing.T) {
	base := []byte("hello world\n")
	// Declares a target of 2 bytes but the instruction inserts 3.
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 2)
	delta = append(delta, 3, 'a', 'b', 'c')
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with oversized reconstruction = nil error, want error")
	}
}

func TestApplyRejectsOutOfBoundsCopy(t *testing.T) {
	base := []byte("hello world\n") // 12 bytes
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 5)
	// Copy opcode: offset byte present (0x01), size byte present (0x10) ->
	// flags 0x91, offset=0, size=5, but starting past the end of base.
	delta = append(delta, 0x91, 10, 5)
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with out-of-bounds copy = nil error, want error")
	}
}

func TestApplyRejectsCopyOverflowingBase(t *testing.T) {
	base := []byte("hello world\n") // 12 bytes
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 20)
	// offset=0, size=20 (> len(base)).
	delta = append(delta, 0x91, 0, 20)
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with copy longer than base = nil error, want error")
	}
}

func TestApplyRejectsTruncatedVarint(t *testing.T) {
	// A single continuation byte with no follow-up byte.
	delta := []byte{0x80}
	if _, err := Apply(nil, delta); err == nil {
		t.Error("Apply with truncated varint = nil error, want error")
	}
}

func TestApplyRejectsTruncatedLiteral(t *testing.T) {
	base := []byte("hi")
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 5)
	// Insert opcode claims 5 literal bytes but only 2 follow.
	delta = append(delta, 5, 'a', 'b')
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with truncated literal = nil error, want error")
	}
}

func TestApplyRejectsTruncatedCopyOperand(t *testing.T) {
	base := []byte("hello world\n")
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 5)
	// Claims an offset byte and a size byte but the stream ends after the
	// opcode.
	delta = append(delta, 0x91)
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with truncated copy operand = nil error, want error")
	}
}

func TestApplyRejectsTrailingGarbage(t *testing.T) {
	base := []byte("hi")
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 2)
	delta = append(delta, 2, 'h', 'i') // exactly reproduces the 2-byte target
	delta = append(delta, 0xFF)        // trailing garbage: a truncated copy opcode
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with trailing garbage after the target = nil error, want error")
	}
}

func TestApplyDecodesZeroSizeCopyAs65536(t *testing.T) {
	base := make([]byte, 65536)
	for i := range base {
		base[i] = byte(i)
	}
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 65536)
	// opcode 0x80: no offset bytes (offset=0), no size bytes (assembled
	// size=0, meaning 65536).
	delta = append(delta, 0x80)
	got, err := Apply(base, delta)
	if err != nil {
		t.Fatalf("Apply(zero-size copy) = %v, want nil error", err)
	}
	if !bytes.Equal(got, base) {
		t.Error("Apply(zero-size copy) did not reproduce the whole base")
	}
}

func TestApplyRejectsDeclaredTargetSizeAboveCap(t *testing.T) {
	delta := appendVarint(nil, 0)
	delta = appendVarint(delta, maxOutputBytes+1)
	if _, err := Apply(nil, delta); err == nil {
		t.Error("Apply with tgtSize above the cap = nil error, want error")
	}
}

func TestApplyRejectsReconstructedOutputAboveCap(t *testing.T) {
	// A base just under the per-copy size limit (0xFFFFFF) and a delta that
	// copies it whole, repeated enough times to blow the 256 MiB output cap
	// long before the (falsely small) declared target size is reached.
	chunk := 0xFFFFFF
	base := make([]byte, chunk)
	delta := appendVarint(nil, uint64(len(base)))
	delta = appendVarint(delta, 1) // lies about the target size
	const repeats = 17             // 17 * 0xFFFFFF > 256 MiB
	for i := 0; i < repeats; i++ {
		// opcode 0x90: no offset bytes (offset=0), one size byte... size
		// needs 3 bytes since 0xFFFFFF doesn't fit in one. flags: size
		// bytes 0,1,2 present -> 0x70, offset absent -> 0x00. opcode=0xF0.
		delta = append(delta, 0xF0, byte(chunk), byte(chunk>>8), byte(chunk>>16))
	}
	if _, err := Apply(base, delta); err == nil {
		t.Error("Apply with output exceeding the cap = nil error, want error")
	}
}

func TestApplyEncodeRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 16, 17, 200, 70000, 131073}
	rng := rand.New(rand.NewSource(42))
	for _, baseSize := range sizes {
		for _, targetSize := range sizes {
			base := randomBytes(rng, baseSize)
			target := randomBytes(rng, targetSize)
			d := Encode(base, target)
			got, err := Apply(base, d)
			if err != nil {
				t.Fatalf("Apply(Encode(base[%d], target[%d])) = %v, want nil error", baseSize, targetSize, err)
			}
			if !bytes.Equal(got, target) {
				t.Fatalf("Apply(Encode(base[%d], target[%d])) did not reproduce target", baseSize, targetSize)
			}
		}
	}
}

func TestApplyEncodeRoundTripNearIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	base := randomBytes(rng, 70000)
	target := append([]byte(nil), base...)
	// A handful of scattered edits: near-identical but not equal.
	for i := 0; i < 20; i++ {
		pos := rng.Intn(len(target))
		target[pos] = byte(rng.Intn(256))
	}
	target = append(target, []byte("trailing addition")...)
	d := Encode(base, target)
	got, err := Apply(base, d)
	if err != nil {
		t.Fatalf("Apply(Encode(near-identical)) = %v, want nil error", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("Apply(Encode(near-identical)) did not reproduce target")
	}
}

func TestEncodeNeverFails(t *testing.T) {
	// Pure inserts: base and target share nothing.
	base := bytes.Repeat([]byte{0x01}, 1000)
	target := bytes.Repeat([]byte{0x02}, 1000)
	d := Encode(base, target)
	got, err := Apply(base, d)
	if err != nil {
		t.Fatalf("Apply(Encode(disjoint)) = %v, want nil error", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("Apply(Encode(disjoint)) did not reproduce target")
	}
}

func TestEncodeCompressionSanity(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	base := randomText(rng, 50000)
	target := append([]byte(nil), base...)
	// A few small edits scattered through the text: near-identical.
	for i := 0; i < 15; i++ {
		pos := rng.Intn(len(target))
		target[pos] = 'x'
	}
	target = append(target, []byte(" and one more sentence at the end.")...)

	d := Encode(base, target)
	if got, want := len(d), len(target)/20; got >= want {
		t.Errorf("len(Encode(near-identical 50KB)) = %d, want < %d (5%% of target size %d)", got, want, len(target))
	}
	got, err := Apply(base, d)
	if err != nil {
		t.Fatalf("Apply(compression-sanity delta) = %v, want nil error", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("Apply(compression-sanity delta) did not reproduce target")
	}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func randomText(rng *rand.Rand, n int) []byte {
	const words = "the quick brown fox jumps over lazy dog while snapvault stores every blob tree and commit as content addressed objects "
	var b []byte
	for len(b) < n {
		b = append(b, words[rng.Intn(len(words))])
	}
	return b[:n]
}
