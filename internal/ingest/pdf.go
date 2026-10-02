package ingest

import (
	"bytes"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// extractPDF uses a pure-Go PDF reader — no cgo, no external pdftotext
// binary, consistent with HUPI's single-static-binary deployment model.
//
// Known, explicitly out-of-scope gap: a scanned/image-only PDF has no
// extractable text layer at all, under any such library — OCR is a
// materially bigger scope item, not solved here. Returning a Result with
// an empty Text and a clear Warning (rather than silently capturing
// nothing) means that case is visibly different from "the file had
// nothing in it" to whoever reads the warning.
func extractPDF(data []byte) (Result, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, err
	}

	reader, err := r.GetPlainText()
	if err != nil {
		return Result{}, err
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, reader); err != nil {
		return Result{}, err
	}

	text := strings.TrimSpace(sb.String())
	if text == "" {
		return Result{Warning: "no extractable text found — this PDF may be scanned/image-only"}, nil
	}
	return Result{Text: text}, nil
}
