package repo

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/object"
)

const (
	cacheFileName = "cache"
	cacheMagic    = "SVDC"
	cacheVersion  = 1
	maxCachePaths = 1_000_000
	maxPathBytes  = 1 << 20
)

// dirCache is the decoded working-tree cache: a path-keyed map of the last
// snapshot's regular files, used to skip hashing unchanged content.
type dirCache struct {
	writtenAt int64
	byPath    map[string]cacheRecord
}

type cacheRecord struct {
	path      string
	size      int64
	mtimeNano int64
	dev       uint64
	ino       uint64
	objectID  string
}

func loadDirCache(path string) *dirCache {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &dirCache{byPath: map[string]cacheRecord{}}
	}
	decoded, err := decodeDirCache(raw)
	if err != nil {
		return &dirCache{byPath: map[string]cacheRecord{}}
	}
	return decoded
}

func (c *dirCache) lookup(entry *pendingEntry, contains func(string) bool) (string, bool) {
	if c == nil || entry == nil {
		return "", false
	}
	rec, ok := c.byPath[entry.relPath]
	if !ok {
		return "", false
	}
	if rec.size != entry.size || rec.mtimeNano != entry.mtime.UnixNano() {
		return "", false
	}
	if rec.dev != 0 && entry.dev != 0 && rec.dev != entry.dev {
		return "", false
	}
	if rec.ino != 0 && entry.ino != 0 && rec.ino != entry.ino {
		return "", false
	}
	if entry.mtime.UnixNano() >= c.writtenAt {
		return "", false
	}
	if contains == nil || !contains(rec.objectID) {
		return "", false
	}
	return rec.objectID, true
}

func (r *Repository) writeDirCache(files []*pendingEntry) error {
	records := make([]cacheRecord, 0, len(files))
	for _, f := range files {
		if f == nil || f.kind != object.KindFile {
			continue
		}
		records = append(records, cacheRecord{
			path:      f.relPath,
			size:      f.size,
			mtimeNano: f.mtime.UnixNano(),
			dev:       f.dev,
			ino:       f.ino,
			objectID:  f.objectID,
		})
	}
	raw := encodeDirCache(time.Now().UnixNano(), records)
	dest := filepath.Join(r.metadata, cacheFileName)
	tmp, err := os.CreateTemp(r.metadata, ".cache-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}

func (r *Repository) removeDirCache() {
	_ = os.Remove(filepath.Join(r.metadata, cacheFileName))
}

func encodeDirCache(writtenAt int64, records []cacheRecord) []byte {
	valid := make([]cacheRecord, 0, len(records))
	for _, rec := range records {
		if object.RequireID(rec.objectID) == nil {
			valid = append(valid, rec)
		}
	}
	sorted := slices.Clone(valid)
	slices.SortFunc(sorted, func(a, b cacheRecord) int {
		return object.CompareNames(a.path, b.path)
	})
	var buf bytes.Buffer
	buf.WriteString(cacheMagic)
	_ = binary.Write(&buf, binary.BigEndian, int32(cacheVersion))
	_ = binary.Write(&buf, binary.BigEndian, writtenAt)
	_ = binary.Write(&buf, binary.BigEndian, int32(len(sorted)))
	for _, rec := range sorted {
		path := []byte(rec.path)
		_ = binary.Write(&buf, binary.BigEndian, int32(len(path)))
		buf.Write(path)
		_ = binary.Write(&buf, binary.BigEndian, rec.size)
		_ = binary.Write(&buf, binary.BigEndian, rec.mtimeNano)
		_ = binary.Write(&buf, binary.BigEndian, rec.dev)
		_ = binary.Write(&buf, binary.BigEndian, rec.ino)
		oid, _ := hex.DecodeString(rec.objectID)
		buf.Write(oid)
	}
	return buf.Bytes()
}

func decodeDirCache(raw []byte) (*dirCache, error) {
	in := bytes.NewReader(raw)
	magic := make([]byte, len(cacheMagic))
	if _, err := io.ReadFull(in, magic); err != nil || string(magic) != cacheMagic {
		return nil, errors.New("not a SnapVault working-tree cache")
	}
	var version int32
	if err := binary.Read(in, binary.BigEndian, &version); err != nil || version != cacheVersion {
		return nil, errors.New("unsupported working-tree cache version")
	}
	var writtenAt int64
	if err := binary.Read(in, binary.BigEndian, &writtenAt); err != nil {
		return nil, errors.New("truncated working-tree cache")
	}
	var count int32
	if err := binary.Read(in, binary.BigEndian, &count); err != nil || count < 0 || count > maxCachePaths {
		return nil, errors.New("invalid working-tree cache entry count")
	}
	decoded := &dirCache{
		writtenAt: writtenAt,
		byPath:    make(map[string]cacheRecord, count),
	}
	for i := int32(0); i < count; i++ {
		var pathLen int32
		if err := binary.Read(in, binary.BigEndian, &pathLen); err != nil || pathLen < 0 || pathLen > maxPathBytes {
			return nil, errors.New("invalid working-tree cache path")
		}
		path := make([]byte, pathLen)
		if _, err := io.ReadFull(in, path); err != nil {
			return nil, errors.New("truncated working-tree cache path")
		}
		var rec cacheRecord
		rec.path = string(path)
		if err := binary.Read(in, binary.BigEndian, &rec.size); err != nil {
			return nil, errors.New("truncated working-tree cache")
		}
		if err := binary.Read(in, binary.BigEndian, &rec.mtimeNano); err != nil {
			return nil, errors.New("truncated working-tree cache")
		}
		if err := binary.Read(in, binary.BigEndian, &rec.dev); err != nil {
			return nil, errors.New("truncated working-tree cache")
		}
		if err := binary.Read(in, binary.BigEndian, &rec.ino); err != nil {
			return nil, errors.New("truncated working-tree cache")
		}
		oid := make([]byte, object.IDHexLength/2)
		if _, err := io.ReadFull(in, oid); err != nil {
			return nil, errors.New("truncated working-tree cache object id")
		}
		rec.objectID = hex.EncodeToString(oid)
		if err := object.RequireID(rec.objectID); err != nil {
			return nil, err
		}
		decoded.byPath[rec.path] = rec
	}
	if in.Len() != 0 {
		return nil, errors.New("trailing garbage in working-tree cache")
	}
	return decoded, nil
}

func fileIdentity(info os.FileInfo) (dev, ino uint64) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(sys.Dev), sys.Ino
}

func sameStat(entry *pendingEntry, info os.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Size() != entry.size || !info.ModTime().Equal(entry.mtime) {
		return false
	}
	dev, ino := fileIdentity(info)
	if entry.dev != 0 && dev != 0 && entry.dev != dev {
		return false
	}
	if entry.ino != 0 && ino != 0 && entry.ino != ino {
		return false
	}
	return true
}
