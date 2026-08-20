package store

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/Hussain0327/snapvault/go/internal/delta"
	"github.com/Hussain0327/snapvault/go/internal/object"
)

// newV2TestStore builds a store that writes format v2 containers.
func newV2TestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	s.SetFormat(FormatV2)
	return s
}

func TestPutInV2StoreWritesContainerFull(t *testing.T) {
	s := newV2TestStore(t)
	payload := []byte("hello world\n")

	id, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	if want := object.ID(object.TypeBlob, payload); id != want {
		t.Errorf("Put returned id %s, want %s", id, want)
	}

	raw, err := os.ReadFile(filepath.Join(s.dir, id[:2], id[2:]))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if len(raw) < 6 || string(raw[:4]) != containerMagic {
		t.Fatalf("stored object does not start with the SVO2 magic: %#v", raw[:min(6, len(raw))])
	}
	if raw[4] != kindFull {
		t.Errorf("kind byte = %#x, want kindFull", raw[4])
	}
	if raw[5] != codecZstd {
		t.Errorf("codec byte = %#x, want codecZstd", raw[5])
	}

	typ, got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if typ != object.TypeBlob {
		t.Errorf("Get type = %v, want TypeBlob", typ)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get payload = %q, want %q", got, payload)
	}
}

func TestPutBlobFileInV2StoreRoundTrips(t *testing.T) {
	s := newV2TestStore(t)
	content := bytes.Repeat([]byte("streamed through a zstd frame\n"), 5_000)
	source := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}

	id, err := s.PutBlobFile(source)
	if err != nil {
		t.Fatalf("PutBlobFile = %v", err)
	}
	if want := object.ID(object.TypeBlob, content); id != want {
		t.Errorf("PutBlobFile id = %s, want %s", id, want)
	}

	var sink bytes.Buffer
	if err := s.CopyPayload(id, object.TypeBlob, &sink); err != nil {
		t.Fatalf("CopyPayload = %v", err)
	}
	if !bytes.Equal(sink.Bytes(), content) {
		t.Error("restored payload differs from the source file")
	}
}

func TestContainerFullZlibReads(t *testing.T) {
	s := newV2TestStore(t)
	payload := []byte("a container/full/zlib object, hand-crafted")
	id := object.ID(object.TypeTree, payload)
	writeContainerFull(t, s, id, codecZlib, object.Header(object.TypeTree, int64(len(payload))), payload)

	typ, got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if typ != object.TypeTree {
		t.Errorf("Get type = %v, want TypeTree", typ)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get payload = %q, want %q", got, payload)
	}
}

func TestContainerDeltaZlibAndZstdReconstruct(t *testing.T) {
	for _, codec := range []byte{codecZlib, codecZstd} {
		codec := codec
		t.Run(codecName(codec), func(t *testing.T) {
			s := newV2TestStore(t)
			basePayload := []byte("the quick brown fox jumps over the lazy dog, more than once, again and again")
			baseID, err := s.Put(object.TypeBlob, basePayload)
			if err != nil {
				t.Fatalf("Put base = %v", err)
			}

			targetPayload := append(append([]byte(nil), basePayload...), []byte(" plus a tail")...)
			targetCanonical := append(object.Header(object.TypeBlob, int64(len(targetPayload))), targetPayload...)
			baseCanonical := append(object.Header(object.TypeBlob, int64(len(basePayload))), basePayload...)
			instructions := delta.Encode(baseCanonical, targetCanonical)
			targetID := object.ID(object.TypeBlob, targetPayload)

			writeContainerDelta(t, s, targetID, codec, baseID, instructions)

			typ, got, err := s.Get(targetID)
			if err != nil {
				t.Fatalf("Get(delta) = %v", err)
			}
			if typ != object.TypeBlob {
				t.Errorf("Get type = %v, want TypeBlob", typ)
			}
			if !bytes.Equal(got, targetPayload) {
				t.Errorf("Get(delta) payload = %q, want %q", got, targetPayload)
			}
		})
	}
}

