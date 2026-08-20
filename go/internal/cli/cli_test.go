package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type result struct {
	code int
	out  string
	err  string
}

func run(t *testing.T, workdir string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut, workdir)
	return result{code: code, out: out.String(), err: errOut.String()}
}

func initialized(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if got := run(t, t.TempDir(), "init", dir); got.code != 0 {
		t.Fatalf("init failed: %+v", got)
	}
	return dir
}

func write(t *testing.T, dir string, rel string, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}

func TestInitReportsMetadataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	got := run(t, t.TempDir(), "init", dir)
	if got.code != 0 {
		t.Fatalf("init = %+v", got)
	}
	if !strings.HasPrefix(got.out, "Initialized empty SnapVault repository in ") ||
		!strings.Contains(got.out, ".snapvault") {
		t.Errorf("init output = %q", got.out)
	}
}

func TestSnapshotPrintsAbbreviatedIDAndMessage(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "content")
	got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "  spaced message  ")
	if got.code != 0 {
		t.Fatalf("snapshot = %+v", got)
	}
	if !regexp.MustCompile(`^Snapshot [0-9a-f]{12} spaced message\n$`).MatchString(got.out) {
		t.Errorf("snapshot output = %q", got.out)
	}
}

func TestSnapshotAcceptsWorkersFlag(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "content")
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "m", "--workers", "2"); got.code != 0 {
		t.Fatalf("snapshot --workers 2 = %+v", got)
	}
	write(t, dir, "g.txt", "more")
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "m", "--workers=1"); got.code != 0 {
		t.Fatalf("snapshot --workers=1 = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "--workers", "0"); got.code != 2 {
		t.Errorf("snapshot --workers 0 = %+v, want usage error", got)
	}
}

func TestLogFormats(t *testing.T) {
	dir := initialized(t)
	if got := run(t, t.TempDir(), "-C", dir, "log"); got.code != 0 ||
		got.out != "No snapshots yet.\n" {
		t.Errorf("log before snapshots = %+v", got)
	}

	write(t, dir, "f.txt", "one")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "first line\nsecond line")

	oneline := run(t, t.TempDir(), "-C", dir, "log", "--oneline")
	if oneline.code != 0 ||
		!regexp.MustCompile(`^[0-9a-f]{12} first line\n$`).MatchString(oneline.out) {
		t.Errorf("log --oneline = %+v", oneline)
	}

	full := run(t, t.TempDir(), "-C", dir, "log")
	if full.code != 0 {
		t.Fatalf("log = %+v", full)
	}
	datePattern := `Date:   \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2}(:\d{2})?)\n`
	wantPattern := `^commit [0-9a-f]{64}\n` + datePattern + `\n    first line\n    second line\n\n$`
	if !regexp.MustCompile(wantPattern).MatchString(full.out) {
		t.Errorf("log output = %q", full.out)
	}

	write(t, dir, "f.txt", "two")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "second")
	withParent := run(t, t.TempDir(), "-C", dir, "log", "--limit", "1")
	if withParent.code != 0 || !strings.Contains(withParent.out, "Parent: ") {
		t.Errorf("log --limit 1 after two snapshots = %+v", withParent)
	}
	if strings.Count(withParent.out, "commit ") != 1 {
		t.Errorf("log --limit 1 printed more than one commit: %q", withParent.out)
	}
}

