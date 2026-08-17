// Package store implements SnapVault's content-addressed filesystem object
// database: format-v1 canonical envelopes, zlib-compressed on disk, sharded
// by the first two hex digits of the object id, written through a temporary
// file and an atomic rename. Reads verify the declared length, the absence of
// trailing data, and the SHA-256 digest before trusting an object.
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

// Store is a content-addressed object database rooted at one directory.
type Store struct {
	dir string
}

// New opens (creating if needed) the object database in objectsDir.
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

func (s *Store) writeObject(t object.Type, payloadSize int64, payload io.Reader) (string, error) {
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
	destination, err := s.pathFor(id)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("object path is not a regular file: %s", destination)
		}
		return id, nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	// Rename is atomic on POSIX; a concurrent writer landing first leaves
	// identical bytes, so replacement is harmless.
	if err := os.Rename(tmpPath, destination); err != nil {
		return "", err
	}
	tmpPath = ""
	return id, nil
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

	inflated, err := zlib.NewReader(f)
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
