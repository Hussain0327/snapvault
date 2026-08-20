package search

import (
	"bytes"
	"compress/zlib"
	"os"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", name, err)
	}
	return data
}

func TestExtractPlainUTF8Text(t *testing.T) {
	const want = "Payment systems process transactions between banks and café merchants.\n"
	text, ok := Extract([]byte(want))
	if !ok {
		t.Fatal("Extract(plain UTF-8 text) = false, want true")
	}
	if text != want {
		t.Errorf("Extract text = %q, want %q", text, want)
	}
}

func TestExtractRejectsInvalidUTF8(t *testing.T) {
	data := []byte{0xff, 0xfe, 0x00, 0x01, 0x02, 0x03}
	if _, ok := Extract(data); ok {
		t.Error("Extract(invalid UTF-8) = true, want false")
	}
}

func TestExtractRejectsLowPrintability(t *testing.T) {
	// Control bytes (0x01), well under the 90% printable threshold, but
	// still technically valid UTF-8 (all single-byte code points).
	data := make([]byte, 100)
	for i := range data {
		data[i] = 0x01
	}
	if _, ok := Extract(data); ok {
		t.Error("Extract(mostly control bytes) = true, want false")
	}
}

func TestExtractCapsUTF8AtOneMiB(t *testing.T) {
	// A multi-byte rune repeated enough to push well past the 1 MiB cap and
	// to make it unlikely the cap lands on a clean rune boundary.
	data := []byte(strings.Repeat("é", 500_000)) // 2 bytes/rune => 1,000,000 bytes
	data = append(data, []byte(strings.Repeat("é", 200_000))...)

	text, ok := Extract(data)
	if !ok {
		t.Fatal("Extract(long UTF-8 text) = false, want true")
	}
	if len(text) > 1<<20 {
		t.Errorf("Extract text is %d bytes, want at most 1 MiB", len(text))
	}
	if !strings.HasPrefix(string(data), text) {
		t.Error("Extract text is not a prefix of the source text")
	}
}

func TestExtractSkipsUnrecognizedBinary(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if _, ok := Extract(data); ok {
		t.Error("Extract(binary, non-PDF data) = true, want false")
	}
}

func TestExtractSkipsGarbagePDF(t *testing.T) {
	data := append([]byte("%PDF-1.4\n"), []byte(strings.Repeat("\x00\x01\x02\x03", 50))...)
	if _, ok := Extract(data); ok {
		t.Error("Extract(garbage PDF) = true, want false")
	}
}

func TestExtractPDFEndToEnd(t *testing.T) {
	data := readTestdata(t, "payment-uncompressed.pdf")
	text, ok := Extract(data)
	if !ok {
		t.Fatal("Extract(payment-uncompressed.pdf) = false, want true")
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "payment systems") {
		t.Errorf("Extract text = %q, want it to mention %q", text, "payment systems")
	}
}

func TestExtractFlatePDFEndToEnd(t *testing.T) {
	data := readTestdata(t, "weather-flate.pdf")
	text, ok := Extract(data)
	if !ok {
		t.Fatal("Extract(weather-flate.pdf) = false, want true")
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "weather forecasting") {
		t.Errorf("Extract text = %q, want it to mention %q", text, "weather forecasting")
	}
}

// TestExtractPDFBuiltinUncompressed exercises the builtin extractor
// directly (bypassing pdftotext, even when it is on PATH) so the fallback
// path is deterministically tested in every environment.
func TestExtractPDFBuiltinUncompressed(t *testing.T) {
	data := readTestdata(t, "payment-uncompressed.pdf")
	text := extractPDFBuiltin(data)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "payment systems process transactions") {
		t.Errorf("extractPDFBuiltin text = %q, want it to mention the payment sentence", text)
	}
	if !strings.Contains(lower, "fraud detection") {
		t.Errorf("extractPDFBuiltin text = %q, want it to mention the fraud sentence", text)
	}
}

func TestExtractPDFBuiltinFlateDecode(t *testing.T) {
	data := readTestdata(t, "weather-flate.pdf")
	text := extractPDFBuiltin(data)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "weather forecasting models predict rainfall") {
		t.Errorf("extractPDFBuiltin text = %q, want it to mention the weather sentence", text)
	}
	if !strings.Contains(lower, "meteorologists") {
		t.Errorf("extractPDFBuiltin text = %q, want it to mention meteorologists", text)
	}
}

// TestExtractPDFBuiltinCapsAggregateOutput builds a PDF with many
// FlateDecode streams that each individually stay under inflateStream's
// per-stream cap but together expand far past maxTextBytes, and asserts the
// builtin extractor's total output is still bounded: a per-stream cap alone
// lets total memory use grow linearly with a crafted PDF's stream count.
func TestExtractPDFBuiltinCapsAggregateOutput(t *testing.T) {
	const (
		streamCount       = 20
		perStreamInflated = 200_000 // 200 KB each, 4 MB total if uncapped.
	)
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	for i := 0; i < streamCount; i++ {
		var deflated bytes.Buffer
		zw := zlib.NewWriter(&deflated)
		zw.Write([]byte("BT ("))
		zw.Write(bytes.Repeat([]byte{'A'}, perStreamInflated))
		zw.Write([]byte(") Tj ET"))
		if err := zw.Close(); err != nil {
			t.Fatalf("zlib.Close = %v", err)
		}
		buf.WriteString("<< /Filter /FlateDecode >>\nstream\n")
		buf.Write(deflated.Bytes())
		buf.WriteString("\nendstream\n")
	}

	text := extractPDFBuiltin(buf.Bytes())
	if len(text) > maxTextBytes {
		t.Errorf("extractPDFBuiltin produced %d bytes across %d streams, want at most maxTextBytes (%d)",
			len(text), streamCount, maxTextBytes)
	}
}

func TestDecodeParenStringEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`(hello world)`, "hello world"},
		{`(escaped \( paren and \) paren)`, "escaped ( paren and ) paren"},
		{`(line\nbreak)`, "line\nbreak"},
		{`(octal\040space)`, "octal space"},
		{`(nested (parens) are balanced)`, "nested (parens) are balanced"},
	}
	for _, c := range cases {
		got, consumed := decodeParenString([]byte(c.in))
		if got != c.want {
			t.Errorf("decodeParenString(%q) text = %q, want %q", c.in, got, c.want)
		}
		if consumed != len(c.in) {
			t.Errorf("decodeParenString(%q) consumed = %d, want %d", c.in, consumed, len(c.in))
		}
	}
}

func TestDecodeHexStringString(t *testing.T) {
	got, consumed := decodeHexString([]byte("<48656C6C6F>"))
	if got != "Hello" {
		t.Errorf("decodeHexString(<48656C6C6F>) = %q, want %q", got, "Hello")
	}
	if want := len("<48656C6C6F>"); consumed != want {
		t.Errorf("decodeHexString consumed = %d, want %d", consumed, want)
	}
}
