package search

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxTextBytes bounds how much of a blob's raw UTF-8 text is sniffed and
	// used, so one huge blob cannot blow up index-building memory.
	maxTextBytes = 1 << 20

	utf8PrintableRatio = 0.90
	pdfPrintableRatio  = 0.40

	pdfMagic = "%PDF-"

	pdftotextTimeout = 10 * time.Second
)

// Extract returns the extractable plain text for a blob's raw bytes, and
// true, or ("", false) if this package has no way to read text from it.
// Detection order: valid UTF-8 (at least 90% printable characters, capped at
// 1 MiB) is used as-is; a PDF (a "%PDF-" prefix) is extracted with
// pdftotext when it is on PATH, else with a minimal builtin extractor;
// anything else is skipped.
func Extract(data []byte) (string, bool) {
	if text, ok := extractUTF8Text(data); ok {
		return text, true
	}
	if bytes.HasPrefix(data, []byte(pdfMagic)) {
		return extractPDF(data)
	}
	return "", false
}

func extractUTF8Text(data []byte) (string, bool) {
	data = capUTF8(data, maxTextBytes)
	if len(data) == 0 || !utf8.Valid(data) {
		return "", false
	}
	if !meetsPrintableRatio(string(data), utf8PrintableRatio) {
		return "", false
	}
	return string(data), true
}

// capUTF8 truncates data to at most max bytes, then drops any trailing
// partial rune the cut may have left behind.
func capUTF8(data []byte, max int) []byte {
	if len(data) <= max {
		return data
	}
	data = data[:max]
	for len(data) > 0 {
		if r, size := utf8.DecodeLastRune(data); r != utf8.RuneError || size != 1 {
			break
		}
		data = data[:len(data)-1]
	}
	return data
}

// meetsPrintableRatio reports whether at least threshold of s's runes are
// printable. \n, \r, and \t count as printable even though unicode.IsPrint
// excludes them, since they are ordinary in extracted text. An empty string
// never meets the threshold.
func meetsPrintableRatio(s string, threshold float64) bool {
	total, printable := 0, 0
	for _, r := range s {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= threshold
}

// extractPDF extracts a best-effort plain-text rendering of a PDF's bytes,
// preferring pdftotext when it is on PATH and falling back to a minimal
// builtin extractor otherwise. The result is validated the same way
// regardless of which one ran: empty or under 40% printable counts as no
// extractable text, the honest outcome for a PDF that is scanned images or
// otherwise beyond this package's simple MVP extractors.
func extractPDF(data []byte) (string, bool) {
	text, ok := extractPDFWithPdftotext(data)
	if !ok {
		text = extractPDFBuiltin(data)
	}
	text = strings.TrimSpace(text)
	if text == "" || !meetsPrintableRatio(text, pdfPrintableRatio) {
		return "", false
	}
	return text, true
}

// extractPDFWithPdftotext runs the pdftotext binary against data, returning
// false if it is not on PATH or fails to run to completion within
// pdftotextTimeout.
func extractPDFWithPdftotext(data []byte) (string, bool) {
	path, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", false
	}

	tmp, err := os.CreateTemp("", "snapvault-search-*.pdf")
	if err != nil {
		return "", false
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", false
	}
	if err := tmp.Close(); err != nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdftotextTimeout)
	defer cancel()
	// "-" writes extracted text to stdout instead of a sibling .txt file.
	// Stdout is captured through a capped writer rather than Output(),
	// which buffers the child's entire output with no limit: a PDF crafted
	// to make pdftotext emit gigabytes would otherwise exhaust memory
	// before the maxTextBytes cap ever applied.
	var out cappedBuffer
	out.limit = maxTextBytes
	cmd := exec.CommandContext(ctx, path, tmpPath, "-")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return out.String(), true
}

// cappedBuffer is an io.Writer that keeps only the first limit bytes
// written to it, discarding (but still accepting, so the writer never
// blocks or errors) anything beyond that.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		c.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

// extractPDFBuiltin is the minimal fallback PDF extractor: it locates
// stream objects, inflates FlateDecode ones, and pulls the literal and hex
// strings out of each content stream's BT..ET text-showing regions. Each
// stream is individually capped at maxTextBytes by inflateStream, but a
// crafted PDF can still hold many streams, so a running total across all of
// them stops scanning once maxTextBytes has been produced in aggregate.
func extractPDFBuiltin(data []byte) string {
	var parts []string
	total := 0
	for _, s := range pdfStreams(data) {
		if total >= maxTextBytes {
			break
		}
		content := s.body
		if s.flate {
			decoded, err := inflateStream(content)
			if err != nil {
				continue
			}
			content = decoded
		}
		for _, part := range pdfContentText(content) {
			if remaining := maxTextBytes - total; remaining < len(part) {
				part = string(capUTF8([]byte(part), remaining))
			}
			parts = append(parts, part)
			total += len(part)
			if total >= maxTextBytes {
				break
			}
		}
	}
	// The per-part trimming above bounds the parts themselves, but
	// strings.Join's separators can still push the joined result a few
	// bytes past maxTextBytes; trim once more so the aggregate cap is exact.
	return string(capUTF8([]byte(strings.Join(parts, " ")), maxTextBytes))
}

// pdfStream is one PDF stream object's body, plus whether its dictionary
// named the FlateDecode filter.
type pdfStream struct {
	body  []byte
	flate bool
}

