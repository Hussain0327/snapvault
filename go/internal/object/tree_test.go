package object

import (
	"bytes"
	"strings"
	"testing"
)

// Golden tree produced by the Java reference implementation: five entries whose
// sorted order pins UTF-16 code-unit comparison (U+10000 before U+FFFD).
const (
	goldenTreeHex = "5356543100000005000000055a2d6469720200bccaa8985496fdc553fa99487f038d3c1c5c" +
		"b5aebbe47d7d9f12bc758820106a00000005612e74787401010bd69098bd9b9cc5934a610ab65da429b5253611" +
		"47faa7b5b922919e9a23143d000000046c696e6b03000bd69098bd9b9cc5934a610ab65da429b525361147faa7" +
		"b5b922919e9a23143d00000004f090808001000bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922" +
		"919e9a23143d00000003efbfbd01000bd69098bd9b9cc5934a610ab65da429b525361147faa7b5b922919e9a23" +
		"143d"
	goldenTreeID = "79dc49597b4cc697ddad8ca36f36561ae631fa8f4fa20a391ad505022ad83420"
)

func goldenTreeEntries() []TreeEntry {
	// Deliberately unsorted input: NewTree must sort it into Java String order.
	return []TreeEntry{
		{Name: "�", Kind: KindFile, ObjectID: goldenBlobID},
		{Name: "\U00010000", Kind: KindFile, ObjectID: goldenBlobID},
		{Name: "Z-dir", Kind: KindDirectory, ObjectID: goldenEmptyTreeID},
		{Name: "a.txt", Kind: KindFile, ObjectID: goldenBlobID, Executable: true},
		{Name: "link", Kind: KindSymlink, ObjectID: goldenBlobID},
	}
}

func TestEmptyTreeMatchesJava(t *testing.T) {
	tree, err := NewTree(nil)
	if err != nil {
		t.Fatalf("NewTree(nil) = %v", err)
	}
	if got := tree.Encode(); !bytes.Equal(got, mustHex(t, "5356543100000000")) {
		t.Errorf("empty tree encodes to %x, want 5356543100000000", got)
	}
	if got := ID(TypeTree, tree.Encode()); got != goldenEmptyTreeID {
		t.Errorf("empty tree id = %s, want %s", got, goldenEmptyTreeID)
	}
}

func TestTreeEncodeMatchesJava(t *testing.T) {
	tree, err := NewTree(goldenTreeEntries())
	if err != nil {
		t.Fatalf("NewTree = %v", err)
	}
	if got := tree.Encode(); !bytes.Equal(got, mustHex(t, goldenTreeHex)) {
		t.Errorf("tree encodes to %x, want %s", got, goldenTreeHex)
	}
	if got := ID(TypeTree, tree.Encode()); got != goldenTreeID {
		t.Errorf("tree id = %s, want %s", got, goldenTreeID)
	}
}

func TestTreeSortsInUTF16CodeUnitOrder(t *testing.T) {
	tree, err := NewTree(goldenTreeEntries())
	if err != nil {
		t.Fatalf("NewTree = %v", err)
	}
	want := []string{"Z-dir", "a.txt", "link", "\U00010000", "�"}
	entries := tree.Entries()
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, name := range want {
		if entries[i].Name != name {
			t.Errorf("entries[%d].Name = %q, want %q", i, entries[i].Name, name)
		}
	}
}

func TestCompareNamesDisagreesWithByteOrderAboveBMP(t *testing.T) {
	if CompareNames("\U00010000", "�") >= 0 {
		t.Error(`CompareNames("\U00010000", "�") >= 0, want < 0 (UTF-16 order)`)
	}
	if strings.Compare("\U00010000", "�") <= 0 {
		t.Fatal("fixture no longer distinguishes UTF-16 from byte order")
	}
	if CompareNames("a", "b") >= 0 || CompareNames("b", "a") <= 0 || CompareNames("a", "a") != 0 {
		t.Error("CompareNames disagrees with byte order on ASCII")
	}
}

func TestDecodeTreeRoundTripsGolden(t *testing.T) {
	tree, err := DecodeTree(mustHex(t, goldenTreeHex))
	if err != nil {
		t.Fatalf("DecodeTree = %v", err)
	}
	if got := tree.Encode(); !bytes.Equal(got, mustHex(t, goldenTreeHex)) {
		t.Errorf("re-encode = %x, want %s", got, goldenTreeHex)
	}
	if !tree.Entries()[1].Executable {
		t.Error("a.txt lost its executable bit in the round trip")
	}
}

func TestNewTreeRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		desc  string
		entry TreeEntry
	}{
		{"empty name", TreeEntry{Name: "", Kind: KindFile, ObjectID: goldenBlobID}},
		{"dot", TreeEntry{Name: ".", Kind: KindFile, ObjectID: goldenBlobID}},
		{"dot-dot", TreeEntry{Name: "..", Kind: KindFile, ObjectID: goldenBlobID}},
		{"slash", TreeEntry{Name: "a/b", Kind: KindFile, ObjectID: goldenBlobID}},
		{"backslash", TreeEntry{Name: `a\b`, Kind: KindFile, ObjectID: goldenBlobID}},
		{"nul", TreeEntry{Name: "a\x00b", Kind: KindFile, ObjectID: goldenBlobID}},
		{"executable dir", TreeEntry{
			Name: "d", Kind: KindDirectory, ObjectID: goldenEmptyTreeID, Executable: true}},
		{"executable symlink", TreeEntry{
			Name: "l", Kind: KindSymlink, ObjectID: goldenBlobID, Executable: true}},
		{"bad id", TreeEntry{Name: "f", Kind: KindFile, ObjectID: "abc"}},
	}
	for _, tc := range cases {
		if _, err := NewTree([]TreeEntry{tc.entry}); err == nil {
			t.Errorf("NewTree accepted %s", tc.desc)
		}
	}
}

func TestNewTreeRejectsDuplicateNames(t *testing.T) {
	entries := []TreeEntry{
		{Name: "same", Kind: KindFile, ObjectID: goldenBlobID},
		{Name: "same", Kind: KindSymlink, ObjectID: goldenBlobID},
	}
	if _, err := NewTree(entries); err == nil {
		t.Error("NewTree accepted duplicate names")
	}
}

func TestDecodeTreeRejectsCorruptPayloads(t *testing.T) {
	golden := mustHex(t, goldenTreeHex)
	truncated := golden[:len(golden)-1]
	trailing := append(append([]byte(nil), golden...), 0)
	badMagic := append([]byte(nil), golden...)
	badMagic[0] = 'X'
	hugeCount := append([]byte(nil), golden[:4]...)
	hugeCount = append(hugeCount, 0x7f, 0xff, 0xff, 0xff)

	cases := []struct {
		desc    string
		payload []byte
	}{
		{"empty", nil},
		{"bad magic", badMagic},
		{"truncated", truncated},
		{"trailing data", trailing},
		{"implausible count", hugeCount},
	}
	for _, tc := range cases {
		if _, err := DecodeTree(tc.payload); err == nil {
			t.Errorf("DecodeTree accepted %s", tc.desc)
		}
	}
}
