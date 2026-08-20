// Package store implements SnapVault's content-addressed filesystem object
// database: canonical envelopes sharded by the first two hex digits of the
// object id, written through a temporary file and an atomic rename. A
// format 1 store always writes and reads the legacy zlib envelope; a
// format 2 store additionally reads and writes format v2 "SVO2" containers
// (see container.go), including delta reconstruction against a base object
// loaded through the same store. Reads verify the declared length, the
// absence of trailing data, and the SHA-256 digest before trusting an
// object, regardless of which on-disk form it used.
package store

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

const (
	copyBufferSize = 64 * 1024
	maxHeaderBytes = 128

	// maxInlinePayload bounds an object read whole into memory. Far above any
	// legitimate tree or commit, and far below what a corrupt header could
	// otherwise make this process allocate. Streamed blobs are not subject
	// to it.
	maxInlinePayload = 256 << 20
)

// Format selects which on-disk object representation Put and PutBlobFile
// write. The zero value behaves as FormatV1, so a Store returned by New
// writes legacy bytes until SetFormat says otherwise; that is what keeps a
// format 1 repository's writes byte-identical without New itself needing a
// parameter every caller must pass.
type Format int

// The store formats a repository's ".snapvault/format" file can select.
const (
	FormatV1 Format = 1
	FormatV2 Format = 2
)

// Store is a content-addressed object database rooted at one directory.
type Store struct {
	dir    string
	format Format
}

// New opens (creating if needed) the object database in objectsDir. The
// returned store writes format v1 (legacy) objects until SetFormat is
// called.
func New(objectsDir string) (*Store, error) {
	abs, err := filepath.Abs(objectsDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: abs}, nil
}

// SetFormat changes which on-disk representation subsequent writes use.
// Reads are unaffected except that a container-form object (see
// container.go) is rejected as corrupt while the store's format is
// FormatV1, matching the rule that format v2 containers never legitimately
// appear in a format 1 repository.
func (s *Store) SetFormat(format Format) {
	s.format = format
}

// Put stores an in-memory payload and returns its object id.
func (s *Store) Put(t object.Type, payload []byte) (string, error) {
	return s.writeObject(t, int64(len(payload)), bytes.NewReader(payload))
}

// PutBlobFile streams a file into the store as a blob, so file size is not
// bounded by memory, and fails if the file changes size while being read.
func (s *Store) PutBlobFile(source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return s.writeObject(object.TypeBlob, info.Size(), f)
}

// BlobFileID returns the blob id a file would have if it were stored,
// without writing anything. Ids match PutBlobFile exactly because both hash
// the same canonical envelope.
func BlobFileID(source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	f, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer f.Close()

	digest := sha256.New()
	digest.Write(object.Header(object.TypeBlob, info.Size()))
	copied, err := io.CopyBuffer(digest, f, make([]byte, copyBufferSize))
	if err != nil {
		return "", err
	}
	if copied != info.Size() {
		return "", fmt.Errorf(
			"file changed while it was being read (expected %d bytes, read %d)",
			info.Size(), copied)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// writeObject dispatches to the on-disk form the store's format calls for.
// FormatV1 always writes the legacy envelope; FormatV2 writes format v2
// container/full/zstd, matching "Who writes what" in the v2 design: only
// repack (a later addition) ever writes container/delta. A payload whose
// canonical bytes would exceed maxInlinePayload falls back to the legacy
// form even in a FormatV2 store, since every v2 read path (unlike the
// legacy streaming path) buffers a reconstructed object whole and enforces
// that same cap -- container/full/zstd would otherwise write an object
// nothing, including this store's own Get, can ever read back. Legacy is
// legal in a format 2 repository forever, so this loses no capability.
func (s *Store) writeObject(t object.Type, payloadSize int64, payload io.Reader) (string, error) {
	if s.format == FormatV2 && payloadSize >= 0 &&
		payloadSize <= maxInlinePayload-int64(len(object.Header(t, payloadSize))) {
		return s.writeContainerFullZstd(t, payloadSize, payload)
	}
	return s.writeLegacy(t, payloadSize, payload)
}

func (s *Store) writeLegacy(t object.Type, payloadSize int64, payload io.Reader) (string, error) {
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

	digest := sha256.New()
	compressed := zlib.NewWriter(tmp)
	canonical := io.MultiWriter(digest, compressed)
	if _, err := canonical.Write(object.Header(t, payloadSize)); err != nil {
		tmp.Close()
		return "", err
	}
	copied, err := io.CopyBuffer(canonical, payload, make([]byte, copyBufferSize))
	if err != nil {
		tmp.Close()
		return "", err
	}
	if err := compressed.Close(); err != nil {
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

// finalizeObject moves a fully-written temporary file into place at id's
// shard path, deduplicating against an object already stored there. renamed
// reports whether tmpPath was consumed, so a caller's deferred cleanup knows
// whether there is still a temporary file to remove.
func (s *Store) finalizeObject(tmpPath, id string) (result string, renamed bool, err error) {
	destination, err := s.pathFor(id)
	if err != nil {
		return "", false, err
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("object path is not a regular file: %s", destination)
		}
		return id, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", false, err
	}
	// Rename is atomic on POSIX; a concurrent writer landing first leaves
	// identical bytes, so replacement is harmless.
	if err := os.Rename(tmpPath, destination); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Get reads an object whole, verifying its envelope and digest.
func (s *Store) Get(id string) (object.Type, []byte, error) {
	var payload bytes.Buffer
	t, err := s.copyVerified(id, nil, &payload, maxInlinePayload)
	if err != nil {
		return 0, nil, err
	}
	return t, payload.Bytes(), nil
}

// CopyPayload streams an object's payload into destination, verifying the
// envelope, the expected type, and the digest of the complete object.
func (s *Store) CopyPayload(id string, expected object.Type, destination io.Writer) error {
	_, err := s.copyVerified(id, &expected, destination, math.MaxInt64)
	return err
}

// copyVerified reads one object, whichever on-disk form it uses, verifying
// its envelope and digest before any of its payload reaches destination. A
// legacy object streams straight through, matching the store's original
// behavior exactly; a container form (full or delta) is fully reconstructed
// in memory first by loadCanonical, since a delta's copy instructions need
// random access into its base.
func (s *Store) copyVerified(
	id string, expected *object.Type, destination io.Writer, maxPayload int64,
) (object.Type, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return 0, err
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("object does not exist: %s", id)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buffered := bufio.NewReader(f)
	first, err := buffered.Peek(1)
	if err != nil {
		return 0, fmt.Errorf("object is corrupt: %s: empty object file", id)
	}
	if !isLegacyHeader(first[0]) {
		return s.copyVerifiedContainer(id, expected, destination, maxPayload)
	}

	inflated, err := zlib.NewReader(buffered)
	if err != nil {
		return 0, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}
	defer inflated.Close()
	digest := sha256.New()
	canonical := bufio.NewReader(io.TeeReader(inflated, digest))

	t, payloadSize, err := readHeader(canonical)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, id)
	}
	if payloadSize > maxPayload {
		return 0, fmt.Errorf("object %s declares an implausible payload size: %d", id, payloadSize)
	}
	if expected != nil && t != *expected {
		return 0, fmt.Errorf("object %s is %s, expected %s", id, t.Token(), expected.Token())
	}

	if _, err := io.CopyN(destination, canonical, payloadSize); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("truncated object payload: %s", id)
		}
		return 0, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}
	if _, err := canonical.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, fmt.Errorf("object has trailing data: %s", id)
		}
		return 0, fmt.Errorf("object is corrupt: %s: %w", id, err)
	}

	if actual := hex.EncodeToString(digest.Sum(nil)); actual != id {
		return 0, fmt.Errorf(
			"object failed its SHA-256 integrity check: %s (actual %s)", id, actual)
	}
	return t, nil
}

