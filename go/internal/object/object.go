// Package object implements SnapVault's canonical format-v1 objects: the typed
// envelope that content addresses reference, and the binary tree and commit
// payloads defined in docs/FORMAT.md. Encodings are byte-for-byte identical to
// the Java reference implementation.
package object

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// Type identifies one of the three kinds of stored object.
type Type int

// The object types persisted in a SnapVault object database.
const (
	TypeBlob Type = iota
	TypeTree
	TypeCommit
)

// IDHexLength is the length of a SHA-256 object id in lowercase hex.
const IDHexLength = 64

const idByteLength = 32

// Token returns the ASCII type token used in the object envelope.
func (t Type) Token() string {
	switch t {
	case TypeBlob:
		return "blob"
	case TypeTree:
		return "tree"
	case TypeCommit:
		return "commit"
	default:
		panic(fmt.Sprintf("unknown object type %d", int(t)))
	}
}

// TypeFromToken returns the object type named by an envelope token.
func TypeFromToken(token string) (Type, error) {
	switch token {
	case "blob":
		return TypeBlob, nil
	case "tree":
		return TypeTree, nil
	case "commit":
		return TypeCommit, nil
	default:
		return 0, fmt.Errorf("unknown object type: %s", token)
	}
}

// Header returns the canonical envelope header "<type> <size>\x00" whose bytes
// prefix every payload before hashing and storage.
func Header(t Type, payloadSize int64) []byte {
	return []byte(t.Token() + " " + strconv.FormatInt(payloadSize, 10) + "\x00")
}

// ID returns the object id an in-memory payload would have if it were stored.
func ID(t Type, payload []byte) string {
	digest := sha256.New()
	digest.Write(Header(t, int64(len(payload))))
	digest.Write(payload)
	return hex.EncodeToString(digest.Sum(nil))
}

// RequireID reports whether id is a well-formed object id: exactly 64
// lowercase hexadecimal characters.
func RequireID(id string) error {
	if len(id) != IDHexLength {
		return errors.New("object id must be 64 hexadecimal characters")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("object id must use lowercase hexadecimal characters")
		}
	}
	return nil
}

func idBytes(id string) ([]byte, error) {
	if err := RequireID(id); err != nil {
		return nil, err
	}
	return hex.DecodeString(id)
}

// CompareNames orders entry names by UTF-16 code unit, matching Java String
// order. It differs from Go's byte-wise string order for names containing
// characters outside the Basic Multilingual Plane, and format v1 sorts tree
// entries in this order.
func CompareNames(a, b string) int {
	ua, ub := utf16Units{rest: a}, utf16Units{rest: b}
	for {
		ca, oka := ua.next()
		cb, okb := ub.next()
		switch {
		case !oka && !okb:
			return 0
		case !oka:
			return -1
		case !okb:
			return 1
		case ca != cb:
			if ca < cb {
				return -1
			}
			return 1
		}
	}
}

// utf16Units yields a string's UTF-16 code units one at a time. A rune above
// the BMP yields its high surrogate, then its low surrogate on the next call.
type utf16Units struct {
	rest    string
	pending rune
}

func (u *utf16Units) next() (rune, bool) {
	if u.pending != 0 {
		unit := u.pending
		u.pending = 0
		return unit, true
	}
	if u.rest == "" {
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(u.rest)
	u.rest = u.rest[size:]
	if r >= 0x10000 {
		u.pending = 0xdc00 + (r-0x10000)&0x3ff
		return 0xd800 + (r-0x10000)>>10, true
	}
	return r, true
}

var errTruncated = errors.New("truncated")

// payloadReader decodes the big-endian primitives shared by tree and commit
// payloads, reporting errTruncated when the payload ends early.
type payloadReader struct {
	rest []byte
}

func (r *payloadReader) int32() (int32, error) {
	if len(r.rest) < 4 {
		return 0, errTruncated
	}
	v := int32(r.rest[0])<<24 | int32(r.rest[1])<<16 | int32(r.rest[2])<<8 | int32(r.rest[3])
	r.rest = r.rest[4:]
	return v, nil
}

func (r *payloadReader) int64() (int64, error) {
	hi, err := r.int32()
	if err != nil {
		return 0, err
	}
	lo, err := r.int32()
	if err != nil {
		return 0, err
	}
	return int64(hi)<<32 | int64(uint32(lo)), nil
}

func (r *payloadReader) byte() (byte, error) {
	if len(r.rest) < 1 {
		return 0, errTruncated
	}
	b := r.rest[0]
	r.rest = r.rest[1:]
	return b, nil
}

func (r *payloadReader) objectID() (string, error) {
	if len(r.rest) < idByteLength {
		return "", errTruncated
	}
	id := hex.EncodeToString(r.rest[:idByteLength])
	r.rest = r.rest[idByteLength:]
	return id, nil
}

func (r *payloadReader) utf8String(maxBytes int) (string, error) {
	length, err := r.int32()
	if err != nil {
		return "", err
	}
	if length < 0 || int64(length) > int64(maxBytes) {
		return "", fmt.Errorf("invalid string length: %d", length)
	}
	if len(r.rest) < int(length) {
		return "", errTruncated
	}
	raw := r.rest[:length]
	r.rest = r.rest[length:]
	if !utf8.Valid(raw) {
		return "", errors.New("string is not valid UTF-8")
	}
	return string(raw), nil
}

func (r *payloadReader) remaining() int {
	return len(r.rest)
}

func appendInt32(dst []byte, v int32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendInt64(dst []byte, v int64) []byte {
	dst = appendInt32(dst, int32(v>>32))
	return appendInt32(dst, int32(v))
}

func appendUTF8String(dst []byte, s string) []byte {
	dst = appendInt32(dst, int32(len(s)))
	return append(dst, s...)
}
