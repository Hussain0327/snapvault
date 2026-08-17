package object

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Golden values produced by the Java reference implementation (GoldenVectors.java).
const (
	goldenBlobID      = "0bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23143d"
	goldenEmptyTreeID = "bccaa8985496fdc553fa99487f038d3c1c5cb5aebbe47d7d9f12bc758820106a"
)

func TestIDMatchesJavaBlobID(t *testing.T) {
	got := ID(TypeBlob, []byte("hello world\n"))
	if got != goldenBlobID {
		t.Errorf("ID(TypeBlob, ...) = %s, want %s", got, goldenBlobID)
	}
}

func TestIDDependsOnType(t *testing.T) {
	payload := []byte("same payload")
	if ID(TypeBlob, payload) == ID(TypeTree, payload) {
		t.Error("blob and tree ids for equal payloads must differ")
	}
}

func TestHeaderIsTypeSizeNul(t *testing.T) {
	got := Header(TypeCommit, 42)
	want := "commit 42\x00"
	if string(got) != want {
		t.Errorf("Header(TypeCommit, 42) = %q, want %q", got, want)
	}
}

func TestRequireIDAcceptsGolden(t *testing.T) {
	if err := RequireID(goldenBlobID); err != nil {
		t.Errorf("RequireID(%s) = %v, want nil", goldenBlobID, err)
	}
}

func TestRequireIDRejectsMalformedIDs(t *testing.T) {
	bad := []string{
		"",
		"abc",
		strings.ToUpper(goldenBlobID),
		strings.Repeat("g", 64),
		goldenBlobID[:63],
		goldenBlobID + "0",
	}
	for _, id := range bad {
		if err := RequireID(id); err == nil {
			t.Errorf("RequireID(%q) = nil, want error", id)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex in test fixture: %v", err)
	}
	return b
}
