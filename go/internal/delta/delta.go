// Package delta implements format v2's delta compression: the Git pack delta
// wire format used to store one blob as a base object id plus a small
// instruction stream, decoded byte-exactly per docs/FORMAT.md section "Delta
// instruction format" (currently drafted in the v2 design document).
package delta

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	// maxOutputBytes bounds both the declared target size and the bytes
	// Apply is willing to accumulate, matching the object store's existing
	// 256 MiB cap on reconstructed canonical bytes.
	maxOutputBytes = 256 << 20

	// blockSize is both the stride at which Encode indexes the base and the
	// minimum length of a copy instruction it emits; a match shorter than
	// this is cheaper to represent as literal bytes.
	blockSize = 16

	// maxInsertBytes is the largest literal length an insert opcode can
	// carry: opcodes 0x01..0x7F (the high bit marks a copy instead).
	maxInsertBytes = 0x7F

	// maxCopyBytes is the largest size a single copy instruction can encode
	// directly (three little-endian size bytes); larger matches are split.
	maxCopyBytes = 0xFFFFFF
)

// Apply reconstructs the target bytes a delta instruction stream describes,
// given the base bytes it was computed against. It validates the stream
// strictly: srcSize must match len(base) exactly, every copy is bounds
// checked against base, and the reconstructed length must equal tgtSize
// exactly once the stream is exhausted.
func Apply(base, delta []byte) ([]byte, error) {
	r := &reader{data: delta}

	srcSize, err := r.varint()
	if err != nil {
		return nil, fmt.Errorf("delta srcSize: %w", err)
	}
	if srcSize != uint64(len(base)) {
		return nil, fmt.Errorf("delta srcSize %d does not match base length %d", srcSize, len(base))
	}

	tgtSize, err := r.varint()
	if err != nil {
		return nil, fmt.Errorf("delta tgtSize: %w", err)
	}
	if tgtSize > maxOutputBytes {
		return nil, fmt.Errorf("delta target size %d exceeds the %d byte cap", tgtSize, maxOutputBytes)
	}

	out := make([]byte, 0, tgtSize)
	for r.remaining() > 0 {
		op, err := r.readByte()
		if err != nil {
			return nil, err // unreachable: remaining() > 0 guarantees a byte
		}
		switch {
		case op == 0x00:
			return nil, errors.New("delta opcode 0x00 is invalid")
		case op&0x80 == 0:
			literal, err := r.take(int(op))
			if err != nil {
				return nil, fmt.Errorf("truncated insert literal: %w", err)
			}
			out = append(out, literal...)
		default:
			offset, size, err := decodeCopy(r, op)
			if err != nil {
				return nil, err
			}
			end := offset + size
			if end > uint64(len(base)) {
				return nil, fmt.Errorf(
					"copy(offset=%d, size=%d) is out of bounds for a %d byte base", offset, size, len(base))
			}
			out = append(out, base[offset:end]...)
		}
		if uint64(len(out)) > maxOutputBytes {
			return nil, fmt.Errorf("reconstructed output exceeds the %d byte cap", maxOutputBytes)
		}
	}

	if uint64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta reconstructed %d bytes, want tgtSize %d", len(out), tgtSize)
	}
	return out, nil
}

// decodeCopy reads the offset and size operands that follow a copy opcode.
// Bits 0-3 of op say which of the 4 little-endian offset bytes are present;
// bits 4-6 say which of the 3 little-endian size bytes are present. An
// assembled size of 0 means 65536, per the wire format.
func decodeCopy(r *reader, op byte) (offset, size uint64, err error) {
	var o, s uint32
	for i := 0; i < 4; i++ {
		if op&(1<<uint(i)) == 0 {
			continue
		}
		b, err := r.readByte()
		if err != nil {
			return 0, 0, fmt.Errorf("truncated copy offset: %w", err)
		}
		o |= uint32(b) << uint(8*i)
	}
	for i := 0; i < 3; i++ {
		if op&(1<<uint(4+i)) == 0 {
			continue
		}
		b, err := r.readByte()
		if err != nil {
			return 0, 0, fmt.Errorf("truncated copy size: %w", err)
		}
		s |= uint32(b) << uint(8*i)
	}
	if s == 0 {
		s = 65536
	}
	return uint64(o), uint64(s), nil
}

// reader consumes a delta instruction stream one varint, byte, or literal run
// at a time, tracking how much input remains.
type reader struct {
	data []byte
}

func (r *reader) remaining() int {
	return len(r.data)
}

func (r *reader) readByte() (byte, error) {
	if len(r.data) == 0 {
		return 0, errors.New("unexpected end of delta stream")
	}
	b := r.data[0]
	r.data = r.data[1:]
	return b, nil
}

