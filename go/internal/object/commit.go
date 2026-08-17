package object

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	commitMagic     = 0x53564331 // "SVC1"
	maxParents      = 64
	maxMessageBytes = 4 << 20
)

// Commit is a snapshot commit linking a root tree to zero or more parents.
type Commit struct {
	TreeID  string
	Parents []string
	Time    time.Time
	Message string
}

// NewCommit validates and builds a commit.
func NewCommit(treeID string, parents []string, at time.Time, message string) (*Commit, error) {
	if err := RequireID(treeID); err != nil {
		return nil, err
	}
	if len(parents) > maxParents {
		return nil, errors.New("a commit cannot have more than 64 parents")
	}
	for _, parent := range parents {
		if err := RequireID(parent); err != nil {
			return nil, err
		}
	}
	if strings.ContainsRune(message, 0) {
		return nil, errors.New("commit message cannot contain NUL")
	}
	if len(message) > maxMessageBytes {
		return nil, errors.New("commit message is too large")
	}
	return &Commit{
		TreeID:  treeID,
		Parents: slices.Clone(parents),
		Time:    at,
		Message: message,
	}, nil
}

// Encode returns the canonical binary payload defined by format v1.
func (c *Commit) Encode() []byte {
	encoded := appendInt32(nil, commitMagic)
	encoded = appendIDBytes(encoded, c.TreeID)
	encoded = appendInt32(encoded, int32(len(c.Parents)))
	for _, parent := range c.Parents {
		encoded = appendIDBytes(encoded, parent)
	}
	encoded = appendInt64(encoded, c.Time.Unix())
	encoded = appendInt32(encoded, int32(c.Time.Nanosecond()))
	return appendUTF8String(encoded, c.Message)
}

func appendIDBytes(dst []byte, id string) []byte {
	raw, err := idBytes(id)
	if err != nil {
		panic(fmt.Sprintf("commit holds an invalid object id: %v", err))
	}
	return append(dst, raw...)
}

// DecodeCommit parses and validates a commit payload.
func DecodeCommit(payload []byte) (*Commit, error) {
	commit, err := decodeCommit(&payloadReader{rest: payload})
	if errors.Is(err, errTruncated) {
		return nil, errors.New("truncated commit object")
	}
	return commit, err
}

func decodeCommit(r *payloadReader) (*Commit, error) {
	magic, err := r.int32()
	if err != nil {
		return nil, err
	}
	if magic != commitMagic {
		return nil, errors.New("invalid commit object signature")
	}
	treeID, err := r.objectID()
	if err != nil {
		return nil, err
	}
	parentCount, err := r.int32()
	if err != nil {
		return nil, err
	}
	if parentCount < 0 || parentCount > maxParents {
		return nil, fmt.Errorf("invalid commit parent count: %d", parentCount)
	}
	parents := make([]string, 0, parentCount)
	for range parentCount {
		parent, err := r.objectID()
		if err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	seconds, err := r.int64()
	if err != nil {
		return nil, err
	}
	nanos, err := r.int32()
	if err != nil {
		return nil, err
	}
	if nanos < 0 || nanos > 999_999_999 {
		return nil, fmt.Errorf("invalid commit nanosecond: %d", nanos)
	}
	message, err := r.utf8String(maxMessageBytes)
	if err != nil {
		return nil, err
	}
	if r.remaining() != 0 {
		return nil, errors.New("commit object has trailing data")
	}

	commit, err := NewCommit(treeID, parents, time.Unix(seconds, int64(nanos)), message)
	if err != nil {
		return nil, fmt.Errorf("invalid commit object: %w", err)
	}
	return commit, nil
}