func TestDiffOutput(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "one")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "base")

	clean := run(t, t.TempDir(), "-C", dir, "diff")
	if clean.code != 0 || clean.out != "No changes.\n" {
		t.Errorf("clean diff = %+v", clean)
	}

	write(t, dir, "f.txt", "changed")
	if err := os.Mkdir(filepath.Join(dir, "hollow"), 0o755); err != nil {
		t.Fatalf("Mkdir = %v", err)
	}
	dirty := run(t, t.TempDir(), "-C", dir, "diff")
	if dirty.code != 0 {
		t.Fatalf("dirty diff = %+v", dirty)
	}
	if dirty.out != "M\tf.txt\nA\thollow/\n2 changes\n" {
		t.Errorf("dirty diff output = %q", dirty.out)
	}

	write(t, dir, "g.txt", "third")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "second")
	between := run(t, t.TempDir(), "-C", dir, "diff", "HEAD~1", "HEAD")
	if between.code != 0 || !strings.HasSuffix(between.out, "3 changes\n") {
		t.Errorf("diff HEAD~1 HEAD = %+v", between)
	}

	single := run(t, t.TempDir(), "-C", dir, "diff", "HEAD")
	if single.code != 0 || single.out != "No changes.\n" {
		t.Errorf("diff HEAD = %+v", single)
	}
	if got := run(t, t.TempDir(), "-C", dir, "diff", "--stat"); got.code != 2 {
		t.Errorf("diff --stat = %+v, want usage error", got)
	}
}

func TestSingularChangeCount(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "one")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "base")
	write(t, dir, "f.txt", "two")
	got := run(t, t.TempDir(), "-C", dir, "diff")
	if !strings.HasSuffix(got.out, "1 change\n") {
		t.Errorf("diff output = %q, want singular count", got.out)
	}
}

func TestRestoreCommandRevertsAndReports(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "original")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "first")
	write(t, dir, "f.txt", "changed")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "second")

	got := run(t, t.TempDir(), "-C", dir, "restore", "HEAD~1")
	if got.code != 0 {
		t.Fatalf("restore = %+v", got)
	}
	if !regexp.MustCompile(`^Restored [0-9a-f]{12} to `).MatchString(got.out) {
		t.Errorf("restore output = %q", got.out)
	}
	content, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil || string(content) != "original" {
		t.Errorf("restored content = %q, %v", content, err)
	}

	out := filepath.Join(t.TempDir(), "export")
	if got := run(t, t.TempDir(), "-C", dir, "restore", "HEAD", "--to", out); got.code != 0 {
		t.Errorf("restore --to = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(out, "f.txt")); err != nil {
		t.Errorf("exported file missing: %v", err)
	}
}

func TestUpgradeCommand(t *testing.T) {
	dir := initialized(t)
	format, err := os.ReadFile(filepath.Join(dir, ".snapvault", "format"))
	if err != nil || string(format) != "snapvault 1\n" {
		t.Fatalf("format before upgrade = %q, %v", format, err)
	}

	got := run(t, t.TempDir(), "-C", dir, "upgrade")
	if got.code != 0 {
		t.Fatalf("upgrade = %+v", got)
	}
	format, err = os.ReadFile(filepath.Join(dir, ".snapvault", "format"))
	if err != nil || string(format) != "snapvault 2\n" {
		t.Errorf("format after upgrade = %q, %v", format, err)
	}

	again := run(t, t.TempDir(), "-C", dir, "upgrade")
	if again.code != 0 {
		t.Fatalf("second upgrade = %+v", again)
	}
	if !strings.Contains(again.out, "repository is already format 2") {
		t.Errorf("second upgrade output = %q, want the already-format-2 message", again.out)
	}

	if got := run(t, t.TempDir(), "-C", dir, "upgrade", "extra"); got.code != 2 {
		t.Errorf("upgrade with an argument = %+v, want a usage error", got)
	}
}

