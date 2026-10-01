package ingest

import (
	"strings"
	"testing"
)

func TestExtractPDFReturnsRealText(t *testing.T) {
	data := buildMinimalPDF("Hello HUPI")
	res, err := extractPDF(data)
	if err != nil {
		t.Fatalf("extractPDF: %v", err)
	}
	if !strings.Contains(res.Text, "Hello HUPI") {
		t.Errorf("extractPDF().Text = %q, want it to contain %q", res.Text, "Hello HUPI")
	}
	if res.Warning != "" {
		t.Errorf("extractPDF().Warning = %q, want empty for real text content", res.Warning)
	}
}

// TestExtractPDFWarnsOnEmptyText covers the scanned/image-only PDF case
// — no extractable text layer at all — which must surface a clear
// Warning rather than silently returning nothing with no signal why.
func TestExtractPDFWarnsOnEmptyText(t *testing.T) {
	data := buildMinimalPDF("")
	res, err := extractPDF(data)
	if err != nil {
		t.Fatalf("extractPDF: %v", err)
	}
	if res.Text != "" {
		t.Errorf("extractPDF().Text = %q, want empty", res.Text)
	}
	if res.Warning == "" {
		t.Error("extractPDF().Warning is empty, want a clear signal that nothing was extractable")
	}
}

func TestExtractPDFRejectsCorruptData(t *testing.T) {
	_, err := extractPDF([]byte("%PDF-1.4\nthis is not a real pdf structure at all"))
	if err == nil {
		t.Error("extractPDF() on corrupt data = nil error, want an error")
	}
}

func TestSniffFormatDetectsPDF(t *testing.T) {
	if got := sniffFormat(buildMinimalPDF("x")); got != formatPDF {
		t.Errorf("sniffFormat() = %v, want formatPDF", got)
	}
}