// TestOversizedBlobInV2StoreFallsBackToLegacy reproduces a blob larger than
// the 256 MiB cap every v2 read path enforces: writeContainerFullZstd has no
// size ceiling of its own, so without a fallback the write "succeeds" and
// produces an object no reader (including this store's own Get) can ever
// decode. A payload over the cap must instead land in the legacy form,
// which the streaming read path leaves uncapped, exactly as it does in a
// format 1 repository.
func TestOversizedBlobInV2StoreFallsBackToLegacy(t *testing.T) {
	s := newV2TestStore(t)
	source := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(source)
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	size := int64(maxInlinePayload) + 4096 // just over the 256 MiB read cap.
	if err := f.Truncate(size); err != nil {
		t.Fatalf("Truncate = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	id, err := s.PutBlobFile(source)
	if err != nil {
		t.Fatalf("PutBlobFile = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(s.dir, id[:2], id[2:]))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if len(raw) == 0 || !isLegacyHeader(raw[0]) {
		t.Fatalf("oversized blob in a v2 store did not fall back to the legacy form: first byte %#x", raw[0])
	}

	var sink bytes.Buffer
	if err := s.CopyPayload(id, object.TypeBlob, &sink); err != nil {
		t.Fatalf("CopyPayload(oversized blob) = %v, want it to stream back out", err)
	}
	if int64(sink.Len()) != size {
		t.Errorf("CopyPayload restored %d bytes, want %d", sink.Len(), size)
	}
}

func TestLegacyObjectReadableInV2Store(t *testing.T) {
	s := newV2TestStore(t)
	// Put with format v1 semantics first, then flip the same store to v2:
	// a legacy object already on disk must stay readable forever.
	s.SetFormat(FormatV1)
	payload := []byte("written back when this repository was still format 1")
	id, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	s.SetFormat(FormatV2)

	typ, got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get(legacy object in v2 store) = %v", err)
	}
	if typ != object.TypeBlob || !bytes.Equal(got, payload) {
		t.Errorf("Get = (%v, %q), want (TypeBlob, %q)", typ, got, payload)
	}
}

func TestContainerObjectRejectedInV1Store(t *testing.T) {
	s := newTestStore(t) // default format is v1
	payload := []byte("should never appear in a format 1 repository")
	id := object.ID(object.TypeBlob, payload)
	writeContainerFull(t, s, id, codecZstd, object.Header(object.TypeBlob, int64(len(payload))), payload)

	if _, _, err := s.Get(id); err == nil || !strings.Contains(err.Error(), "format 1") {
		t.Errorf("Get(container in v1 store) = %v, want a format 1 rejection error", err)
	}
}

func TestUnknownContainerKindAndCodecAreCorrupt(t *testing.T) {
	s := newV2TestStore(t)

	badKind := strings.Repeat("aa", 32)
	writeRawFile(t, s, badKind, append([]byte(containerMagic), 0x99, codecZstd))
	if _, _, err := s.Get(badKind); err == nil {
		t.Error("Get(unknown kind) = nil error, want error")
	}

	badCodec := strings.Repeat("bb", 32)
	writeRawFile(t, s, badCodec, append([]byte(containerMagic), kindFull, 0x99))
	if _, _, err := s.Get(badCodec); err == nil {
		t.Error("Get(unknown codec) = nil error, want error")
	}

	partialMagic := strings.Repeat("cc", 32)
	writeRawFile(t, s, partialMagic, []byte("SVX"))
	if _, _, err := s.Get(partialMagic); err == nil {
		t.Error("Get(partial magic) = nil error, want error")
	}

	empty := strings.Repeat("dd", 32)
	writeRawFile(t, s, empty, nil)
	if _, _, err := s.Get(empty); err == nil {
		t.Error("Get(empty object) = nil error, want error")
	}
}

func TestContainerDeltaMissingBaseIsAnError(t *testing.T) {
	s := newV2TestStore(t)
	missingBase := strings.Repeat("ee", 32)
	targetID := strings.Repeat("ff", 32)
	writeContainerDelta(t, s, targetID, codecZstd, missingBase, delta.Encode(nil, []byte("x")))

	if _, _, err := s.Get(targetID); err == nil {
		t.Error("Get(delta with missing base) = nil error, want error")
	}
}

func TestContainerDigestMismatchIsCorrupt(t *testing.T) {
	s := newV2TestStore(t)
	payload := []byte("tampered after the fact")
	id, err := s.Put(object.TypeBlob, payload)
	if err != nil {
		t.Fatalf("Put = %v", err)
	}
	path := filepath.Join(s.dir, id[:2], id[2:])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, _, err := s.Get(id); err == nil {
		t.Error("Get(tampered container) = nil error, want error")
	}
}

// TestContainerDeltaCycleFailsImmediately builds two objects that are each
// other's delta base and asserts resolving either one fails with a cycle
// error right away, rather than recursing to the depth cap (32 hops,
// decompressing a fresh instruction stream at every level) before giving up.
func TestContainerDeltaCycleFailsImmediately(t *testing.T) {
	s := newV2TestStore(t)
	idA := strings.Repeat("aa", 32)
	idB := strings.Repeat("bb", 32)
	writeContainerDelta(t, s, idA, codecZstd, idB, delta.Encode(nil, []byte("x")))
	writeContainerDelta(t, s, idB, codecZstd, idA, delta.Encode(nil, []byte("y")))

	_, _, err := s.Get(idA)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("Get(cyclic delta) = %v, want a delta cycle error", err)
	}
}

// TestContainerZstdRejectsMultiFrameSkippableAndTrailingGarbage covers the
// three ways a codec-zstd stream can violate FORMAT.md's "exactly one
// standard zstd frame, no skippable frames" rule. Before the fix, klauspost's
// default reader silently concatenated a second frame, silently skipped a
// leading skippable frame, and failed on trailing garbage with an unrelated
// error -- none of which is the explicit, spec-matching rejection every
// on-disk form here should get.
func TestContainerZstdRejectsMultiFrameSkippableAndTrailingGarbage(t *testing.T) {
	s := newV2TestStore(t)
	payload := []byte("hello frames")
	canonical := append(append([]byte(nil), object.Header(object.TypeBlob, int64(len(payload)))...), payload...)
	// id matches the *single-frame* canonical bytes exactly, so a rejection
	// can only come from framing enforcement -- padding the stream with a
	// second frame or trailing bytes would otherwise also fail on a simple
	// digest mismatch, masking whether framing itself is actually checked.
	id := object.ID(object.TypeBlob, payload)
	frame := zstdFrame(t, canonical)

	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 0x04, 0x00, 0x00, 0x00, 0xde, 0xad, 0xbe, 0xef}

	cases := []struct {
		name string
		body []byte
	}{
		{"twoFrames", append(append([]byte(nil), frame...), frame...)},
		{"trailingGarbage", append(append([]byte(nil), frame...), 0x01, 0x02, 0x03)},
		{"skippableFirst", append(append([]byte(nil), skippable...), frame...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := append(containerHeader(kindFull, codecZstd), c.body...)
			writeRawFile(t, s, id, raw)
			if _, _, err := s.Get(id); err == nil {
				t.Errorf("Get(%s) = nil error, want a framing rejection", c.name)
			}
		})
	}
}

