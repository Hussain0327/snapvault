package object

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Golden commits produced by the Java reference implementation.
const (
	goldenRootCommitHex = "5356433179dc49597b4cc697ddad8ca36f36561ae631fa8f4fa20a391ad505022ad834" +
		"2000000000000000006553f100075bcd150000001046697273740a0a426f6479206c696e65"
	goldenRootCommitID = "15c97e30bf810809623268ebca57ca27a396d89e5f01c39d32b2c3f9486c90f3"

	goldenChildCommitHex = "53564331bccaa8985496fdc553fa99487f038d3c1c5cb5aebbe47d7d9f12bc758820" +
		"106a0000000115c97e30bf810809623268ebca57ca27a396d89e5f01c39d32b2c3f9486c90f30000000065" +
		"53f10100000000000000065365636f6e64"
	goldenChildCommitID = "7ae68059450175b7677905b62b94f348baf4c72431891e3974e33a8af32da201"
)

func TestCommitEncodeMatchesJava(t *testing.T) {
	root, err := NewCommit(
		goldenTreeID, nil, time.Unix(1700000000, 123456789), "First\n\nBody line")
	if err != nil {
		t.Fatalf("NewCommit(root) = %v", err)
	}
	if got := root.Encode(); !bytes.Equal(got, mustHex(t, goldenRootCommitHex)) {
		t.Errorf("root commit encodes to %x, want %s", got, goldenRootCommitHex)
	}
	if got := ID(TypeCommit, root.Encode()); got != goldenRootCommitID {
		t.Errorf("root commit id = %s, want %s", got, goldenRootCommitID)
	}

	child, err := NewCommit(
		goldenEmptyTreeID, []string{goldenRootCommitID}, time.Unix(1700000001, 0), "Second")
	if err != nil {
		t.Fatalf("NewCommit(child) = %v", err)
	}
	if got := child.Encode(); !bytes.Equal(got, mustHex(t, goldenChildCommitHex)) {
		t.Errorf("child commit encodes to %x, want %s", got, goldenChildCommitHex)
	}
	if got := ID(TypeCommit, child.Encode()); got != goldenChildCommitID {
		t.Errorf("child commit id = %s, want %s", got, goldenChildCommitID)
	}
}

func TestDecodeCommitRoundTripsGolden(t *testing.T) {
	commit, err := DecodeCommit(mustHex(t, goldenRootCommitHex))
	if err != nil {
		t.Fatalf("DecodeCommit = %v", err)
	}
	if commit.TreeID != goldenTreeID {
		t.Errorf("TreeID = %s, want %s", commit.TreeID, goldenTreeID)
	}
	if len(commit.Parents) != 0 {
		t.Errorf("Parents = %v, want none", commit.Parents)
	}
	if !commit.Time.Equal(time.Unix(1700000000, 123456789)) {
		t.Errorf("Time = %v, want 1700000000.123456789", commit.Time)
	}
	if commit.Message != "First\n\nBody line" {
		t.Errorf("Message = %q", commit.Message)
	}
	if got := commit.Encode(); !bytes.Equal(got, mustHex(t, goldenRootCommitHex)) {
		t.Errorf("re-encode = %x, want %s", got, goldenRootCommitHex)
	}

	child, err := DecodeCommit(mustHex(t, goldenChildCommitHex))
	if err != nil {
		t.Fatalf("DecodeCommit(child) = %v", err)
	}
	if len(child.Parents) != 1 || child.Parents[0] != goldenRootCommitID {
		t.Errorf("child.Parents = %v, want [%s]", child.Parents, goldenRootCommitID)
	}
}

func TestNewCommitRejectsInvalidInput(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if _, err := NewCommit("abc", nil, now, "m"); err == nil {
		t.Error("NewCommit accepted an invalid tree id")
	}
	if _, err := NewCommit(goldenTreeID, []string{"abc"}, now, "m"); err == nil {
		t.Error("NewCommit accepted an invalid parent id")
	}
	if _, err := NewCommit(goldenTreeID, nil, now, "a\x00b"); err == nil {
		t.Error("NewCommit accepted NUL in the message")
	}
	tooManyParents := make([]string, 65)
	for i := range tooManyParents {
		tooManyParents[i] = goldenRootCommitID
	}
	if _, err := NewCommit(goldenTreeID, tooManyParents, now, "m"); err == nil {
		t.Error("NewCommit accepted 65 parents")
	}
	if _, err := NewCommit(goldenTreeID, nil, now, strings.Repeat("x", 4*1024*1024+1)); err == nil {
		t.Error("NewCommit accepted an oversized message")
	}
}

func TestDecodeCommitRejectsCorruptPayloads(t *testing.T) {
	golden := mustHex(t, goldenRootCommitHex)
	truncated := golden[:len(golden)-1]
	trailing := append(append([]byte(nil), golden...), 0)
	badMagic := append([]byte(nil), golden...)
	badMagic[0] = 'X'
	badNano := append([]byte(nil), golden...)
	// The nanosecond int32 sits after magic(4) + tree(32) + parentCount(4) + seconds(8).
	copy(badNano[48:52], []byte{0x7f, 0xff, 0xff, 0xff})

	cases := []struct {
		desc    string
		payload []byte
	}{
		{"empty", nil},
		{"bad magic", badMagic},
		{"truncated", truncated},
		{"trailing data", trailing},
		{"nanosecond out of range", badNano},
	}
	for _, tc := range cases {
		if _, err := DecodeCommit(tc.payload); err == nil {
			t.Errorf("DecodeCommit accepted %s", tc.desc)
		}
	}
}
