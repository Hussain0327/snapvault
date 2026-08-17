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

func TestUsageAndErrors(t *testing.T) {
	blank := run(t, t.TempDir())
	if blank.code != 0 || !strings.Contains(blank.out, "Usage:") {
		t.Errorf("no arguments = %+v", blank)
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
