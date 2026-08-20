package store

// This file implements format v2's "SVO2" container envelope, which wraps
// the same canonical bytes a legacy object holds — either directly (kind
// full) or as a delta against a base object also held in this store (kind
// delta) — under a zlib or zstd codec. See docs/FORMAT.md (v2 addendum) for
// the wire format.

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/klauspost/compress/zstd"

	"github.com/Hussain0327/snapvault/go/internal/delta"
	"github.com/Hussain0327/snapvault/go/internal/object"
)

const (
	// containerMagic opens every format v2 object file. Its first byte
	// (0x53) never collides with a zlib CMF byte (see isLegacyHeader), which
	// is what makes sniffing unambiguous.
	containerMagic = "SVO2"

	kindFull  byte = 0x01
	kindDelta byte = 0x02

	codecZlib byte = 0x01
	codecZstd byte = 0x02

	// baseIDLen is the raw (non-hex) byte length of a delta's base object id
	// field.
	baseIDLen = 32

	// maxDeltaChainDepth bounds how many delta hops a read follows before
	// reaching a full object; a full object is depth 0. This is the format's
	// documented cap, and it also stops a base cycle from recursing forever
	// instead of erroring cleanly.
	maxDeltaChainDepth = 32

	// maxDecodedBytes bounds any single stream a container decodes:
	// reconstructed canonical bytes or a delta instruction stream. Matches
	// the store's existing 256 MiB payload cap.
	maxDecodedBytes = maxInlinePayload

	// zstdDecoderMemory caps how much memory zstd decoding may use per
	// stream, regardless of what an (optional) frame content size claims.
	zstdDecoderMemory = 256 << 20
)

// isLegacyHeader reports whether b is a zlib CMF byte: (b & 0x0F) == 0x08,
// the on-disk signature of a format v1 object. A container's first byte,
// 'S' = 0x53, has low nibble 0x3 and can never match.
func isLegacyHeader(b byte) bool {
	return b&0x0f == 0x08
}

// containerHeader renders the 6-byte container prefix: magic, kind, codec.
func containerHeader(kind, codec byte) []byte {
	return append([]byte(containerMagic), kind, codec)
}

// writeContainerFullZstd stores payload as a container/full/zstd object: the
// container header, then a streaming zstd compression of the canonical
// envelope. This is the only form a FormatV2 store's Put and PutBlobFile
// write; container/delta is written only by repack.
func (s *Store) writeContainerFullZstd(t object.Type, payloadSize int64, payload io.Reader) (string, error) {
	if payloadSize < 0 {
		return "", errors.New("payload size cannot be negative")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.dir, "tmp-*.object")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(containerHeader(kindFull, codecZstd)); err != nil {
		tmp.Close()
		return "", err
	}

	digest := sha256.New()
	// A plain streaming writer never needs the payload buffered up front, so
	// PutBlobFile's caller-supplied file reader is compressed as it is read.
	zw, err := zstd.NewWriter(tmp)
	if err != nil {
		tmp.Close()
		return "", err
	}
	canonical := io.MultiWriter(digest, zw)
	if _, err := canonical.Write(object.Header(t, payloadSize)); err != nil {
		zw.Close()
		tmp.Close()
		return "", err
	}
	copied, err := io.CopyBuffer(canonical, payload, make([]byte, copyBufferSize))
	if err != nil {
		zw.Close()
		tmp.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if copied != payloadSize {
		return "", fmt.Errorf(
			"file changed while it was being stored (expected %d bytes, read %d)",
			payloadSize, copied)
	}

	id := hex.EncodeToString(digest.Sum(nil))
	finalID, renamed, err := s.finalizeObject(tmpPath, id)
	if err != nil {
		return "", err
	}
	if renamed {
		tmpPath = ""
	}
	return finalID, nil
}

// copyVerifiedContainer answers a top-level Get/CopyPayload for an object
// whose first byte was not a legacy zlib CMF byte: it fully resolves the
// object through loadCanonical, then hands the payload to destination.
func (s *Store) copyVerifiedContainer(
	id string, expected *object.Type, destination io.Writer, maxPayload int64,
) (object.Type, error) {
	t, canonical, err := s.loadCanonical(id, 0, map[string]bool{id: true})
	if err != nil {
		return 0, err
	}
	if expected != nil && t != *expected {
		return 0, fmt.Errorf("object %s is %s, expected %s", id, t.Token(), expected.Token())
	}
	_, payload, err := parseEnvelope(canonical)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, id)
	}
	if int64(len(payload)) > maxPayload {
		return 0, fmt.Errorf("object %s declares an implausible payload size: %d", id, len(payload))
	}
	if _, err := destination.Write(payload); err != nil {
		return 0, err
	}
	return t, nil
}