func TestRepackCommand(t *testing.T) {
	dir := initialized(t)

	if got := run(t, t.TempDir(), "-C", dir, "repack"); got.code != 1 ||
		!strings.Contains(got.err, "snapvault upgrade") {
		t.Fatalf("repack on a format 1 repository = %+v, want an 'upgrade' hint", got)
	}

	if got := run(t, t.TempDir(), "-C", dir, "upgrade"); got.code != 0 {
		t.Fatalf("upgrade = %+v", got)
	}

	content := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 2000)
	for v := 0; v < 5; v++ {
		write(t, dir, "big.txt", content+strings.Repeat("x", v))
		if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "v"); got.code != 0 {
			t.Fatalf("snapshot %d = %+v", v, got)
		}
	}

	dry := run(t, t.TempDir(), "-C", dir, "repack", "--dry-run")
	if dry.code != 0 {
		t.Fatalf("repack --dry-run = %+v", dry)
	}
	if !strings.HasPrefix(dry.out, "would repack ") || !strings.Contains(dry.out, "% smaller)") {
		t.Errorf("repack --dry-run output = %q", dry.out)
	}

	got := run(t, t.TempDir(), "-C", dir, "repack")
	if got.code != 0 {
		t.Fatalf("repack = %+v", got)
	}
	if !strings.HasPrefix(got.out, "repacked ") || !strings.Contains(got.out, " objects: ") ||
		!strings.Contains(got.out, " -> ") || !strings.Contains(got.out, "% smaller)") {
		t.Errorf("repack output = %q", got.out)
	}

	again := run(t, t.TempDir(), "-C", dir, "repack")
	if again.code != 0 || again.out != "nothing to repack.\n" {
		t.Errorf("second repack = %+v, want the nothing-to-repack message", again)
	}

	if got := run(t, t.TempDir(), "-C", dir, "repack", "--bogus"); got.code != 2 {
		t.Errorf("repack --bogus = %+v, want a usage error", got)
	}
	if got := run(t, t.TempDir(), "repack"); got.code != 1 {
		t.Errorf("repack outside a repository = %+v, want exit 1", got)
	}
}

// fixturePDF is a minimal, hand-built PDF (uncompressed content stream, two
// Tj-shown literal strings) whose text both pdftotext and the builtin
// fallback extractor can read: "Quantum resonance crystals amplify obsidian
// lattice vibrations for laboratory experiments." and "Laboratory
// technicians calibrate resonance crystals before each trial."
const fixturePDF = "%PDF-1.4\n" +
	"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >>" +
	" /MediaBox [0 0 612 792] /Contents 5 0 R >>\nendobj\n" +
	"4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n" +
	"5 0 obj\n<< /Length 207 >>\nstream\n" +
	"BT\n/F1 12 Tf\n72 712 Td\n" +
	"(Quantum resonance crystals amplify obsidian lattice vibrations for laboratory experiments.) Tj\n" +
	"0 -14 Td\n(Laboratory technicians calibrate resonance crystals before each trial.) Tj\nET\n\n" +
	"endstream\nendobj\ntrailer\n<< /Size 6 /Root 1 0 R >>\n%%EOF\n"

func TestFindWithoutIndexErrors(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "content")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "first")

	got := run(t, t.TempDir(), "-C", dir, "find", "content")
	if got.code != 1 {
		t.Fatalf("find without an index = %+v, want exit 1", got)
	}
	if !strings.Contains(got.err, "no search index; run 'snapvault index' first") {
		t.Errorf("find without an index err = %q", got.err)
	}
}