func (r *reader) take(n int) ([]byte, error) {
	if len(r.data) < n {
		return nil, errors.New("unexpected end of delta stream")
	}
	b := r.data[:n]
	r.data = r.data[n:]
	return b, nil
}

// varint decodes a little-endian base-128 varint: 7 data bits per byte, with
// the high bit marking whether another byte follows.
func (r *reader) varint() (uint64, error) {
	var v uint64
	var shift uint
	for i := 0; ; i++ {
		if i >= 10 {
			return 0, errors.New("varint is too long")
		}
		b, err := r.readByte()
		if err != nil {
			return 0, errors.New("truncated varint")
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
}

// appendVarint appends v to dst as a little-endian base-128 varint.
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// Encode produces a valid delta instruction stream that Apply(base, ...)
// turns back into target. It never fails: with no shared content the result
// is simply a stream of insert instructions covering all of target.
//
// Matching indexes base at a fixed 16-byte stride, looks up each target
// position's window in that index, and on a hit extends the match forward
// and backward (into the pending literal run) before emitting a copy.
func Encode(base, target []byte) []byte {
	out := appendVarint(nil, uint64(len(base)))
	out = appendVarint(out, uint64(len(target)))

	index := indexBase(base)
	literalStart := 0
	j := 0
	for j+blockSize <= len(target) {
		bpos, ok := index[windowHash(target[j:j+blockSize])]
		if !ok || !bytes.Equal(base[bpos:bpos+blockSize], target[j:j+blockSize]) {
			j++
			continue
		}

		start, bStart := j, bpos
		for start > literalStart && bStart > 0 && target[start-1] == base[bStart-1] {
			start--
			bStart--
		}
		end, bEnd := j+blockSize, bpos+blockSize
		for end < len(target) && bEnd < len(base) && target[end] == base[bEnd] {
			end++
			bEnd++
		}

		out = appendLiterals(out, target[literalStart:start])
		out = appendCopies(out, uint64(bStart), uint64(end-start))
		j = end
		literalStart = j
	}

	return appendLiterals(out, target[literalStart:])
}

// appendLiterals emits target as a chain of insert instructions, each
// carrying at most maxInsertBytes literal bytes.
func appendLiterals(dst, target []byte) []byte {
	for len(target) > 0 {
		n := len(target)
		if n > maxInsertBytes {
			n = maxInsertBytes
		}
		dst = append(dst, byte(n))
		dst = append(dst, target[:n]...)
		target = target[n:]
	}
	return dst
}

// appendCopies emits a run of size bytes starting at offset in the base as a
// chain of copy instructions, each carrying at most maxCopyBytes.
func appendCopies(dst []byte, offset, size uint64) []byte {
	for size > 0 {
		n := size
		if n > maxCopyBytes {
			n = maxCopyBytes
		}
		dst = append(dst, encodeCopy(uint32(offset), uint32(n))...)
		offset += n
		size -= n
	}
	return dst
}

// encodeCopy renders one copy instruction: opcode byte 0x80 with a bit set
// for each significant little-endian offset/size byte, followed by those
// bytes. size must be nonzero, so the significant-byte encoding never needs
// the wire format's zero-means-65536 shorthand.
func encodeCopy(offset, size uint32) []byte {
	op := byte(0x80)
	var operands []byte
	for i := 0; i < 4; i++ {
		if significant(offset, i) {
			op |= 1 << uint(i)
			operands = append(operands, byte(offset>>uint(8*i)))
		}
	}
	for i := 0; i < 3; i++ {
		if significant(size, i) {
			op |= 1 << uint(4+i)
			operands = append(operands, byte(size>>uint(8*i)))
		}
	}
	return append([]byte{op}, operands...)
}

// significant reports whether byte i (0 = least significant) is needed to
// represent v: it is included if it, or any more significant byte, is
// nonzero.
func significant(v uint32, i int) bool {
	return v>>uint(8*i) != 0
}

// hashMultiplier is an arbitrary odd 64-bit constant (the FNV-1a prime) used
// to build a simple polynomial hash over fixed-size windows.
const hashMultiplier = 1099511628211

func windowHash(w []byte) uint64 {
	var h uint64
	for _, b := range w {
		h = h*hashMultiplier + uint64(b)
	}
	return h
}

// indexBase maps the hash of every non-overlapping blockSize-byte window of
// base to that window's starting offset. A later window with the same hash
// overwrites an earlier one; Encode always confirms a hit against the actual
// bytes, so a lost candidate only costs compression ratio, never correctness.
func indexBase(base []byte) map[uint64]int {
	index := make(map[uint64]int, len(base)/blockSize)
	for i := 0; i+blockSize <= len(base); i += blockSize {
		index[windowHash(base[i:i+blockSize])] = i
	}
	return index
}
