package delta

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenDir holds the shared v2 delta fixtures consumed by every language's
// test suite. See tests/golden/v2/delta/MANIFEST.md for what each covers and
// how to regenerate them.
const goldenDir = "../../../tests/golden/v2/delta"

// goldenCases lists every NN-name fixture stem expected in goldenDir. Each
// stem has a matching .base, .delta, and .target file.
var goldenCases = []string{
	"01-worked-example",
	"02-multi-byte-varint",
	"03-copy-65536",
	"04-insert-chain",
	"05-binary-content",
	"06-mixed-edits",
}

// rejectDir holds the shared v2 delta *negative* fixtures: malformed streams
// every language's decoder must refuse. Kept in its own subdirectory of
// goldenDir so a reject case can never be mistaken for an accept case. See
// tests/golden/v2/delta/MANIFEST.md.
const rejectDir = goldenDir + "/reject"

// rejectCases maps each reject fixture stem (a matching .base and .delta,
// but deliberately no .target) to a substring Apply's error must contain, so
// this test pins *why* the stream is rejected and not just that it is.
var rejectCases = map[string]string{
	"01-copy-past-end":           "out of bounds",
	"02-truncated-instruction":   "truncated insert literal",
	"03-truncated-varint-header": "truncated varint",
	"04-reserved-opcode-zero":    "opcode 0x00 is invalid",
	"05-src-size-mismatch":       "does not match base length",
	"06-tgt-size-mismatch":       "want tgtSize",
}

func TestGoldenVectorsApplyToTarget(t *testing.T) {
	for _, name := range goldenCases {
		t.Run(name, func(t *testing.T) {
			base := readGolden(t, goldenDir, name, "base")
			delta := readGolden(t, goldenDir, name, "delta")
			target := readGolden(t, goldenDir, name, "target")

			got, err := Apply(base, delta)
			if err != nil {
				t.Fatalf("Apply(%s.base, %s.delta) = %v, want nil error", name, name, err)
			}
			if !bytes.Equal(got, target) {
				t.Errorf("Apply(%s.base, %s.delta) did not reproduce %s.target", name, name, name)
			}
		})
	}
}

// TestGoldenVectorsRejectMalformed feeds every reject/ fixture to Apply and
// requires an error naming the specific defect, so a decoder that happens to
// fail a malformed stream for the wrong reason (or a future refactor that
// silently starts accepting it) both get caught.
func TestGoldenVectorsRejectMalformed(t *testing.T) {
	for name, wantSubstring := range rejectCases {
		t.Run(name, func(t *testing.T) {
			base := readGolden(t, rejectDir, name, "base")
			delta := readGolden(t, rejectDir, name, "delta")

			got, err := Apply(base, delta)
			if err == nil {
				t.Fatalf("Apply(reject/%s.base, reject/%s.delta) = %q, nil, want an error", name, name, got)
			}
			if !strings.Contains(err.Error(), wantSubstring) {
				t.Errorf("Apply(reject/%s.base, reject/%s.delta) error = %q, want it to contain %q",
					name, name, err.Error(), wantSubstring)
			}
		})
	}
}

func readGolden(t *testing.T, dir, name, ext string) []byte {
	t.Helper()
	path := filepath.Join(dir, name+"."+ext)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden fixture %s: %v", path, err)
	}
	return data
}