func readHeader(r *bufio.Reader) (object.Type, int64, error) {
	var header []byte
	for range maxHeaderBytes {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, 0, errors.New("truncated object header")
			}
			return 0, 0, fmt.Errorf("object is corrupt: %w", err)
		}
		if b != 0 {
			header = append(header, b)
			continue
		}
		separator := bytes.IndexByte(header, ' ')
		if separator <= 0 || separator == len(header)-1 {
			return 0, 0, errors.New("malformed object header")
		}
		t, err := object.TypeFromToken(string(header[:separator]))
		if err != nil {
			return 0, 0, err
		}
		size, err := strconv.ParseInt(string(header[separator+1:]), 10, 64)
		if err != nil {
			return 0, 0, errors.New("malformed object size")
		}
		if size < 0 {
			return 0, 0, errors.New("negative object size")
		}
		return t, size, nil
	}
	return 0, 0, errors.New("object header is too long")
}

// Contains reports whether id names a stored object. Malformed ids are
// simply absent.
func (s *Store) Contains(id string) bool {
	path, err := s.pathFor(id)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// FindByPrefix returns the sorted ids of stored objects beginning with a
// 2- to 64-character hexadecimal prefix.
func (s *Store) FindByPrefix(prefix string) ([]string, error) {
	normalized := strings.ToLower(prefix)
	if len(normalized) < 2 || len(normalized) > object.IDHexLength {
		return nil, errors.New("object prefix must contain 2 to 64 hex characters")
	}
	for i := 0; i < len(normalized); i++ {
		c := normalized[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errors.New("object prefix must be hexadecimal")
		}
	}

	shardName, filePrefix := normalized[:2], normalized[2:]
	entries, err := os.ReadDir(filepath.Join(s.dir, shardName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == object.IDHexLength-2 &&
			strings.HasPrefix(name, filePrefix) && entry.Type().IsRegular() {
			matches = append(matches, shardName+name)
		}
	}
	slices.Sort(matches)
	return matches, nil
}

// Count returns the number of stored objects.
func (s *Store) Count() (int64, error) {
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, shard := range shards {
		if len(shard.Name()) != 2 || !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, shard.Name()))
		if err != nil {
			return 0, err
		}
		for _, file := range files {
			if len(file.Name()) == object.IDHexLength-2 && file.Type().IsRegular() {
				total++
			}
		}
	}
	return total, nil
}

func (s *Store) pathFor(id string) (string, error) {
	if err := object.RequireID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id[:2], id[2:]), nil
}