// loadCanonical returns id's fully verified canonical bytes (envelope header
// plus payload), resolving container forms — including delta chains,
// recursively loading each base up to maxDeltaChainDepth deep — and
// rejecting a container-form object outright when the store's own format is
// FormatV1. Every path through this function checks the SHA-256 digest
// before returning, deltas included. Unlike the streaming legacy fast path
// in copyVerified, it always buffers the whole object; that is what lets a
// delta's copy instructions address any offset in its base, and the design's
// 256 MiB reconstructed-object cap is what keeps that bounded.
//
// chainStack holds every id already on the current chain (the id this call
// tree was originally asked to resolve, plus every delta base visited since)
// so a cycle is caught the moment it would be revisited, rather than only
// after maxDeltaChainDepth hops of decompressing a fresh instruction stream
// at every level -- a two-object cycle would otherwise multiply the 256 MiB
// per-stream cap by up to 32 before the depth check alone rejected it.
func (s *Store) loadCanonical(id string, depth int, chainStack map[string]bool) (object.Type, []byte, error) {
	if depth > maxDeltaChainDepth {
		return 0, nil, fmt.Errorf(
			"object %s exceeds the delta chain depth cap of %d", id, maxDeltaChainDepth)
	}
	path, err := s.pathFor(id)
	if err != nil {
		return 0, nil, err
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return 0, nil, fmt.Errorf("object does not exist: %s", id)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	buffered := bufio.NewReader(f)
	first, err := buffered.Peek(1)
	if err != nil {
		return 0, nil, fmt.Errorf("object is corrupt: %s: empty object file", id)
	}

	var canonical []byte
	switch {
	case isLegacyHeader(first[0]):
		canonical, err = decodeLegacy(buffered, id)
	case first[0] == containerMagic[0]:
		if s.format != FormatV2 {
			return 0, nil, fmt.Errorf(
				"object is corrupt: %s: container-form object in a format 1 repository", id)
		}
		canonical, err = s.decodeContainer(buffered, id, depth, chainStack)
	default:
		err = fmt.Errorf("object is corrupt: %s: unrecognized object header", id)
	}
	if err != nil {
		return 0, nil, err
	}

	sum := sha256.Sum256(canonical)
	if actual := hex.EncodeToString(sum[:]); actual != id {
		return 0, nil, fmt.Errorf(
			"object failed its SHA-256 integrity check: %s (actual %s)", id, actual)
	}
	t, _, err := parseEnvelope(canonical)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s", err, id)
	}
	return t, canonical, nil
}

// decodeLegacy fully decodes a legacy zlib object into memory, for use as a
// delta base; a direct top-level read of a legacy object instead uses the
// streaming fast path in copyVerified and never calls this.
func decodeLegacy(r io.Reader, id string) ([]byte, error) {
	inflated, err := zlib.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}
	defer inflated.Close()
	return readCapped(inflated, id)
}

// decodeContainer parses the container header already peeked from r (magic,
// kind, codec) and returns the canonical bytes it describes: the decoded
// stream itself for kind full, or a delta reconstructed against its base for
// kind delta.
func (s *Store) decodeContainer(r io.Reader, id string, depth int, chainStack map[string]bool) ([]byte, error) {
	header := make([]byte, len(containerMagic)+2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("object is corrupt: %s: truncated container header", id)
	}
	if string(header[:len(containerMagic)]) != containerMagic {
		return nil, fmt.Errorf("object is corrupt: %s: bad container magic", id)
	}
	kind, codec := header[len(containerMagic)], header[len(containerMagic)+1]
	if codec != codecZlib && codec != codecZstd {
		return nil, fmt.Errorf("object is corrupt: %s: unknown container codec %#x", id, codec)
	}

	switch kind {
	case kindFull:
		return decodeCodecStream(codec, r, id)
	case kindDelta:
		return s.decodeContainerDelta(r, id, codec, depth, chainStack)
	default:
		return nil, fmt.Errorf("object is corrupt: %s: unknown container kind %#x", id, kind)
	}
}

func (s *Store) decodeContainerDelta(
	r io.Reader, id string, codec byte, depth int, chainStack map[string]bool,
) ([]byte, error) {
	rawBaseID := make([]byte, baseIDLen)
	if _, err := io.ReadFull(r, rawBaseID); err != nil {
		return nil, fmt.Errorf("object is corrupt: %s: truncated delta base id", id)
	}
	baseID := hex.EncodeToString(rawBaseID)

	instructions, err := decodeCodecStream(codec, r, id)
	if err != nil {
		return nil, err
	}
	if chainStack[baseID] {
		return nil, fmt.Errorf(
			"object is corrupt: %s: delta cycle detected while resolving base %s", id, baseID)
	}
	chainStack[baseID] = true
	_, base, err := s.loadCanonical(baseID, depth+1, chainStack)
	delete(chainStack, baseID)
	if err != nil {
		return nil, fmt.Errorf("resolving delta base for %s: %w", id, err)
	}
	target, err := delta.Apply(base, instructions)
	if err != nil {
		return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}
	return target, nil
}