// zstdFrame compresses data into exactly one standard zstd frame.
func zstdFrame(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter = %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("zstd write = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close = %v", err)
	}
	return buf.Bytes()
}

func TestDeltaChainDepthCap(t *testing.T) {
	s := newV2TestStore(t)

	basePayload := []byte("base of a long delta chain")
	id, err := s.Put(object.TypeBlob, basePayload)
	if err != nil {
		t.Fatalf("Put base = %v", err)
	}
	canonical := append(object.Header(object.TypeBlob, int64(len(basePayload))), basePayload...)

	// Build a chain of 32 deltas, each one byte longer than its base's
	// payload; a full object is depth 0, so this chain's tip sits at the
	// documented cap of 32 delta hops and must still resolve.
	for i := 0; i < 32; i++ {
		payload := append(append([]byte(nil), canonical[len(canonical)-len(basePayload):]...), byte('a'+i))
		next := append(object.Header(object.TypeBlob, int64(len(payload))), payload...)
		nextID := object.ID(object.TypeBlob, payload)
		writeContainerDelta(t, s, nextID, codecZstd, id, delta.Encode(canonical, next))
		id, canonical, basePayload = nextID, next, payload
	}

	if _, _, err := s.Get(id); err != nil {
		t.Fatalf("Get(chain at depth 32) = %v, want nil error", err)
	}

	// One more hop pushes the chain to depth 33, past the cap.
	payload := append(append([]byte(nil), basePayload...), 'z')
	next := append(object.Header(object.TypeBlob, int64(len(payload))), payload...)
	nextID := object.ID(object.TypeBlob, payload)
	writeContainerDelta(t, s, nextID, codecZstd, id, delta.Encode(canonical, next))

	if _, _, err := s.Get(nextID); err == nil {
		t.Error("Get(chain at depth 33) = nil error, want depth cap error")
	}
}

func codecName(codec byte) string {
	if codec == codecZlib {
		return "zlib"
	}
	return "zstd"
}

// writeContainerFull hand-crafts a container/full object at id's shard path,
// compressing canonical (header+payload) with the given codec.
func writeContainerFull(t *testing.T, s *Store, id string, codec byte, header, payload []byte) {
	t.Helper()
	canonical := append(append([]byte(nil), header...), payload...)
	body := compressWith(t, codec, canonical)
	raw := append(containerHeader(kindFull, codec), body...)
	writeRawFile(t, s, id, raw)
}

// writeContainerDelta hand-crafts a container/delta object at id's shard
// path: the base id as 32 raw bytes, then instructions compressed with the
// given codec.
func writeContainerDelta(t *testing.T, s *Store, id string, codec byte, baseID string, instructions []byte) {
	t.Helper()
	baseRaw, err := hex.DecodeString(baseID)
	if err != nil {
		t.Fatalf("hex.DecodeString(%q) = %v", baseID, err)
	}
	body := compressWith(t, codec, instructions)
	raw := append(containerHeader(kindDelta, codec), baseRaw...)
	raw = append(raw, body...)
	writeRawFile(t, s, id, raw)
}

func compressWith(t *testing.T, codec byte, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch codec {
	case codecZlib:
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zlib write = %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zlib close = %v", err)
		}
	case codecZstd:
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd.NewWriter = %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zstd write = %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zstd close = %v", err)
		}
	default:
		t.Fatalf("unknown codec %#x", codec)
	}
	return buf.Bytes()
}

// writeRawFile places raw bytes directly at id's shard path, bypassing Put
// so tests can plant hand-crafted or malformed objects.
func writeRawFile(t *testing.T, s *Store, id string, raw []byte) {
	t.Helper()
	shard := filepath.Join(s.dir, id[:2])
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, id[2:]), raw, 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}