func TestIndexAndFindEndToEnd(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "docs/report.txt",
		"The obsidian lantern illuminates ancient sapphire caverns beneath the mountain.")
	write(t, dir, "docs/weather.txt",
		"Quiet valleys hold ancient quartz stones through the seasons.")
	if err := os.WriteFile(filepath.Join(dir, "docs", "manual.pdf"), []byte(fixturePDF), 0o644); err != nil {
		t.Fatalf("WriteFile(pdf) = %v", err)
	}
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "add initial docs"); got.code != 0 {
		t.Fatalf("snapshot 1 = %+v", got)
	}

	// The rewrite shares a little vocabulary with the original ("obsidian
	// lantern") but replaces the rest, so a query drawn from only the new
	// wording cannot also match the original version's blob, which remains
	// separately indexed as part of history.
	write(t, dir, "docs/report.txt",
		"The obsidian lantern now illuminates freshly restored granite corridors beneath the citadel.")
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "rewrite the lantern report"); got.code != 0 {
		t.Fatalf("snapshot 2 = %+v", got)
	}

	idxOut := run(t, t.TempDir(), "-C", dir, "index")
	if idxOut.code != 0 {
		t.Fatalf("index = %+v", idxOut)
	}
	if !regexp.MustCompile(`^indexed \d+ blobs \(\d+ chunks\) with builtin-lexical-v1\n$`).MatchString(idxOut.out) {
		t.Errorf("index output = %q", idxOut.out)
	}

	find := run(t, t.TempDir(), "-C", dir, "find", "freshly restored granite corridors citadel")
	if find.code != 0 {
		t.Fatalf("find = %+v", find)
	}
	lines := strings.Split(strings.TrimRight(find.out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("find output has too few lines: %q", find.out)
	}
	wantFirstLine := regexp.MustCompile(
		`^[0-9a-f]{12}  docs/report\.txt  \(snapshot: "rewrite the lantern report", [0-9a-f]{12}\)$`)
	if !wantFirstLine.MatchString(lines[0]) {
		t.Errorf("find first line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "    ") {
		t.Errorf("find snippet line = %q, want a 4-space indent", lines[1])
	}

	findPDF := run(t, t.TempDir(), "-C", dir, "find", "quantum resonance crystals laboratory", "--limit", "1")
	if findPDF.code != 0 {
		t.Fatalf("find (pdf query) = %+v", findPDF)
	}
	if !strings.Contains(findPDF.out, "docs/manual.pdf") {
		t.Errorf("find (pdf query) output = %q, want it to mention the pdf", findPDF.out)
	}
}

func TestIndexAcceptsEmbedderFlagAndRejectsUnknown(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "content")
	run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "first")

	if got := run(t, t.TempDir(), "-C", dir, "index", "--embedder", "builtin"); got.code != 0 {
		t.Fatalf("index --embedder builtin = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "index", "--embedder=builtin"); got.code != 0 {
		t.Fatalf("index --embedder=builtin = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "index", "--embedder", "nonsense"); got.code != 2 {
		t.Errorf("index --embedder nonsense = %+v, want a usage error", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "find"); got.code != 2 {
		t.Errorf("find with no query = %+v, want a usage error", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "find", "q", "--limit", "0"); got.code != 2 {
		t.Errorf("find --limit 0 = %+v, want a usage error", got)
	}
}

func TestUsageAndErrors(t *testing.T) {
	blank := run(t, t.TempDir())
	if blank.code != 0 || !strings.Contains(blank.out, "Usage:") {
		t.Errorf("no arguments = %+v", blank)
	}
	if !strings.Contains(blank.out, "index [--embedder") || !strings.Contains(blank.out, "find <query>") {
		t.Errorf("usage text is missing index/find: %q", blank.out)
	}
	if got := run(t, t.TempDir(), "help"); got.code != 0 || !strings.Contains(got.out, "Usage:") {
		t.Errorf("help = %+v", got)
	}
	if got := run(t, t.TempDir(), "version"); got.code != 0 || got.out != "SnapVault 1.0.0\n" {
		t.Errorf("version = %+v", got)
	}

	unknown := run(t, t.TempDir(), "frobnicate")
	if unknown.code != 2 || !strings.Contains(unknown.err, "unknown command: frobnicate") ||
		!strings.Contains(unknown.err, "Run 'snapvault help' for usage.") {
		t.Errorf("unknown command = %+v", unknown)
	}
	if got := run(t, t.TempDir(), "-C"); got.code != 2 {
		t.Errorf("bare -C = %+v, want usage error", got)
	}
	if got := run(t, t.TempDir(), "log"); got.code != 1 ||
		!strings.Contains(got.err, "error: ") {
		t.Errorf("log outside repository = %+v", got)
	}
	dir := initialized(t)
	if got := run(t, t.TempDir(), "-C", dir, "log", "--limit", "zero"); got.code != 2 ||
		!strings.Contains(got.err, "log limit must be a number") {
		t.Errorf("bad limit = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "restore"); got.code != 2 ||
		!strings.Contains(got.err, "restore requires a snapshot revision") {
		t.Errorf("restore without revision = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "   "); got.code != 1 {
		t.Errorf("blank message = %+v, want exit 1", got)
	}
}

func TestCommitIsSnapshotAlias(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "f.txt", "x")
	if got := run(t, t.TempDir(), "-C", dir, "commit", "-m", "aliased"); got.code != 0 ||
		!strings.Contains(got.out, "aliased") {
		t.Errorf("commit alias = %+v", got)
	}
}

func TestFormatJavaTimestampMatchesISOOffsetDateTime(t *testing.T) {
	utc := time.FixedZone("UTC", 0)
	est := time.FixedZone("EST", -5*3600)
	halfHour := time.FixedZone("IST", 5*3600+1800)

	// Golden strings measured from DateTimeFormatter.ISO_OFFSET_DATE_TIME
	// (GoldenDates.java): seconds always print, and the fraction keeps the
	// minimal digits needed, so .100 renders as .1.
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 8, 17, 10, 15, 30, 0, utc), "2026-08-17T10:15:30Z"},
		{time.Date(2026, 8, 17, 10, 15, 0, 0, utc), "2026-08-17T10:15:00Z"},
		{time.Date(2026, 8, 17, 10, 15, 0, 500_000_000, utc), "2026-08-17T10:15:00.5Z"},
		{time.Date(2026, 8, 17, 10, 15, 30, 123_000_000, est), "2026-08-17T10:15:30.123-05:00"},
		{time.Date(2026, 8, 17, 10, 15, 30, 123_456_000, halfHour),
			"2026-08-17T10:15:30.123456+05:30"},
		{time.Date(2026, 8, 17, 10, 15, 30, 123_456_789, utc), "2026-08-17T10:15:30.123456789Z"},
		{time.Date(2026, 8, 17, 23, 59, 59, 1_000_000, utc), "2026-08-17T23:59:59.001Z"},
		{time.Date(2026, 8, 17, 10, 15, 30, 100_000_000, utc), "2026-08-17T10:15:30.1Z"},
	}
	for _, tc := range cases {
		if got := formatJavaTimestamp(tc.at); got != tc.want {
			t.Errorf("formatJavaTimestamp(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestPrintablePathEscapesControlCharacters(t *testing.T) {
	if got := printablePath("a\tb\\c\nd\re"); got != `a\tb\\c\nd\re` {
		t.Errorf("printablePath = %q", got)
	}
}

func TestPrintableSnippetEscapesControlBytesButKeepsUTF8(t *testing.T) {
	input := "safe \x1b[31mred\x07 line\tend\\slash café 日本語"
	want := `safe \x1b[31mred\x07 line\tend\\slash café 日本語`
	if got := printableSnippet(input); got != want {
		t.Errorf("printableSnippet(%q) = %q, want %q", input, got, want)
	}
}

// TestFindEscapesControlBytesInSnippet proves that a blob whose extracted
// text carries an ANSI escape sequence cannot use 'find' to write that
// sequence to the terminal: enough of the surrounding text is ordinary
// prose to pass the extractor's printable-ratio check, so the escape bytes
// reach the snippet, and the snippet must come out escaped rather than raw.
func TestFindEscapesControlBytesInSnippet(t *testing.T) {
	dir := initialized(t)
	write(t, dir, "docs/log.txt",
		"Sensor telemetry from the aurora vault control room reads: "+
			"\x1b[31mALERT\x07 unauthorized access attempt\x1b[0m detected near the reactor.")
	if got := run(t, t.TempDir(), "-C", dir, "snapshot", "-m", "add sensor log"); got.code != 0 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got := run(t, t.TempDir(), "-C", dir, "index"); got.code != 0 {
		t.Fatalf("index = %+v", got)
	}

	find := run(t, t.TempDir(), "-C", dir,
		"find", "aurora vault control room unauthorized access reactor")
	if find.code != 0 {
		t.Fatalf("find = %+v", find)
	}
	if strings.ContainsAny(find.out, "\x1b\x07") {
		t.Fatalf("find output leaked raw control bytes: %q", find.out)
	}
	if !strings.Contains(find.out, `\x1b[31mALERT\x07`) {
		t.Errorf("find output = %q, want the ANSI escape sequence escaped", find.out)
	}
}