// decodeCodecStream decompresses the rest of r under the named codec,
// capped at maxDecodedBytes.
func decodeCodecStream(codec byte, r io.Reader, id string) ([]byte, error) {
	switch codec {
	case codecZlib:
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
		}
		defer zr.Close()
		return readCapped(zr, id)
	case codecZstd:
		// FORMAT.md requires exactly one standard zstd frame, no skippable
		// frames: klauspost's default streaming reader instead concatenates
		// every well-formed frame it finds and silently tolerates a leading
		// skippable frame, so framing is validated statically (via the
		// frame and block headers, never a decompressed size) before any
		// byte reaches the decoder.
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
		}
		frameLen, err := zstdSingleFrameLength(raw)
		if err != nil {
			return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
		}
		if frameLen != len(raw) {
			return nil, fmt.Errorf(
				"object is corrupt: %s: codec-zstd stream carries more than one frame", id)
		}
		zr, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderMaxMemory(zstdDecoderMemory))
		if err != nil {
			return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
		}
		defer zr.Close()
		return readCapped(zr, id)
	default:
		return nil, fmt.Errorf("object is corrupt: %s: unknown container codec", id)
	}
}

// zstdSingleFrameLength returns the byte length of the single standard zstd
// frame raw is expected to hold entirely: FORMAT.md's "exactly one standard
// zstd frame, no skippable frames" rule. It walks the frame header (via the
// zstd package's own header decoder) and then every data block's header,
// each of which states its own stored content length, so the frame's exact
// end is known without decompressing anything and without ever trusting the
// optional frame content size field. The caller rejects raw as carrying more
// than one frame (or trailing garbage) whenever the returned length is
// shorter than len(raw).
func zstdSingleFrameLength(raw []byte) (int, error) {
	var header zstd.Header
	if err := header.Decode(raw); err != nil {
		return 0, fmt.Errorf("invalid zstd frame header: %w", err)
	}
	if header.Skippable {
		return 0, errors.New("skippable zstd frames are not allowed")
	}

	offset := header.HeaderSize
	for {
		if offset+3 > len(raw) {
			return 0, errors.New("truncated zstd block header")
		}
		blockHeader := uint32(raw[offset]) | uint32(raw[offset+1])<<8 | uint32(raw[offset+2])<<16
		const (
			blockTypeRaw        = 0
			blockTypeRLE        = 1
			blockTypeCompressed = 2
			blockTypeReserved   = 3
		)
		last := blockHeader&1 != 0
		blockType := (blockHeader >> 1) & 3
		blockSize := int(blockHeader >> 3)
		offset += 3

		switch blockType {
		case blockTypeRLE:
			offset++
		case blockTypeRaw, blockTypeCompressed:
			offset += blockSize
		default: // blockTypeReserved
			return 0, errors.New("reserved zstd block type")
		}
		if offset > len(raw) {
			return 0, errors.New("truncated zstd block")
		}
		if last {
			break
		}
	}
	if header.HasCheckSum {
		offset += 4
	}
	if offset > len(raw) {
		return 0, errors.New("truncated zstd checksum")
	}
	return offset, nil
}

// readCapped reads r to completion, rejecting more than maxDecodedBytes so a
// hostile or corrupt stream cannot make decoding allocate without bound.
func readCapped(r io.Reader, id string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDecodedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}
	if len(data) > maxDecodedBytes {
		return nil, fmt.Errorf("object %s decodes to more than the %d byte cap", id, maxDecodedBytes)
	}
	return data, nil
}

// parseEnvelope splits a fully-decoded canonical byte slice into its
// declared type and payload, applying the same envelope rules as the
// streaming legacy path: a well-formed header, and a payload that is
// exactly the declared size — neither truncated nor carrying trailing data.
func parseEnvelope(canonical []byte) (object.Type, []byte, error) {
	nul := bytes.IndexByte(canonical, 0)
	if nul <= 0 {
		return 0, nil, errors.New("malformed object header")
	}
	if nul >= maxHeaderBytes {
		return 0, nil, errors.New("object header is too long")
	}
	header := canonical[:nul]
	sep := bytes.IndexByte(header, ' ')
	if sep <= 0 || sep == len(header)-1 {
		return 0, nil, errors.New("malformed object header")
	}
	t, err := object.TypeFromToken(string(header[:sep]))
	if err != nil {
		return 0, nil, err
	}
	size, err := strconv.ParseInt(string(header[sep+1:]), 10, 64)
	if err != nil || size < 0 {
		return 0, nil, errors.New("malformed object size")
	}
	if size > maxDecodedBytes {
		return 0, nil, fmt.Errorf("object declares an implausible payload size: %d", size)
	}
	payload := canonical[nul+1:]
	if int64(len(payload)) != size {
		return 0, nil, fmt.Errorf("object payload is %d bytes, header declares %d", len(payload), size)
	}
	return t, payload, nil
}
