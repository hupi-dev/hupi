package ingest

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestExtractDOCXWalksParagraphs(t *testing.T) {
	data := buildMinimalDocx("First paragraph.", "Second paragraph.")
	res, err := extractDOCX(data)
	if err != nil {
		t.Fatalf("extractDOCX: %v", err)
	}
	if !strings.Contains(res.Text, "First paragraph.") || !strings.Contains(res.Text, "Second paragraph.") {
		t.Errorf("extractDOCX().Text = %q, want both paragraphs present", res.Text)
	}
	firstIdx := strings.Index(res.Text, "First paragraph.")
	secondIdx := strings.Index(res.Text, "Second paragraph.")
	if firstIdx < 0 || secondIdx < 0 || firstIdx > secondIdx {
		t.Errorf("extractDOCX().Text = %q, want paragraphs in original order", res.Text)
	}
}

func TestSniffFormatDetectsDOCXNotPlainZip(t *testing.T) {
	if got := sniffFormat(buildMinimalDocx("x")); got != formatDOCX {
		t.Errorf("sniffFormat() = %v, want formatDOCX", got)
	}
}

// TestExtractDOCXRejectsZipBomb is the zip-bomb defense regression test:
// a single highly-repetitive text part compresses to a tiny zip file
// but decompresses to far more than maxDocxEntryBytes. The defense reads
// through io.LimitReader against actual decompressed bytes, not the zip
// header's own declared (and untrustworthy) size, so this must fail
// with a clear error rather than allocating unbounded memory.
func TestExtractDOCXRejectsZipBomb(t *testing.T) {
	data := buildZipBombDocx(maxDocxEntryBytes + 1024)
	if len(data) > 1<<20 {
		t.Fatalf("test setup: zip bomb fixture itself is %d bytes, want it to stay small (that's the whole point of a zip bomb)", len(data))
	}
	_, err := extractDOCX(data)
	if err == nil {
		t.Error("extractDOCX() on an oversized decompressed entry = nil error, want a rejection")
	}
}

// TestExtractDOCXRejectsMissingBody confirms extractDOCX refuses
// cleanly (not silently returning empty text) if ever called directly
// against a zip with no word/document.xml entry — sniffFormat already
// guards against this in the normal dispatch path (isDocx checks for
// exactly this entry), but extractDOCX itself should be defensive too.
func TestExtractDOCXRejectsMissingBody(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("not-word-document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<root/>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if isDocx(buf.Bytes()) {
		t.Fatalf("test setup: expected this fixture NOT to be sniffed as docx (no word/document.xml)")
	}
	if _, err := extractDOCX(buf.Bytes()); err == nil {
		t.Error("extractDOCX() on a zip missing word/document.xml = nil error, want a rejection")
	}
}

func TestExtractDOCXRejectsCorruptZip(t *testing.T) {
	_, err := extractDOCX([]byte("PK\x03\x04not a real zip"))
	if err == nil {
		t.Error("extractDOCX() on corrupt zip data = nil error, want an error")
	}
}
