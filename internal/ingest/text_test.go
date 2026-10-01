package ingest

import (
	"errors"
	"strings"
	"testing"
)

func TestExtractTextPassesThroughValidUTF8(t *testing.T) {
	data := []byte("plain text with a café and some “smart quotes”")
	res, err := extractText(data)
	if err != nil {
		t.Fatalf("extractText: %v", err)
	}
	if res.Text != string(data) {
		t.Errorf("extractText().Text = %q, want unchanged %q", res.Text, string(data))
	}
}

func TestExtractTextEmptyInput(t *testing.T) {
	res, err := extractText(nil)
	if err != nil {
		t.Fatalf("extractText(nil): %v", err)
	}
	if res.Text != "" || res.Warning != "" {
		t.Errorf("extractText(nil) = %+v, want zero value", res)
	}
}

// TestExtractTextRepairsAFewBadBytes covers the common real case: a
// text file saved under a legacy encoding with a handful of bytes that
// don't decode as UTF-8 — repaired via strings.ToValidUTF8, not
// rejected outright, since the replacement ratio stays low.
func TestExtractTextRepairsAFewBadBytes(t *testing.T) {
	data := append([]byte("mostly valid ascii text here, "), 0xff, 0xfe)
	data = append(data, []byte(" with a little more valid text after it to keep the ratio low")...)
	res, err := extractText(data)
	if err != nil {
		t.Fatalf("extractText: %v", err)
	}
	if strings.Contains(res.Text, "\x00") {
		t.Errorf("extractText().Text = %q, want no raw null bytes", res.Text)
	}
	if !strings.Contains(res.Text, "mostly valid ascii text here") {
		t.Errorf("extractText().Text = %q, want the valid surrounding text preserved", res.Text)
	}
}

// TestExtractTextRejectsMostlyBinaryContent is the mislabeled-binary
// guard: a "text file" that's mostly unrepairable garbage after UTF-8
// repair is a strong signal it's actually a binary file, not text saved
// under a legacy encoding — rejected with a clear error instead of
// capturing mostly replacement characters into the pipeline.
func TestExtractTextRejectsMostlyBinaryContent(t *testing.T) {
	data := make([]byte, 200)
	for i := range data {
		data[i] = byte(0x80 + i%0x7f) // mostly invalid UTF-8 continuation/lead bytes
	}
	_, err := extractText(data)
	if err == nil {
		t.Error("extractText() on mostly-binary content = nil error, want a rejection")
	}
	var invalidErr *InvalidTextError
	if !errors.As(err, &invalidErr) {
		t.Errorf("extractText() error = %v, want an *InvalidTextError", err)
	}
}

func TestSniffFormatDefaultsToText(t *testing.T) {
	if got := sniffFormat([]byte("just plain text")); got != formatText {
		t.Errorf("sniffFormat() = %v, want formatText", got)
	}
}
