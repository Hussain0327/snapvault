package object

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	treeMagic      = 0x53565431 // "SVT1"
	maxTreeEntries = 1_000_000
	maxNameBytes   = 1 << 20
)

// EntryKind is the filesystem node represented by a tree entry.
type EntryKind uint8

// The tree entry kinds defined by format v1.
const (
	KindFile      EntryKind = 1
	KindDirectory EntryKind = 2
	KindSymlink   EntryKind = 3
)

// TreeEntry is a single immutable entry in a directory tree object.
type TreeEntry struct {
	Name       string
	Kind       EntryKind
	ObjectID   string
	Executable bool
}

func (e TreeEntry) validate() error {
	if e.Name == "" || e.Name == "." || e.Name == ".." ||
		strings.ContainsAny(e.Name, "/\\\x00") || !utf8.ValidString(e.Name) {
		return fmt.Errorf("unsafe tree entry name: %s", e.Name)
	}
	if e.Kind != KindFile && e.Kind != KindDirectory && e.Kind != KindSymlink {
		return fmt.Errorf("unknown tree entry kind: %d", e.Kind)
	}
	if e.Kind != KindFile && e.Executable {
		return errors.New("only regular files can be executable")
	}
	return RequireID(e.ObjectID)
}

// Tree is a canonical, sorted directory listing stored as a tree object.
type Tree struct {
	entries []TreeEntry
}

// NewTree validates entries, sorts them into canonical UTF-16 code-unit
// order, and rejects duplicate names.
func NewTree(entries []TreeEntry) (*Tree, error) {
	sorted := slices.Clone(entries)
	for _, entry := range sorted {
		if err := entry.validate(); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(sorted, func(a, b TreeEntry) int {
		return CompareNames(a.Name, b.Name)
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Name == sorted[i-1].Name {
			return nil, fmt.Errorf("duplicate tree entry: %s", sorted[i].Name)
		}
	}
	return &Tree{entries: sorted}, nil
}

// Entries returns the entries in canonical order.
func (t *Tree) Entries() []TreeEntry {
	return slices.Clone(t.entries)
}

// Encode returns the canonical binary payload defined by format v1.
func (t *Tree) Encode() []byte {
	encoded := appendInt32(nil, treeMagic)
	encoded = appendInt32(encoded, int32(len(t.entries)))
	for _, entry := range t.entries {
		encoded = appendUTF8String(encoded, entry.Name)
		executable := byte(0)
		if entry.Executable {
			executable = 1
		}
		encoded = append(encoded, byte(entry.Kind), executable)
		id, err := idBytes(entry.ObjectID)
		if err != nil {
			panic(fmt.Sprintf("tree holds an invalid object id: %v", err))
		}
		encoded = append(encoded, id...)
	}
	return encoded
}

// DecodeTree parses and validates a tree payload.
func DecodeTree(payload []byte) (*Tree, error) {
	r := &payloadReader{rest: payload}
	tree, err := decodeTree(r)
	if errors.Is(err, errTruncated) {
		return nil, errors.New("truncated tree object")
	}
	return tree, err
}

func decodeTree(r *payloadReader) (*Tree, error) {
	magic, err := r.int32()
	if err != nil {
		return nil, err
	}
	if magic != treeMagic {
		return nil, errors.New("invalid tree object signature")
	}
	count, err := r.int32()
	if err != nil {
		return nil, err
	}
	if count < 0 || count > maxTreeEntries {
		return nil, fmt.Errorf("invalid tree entry count: %d", count)
	}

	entries := make([]TreeEntry, 0, min(count, 4096))
	for range count {
		name, err := r.utf8String(maxNameBytes)
		if err != nil {
			return nil, err
		}
		kind, err := r.byte()
		if err != nil {
			return nil, err
		}
		executable, err := r.byte()
		if err != nil {
			return nil, err
		}
		id, err := r.objectID()
		if err != nil {
			return nil, err
		}
		entries = append(entries, TreeEntry{
			Name:       name,
			Kind:       EntryKind(kind),
			ObjectID:   id,
			Executable: executable != 0,
		})
	}
	if r.remaining() != 0 {
		return nil, errors.New("tree object has trailing data")
	}

	tree, err := NewTree(entries)
	if err != nil {
		return nil, fmt.Errorf("invalid tree object: %w", err)
	}
	return tree, nil
}