// pdfStreams scans data for every "stream" ... "endstream" pair. It is a
// minimal scanner, not a PDF parser: it does not track object boundaries,
// so a stream keyword appearing inside another stream's binary data could
// in principle confuse it. That is an acceptable MVP tradeoff.
func pdfStreams(data []byte) []pdfStream {
	const (
		streamKeyword    = "stream"
		endStreamKeyword = "endstream"
		dictLookback     = 4096
	)

	var streams []pdfStream
	rest := data
	for {
		idx := bytes.Index(rest, []byte(streamKeyword))
		if idx < 0 {
			break
		}
		dictStart := max(0, idx-dictLookback)
		flate := bytes.Contains(rest[dictStart:idx], []byte("FlateDecode"))

		bodyStart := idx + len(streamKeyword)
		// The stream keyword is followed by CRLF or a lone LF before the
		// binary body begins; skip exactly that.
		if bodyStart < len(rest) && rest[bodyStart] == '\r' {
			bodyStart++
		}
		if bodyStart < len(rest) && rest[bodyStart] == '\n' {
			bodyStart++
		}

		end := bytes.Index(rest[bodyStart:], []byte(endStreamKeyword))
		if end < 0 {
			break
		}
		streams = append(streams, pdfStream{body: rest[bodyStart : bodyStart+end], flate: flate})
		rest = rest[bodyStart+end+len(endStreamKeyword):]
	}
	return streams
}

// inflateStream decompresses a FlateDecode stream. PDF's FlateDecode is
// zlib-wrapped deflate; a handful of encoders emit raw deflate without the
// zlib header, so that is tried as a fallback.
func inflateStream(data []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		out, readErr := io.ReadAll(io.LimitReader(zr, maxTextBytes))
		zr.Close()
		if readErr == nil {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	defer fr.Close()
	return io.ReadAll(io.LimitReader(fr, maxTextBytes))
}

// pdfContentText extracts the literal and hex string operands found inside
// every BT..ET (text object) region of a decoded content stream.
func pdfContentText(content []byte) []string {
	var out []string
	rest := content
	for {
		start := bytes.Index(rest, []byte("BT"))
		if start < 0 {
			break
		}
		end := bytes.Index(rest[start:], []byte("ET"))
		if end < 0 {
			break
		}
		out = append(out, pdfStringOperands(rest[start:start+end])...)
		rest = rest[start+end+len("ET"):]
	}
	return out
}

// pdfStringOperands returns every literal "(...)" and hex "<...>" string
// found in block, in order of appearance.
func pdfStringOperands(block []byte) []string {
	var out []string
	i := 0
	for i < len(block) {
		switch {
		case block[i] == '(':
			text, consumed := decodeParenString(block[i:])
			if text != "" {
				out = append(out, text)
			}
			i += consumed
		case block[i] == '<' && i+1 < len(block) && block[i+1] == '<':
			// A dictionary "<<...>>", not a hex string; skip past it.
			i += 2
		case block[i] == '<':
			text, consumed := decodeHexString(block[i:])
			if text != "" {
				out = append(out, text)
			}
			i += consumed
		default:
			i++
		}
	}
	return out
}

// decodeParenString decodes a PDF literal string starting at s[0] == '(',
// honoring \n \r \t \( \) \\ and up-to-three-digit octal escapes, and
// balancing unescaped nested parentheses. It returns the decoded text and
// the number of bytes of s the string occupies, including both
// parentheses; on an unterminated string it consumes all of s.
func decodeParenString(s []byte) (string, int) {
	var out []byte
	depth := 1
	i := 1
	for i < len(s) {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			consumed := 2
			switch esc := s[i+1]; {
			case esc == 'n':
				out = append(out, '\n')
			case esc == 'r':
				out = append(out, '\r')
			case esc == 't':
				out = append(out, '\t')
			case esc == '(' || esc == ')' || esc == '\\':
				out = append(out, esc)
			case esc == '\n':
				// Backslash-newline is a line continuation: no character.
			case esc >= '0' && esc <= '7':
				val, j := 0, i+1
				for n := 0; n < 3 && j < len(s) && s[j] >= '0' && s[j] <= '7'; n++ {
					val = val*8 + int(s[j]-'0')
					j++
				}
				out = append(out, byte(val))
				consumed = j - i
			default:
				out = append(out, esc)
			}
			i += consumed
		case c == '(':
			depth++
			out = append(out, c)
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 {
				return string(out), i
			}
			// A nested, balanced ')' is literal string content, not the
			// terminator.
			out = append(out, c)
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out), i
}

// decodeHexString decodes a PDF hex string starting at s[0] == '<'. A
// trailing unpaired hex digit is padded with an implicit 0, per the PDF
// spec. It returns the decoded text and the number of bytes of s the
// string occupies, including both angle brackets; on an unterminated
// string it consumes all of s.
func decodeHexString(s []byte) (string, int) {
	end := bytes.IndexByte(s[1:], '>')
	if end < 0 {
		return "", len(s)
	}

	var digits []byte
	for _, c := range s[1 : 1+end] {
		if isHexDigit(c) {
			digits = append(digits, c)
		}
	}
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}
	decoded, err := hex.DecodeString(string(digits))
	if err != nil {
		return "", 1 + end + 1
	}
	return string(decoded), 1 + end + 1
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
