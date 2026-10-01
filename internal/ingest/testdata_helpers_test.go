package ingest

import (
	"archive/zip"
	"bytes"
	"fmt"
)

// buildMinimalPDF constructs a small, valid, uncompressed single-page
// PDF containing the given text (via the Tj text-showing operator) — a
// real, parseable fixture rather than a binary file committed to the
// repo, computing its own xref byte offsets so there's nothing fragile
// to keep in sync by hand.
func buildMinimalPDF(text string) []byte {
	var buf bytes.Buffer
	offsets := make([]int, 0, 6)

	buf.WriteString("%PDF-1.4\n")

	writeObj := func(n int, body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", n, body)
	}

	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >> /MediaBox [0 0 612 792] /Contents 5 0 R >>")
	writeObj(4, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", pdfEscape(text))
	writeObj(5, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))

	xrefOffset := buf.Len()
	buf.WriteString("xref\n")
	fmt.Fprintf(&buf, "0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	buf.WriteString("trailer\n")
	fmt.Fprintf(&buf, "<< /Size %d /Root 1 0 R >>\n", len(offsets)+1)
	buf.WriteString("startxref\n")
	fmt.Fprintf(&buf, "%d\n", xrefOffset)
	buf.WriteString("%%EOF")

	return buf.Bytes()
}

func pdfEscape(s string) string {
	var sb bytes.Buffer
	for _, r := range s {
		switch r {
		case '(', ')', '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// buildMinimalDocx constructs a valid, minimal .docx (a zip containing
// just word/document.xml) with the given paragraphs as <w:p><w:t>...
// elements — enough structure for extractDocxPart to walk, without
// pulling in a real .docx-writing library.
func buildMinimalDocx(paragraphs ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>`)
		body.WriteString(xmlEscape(p))
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	body.WriteString(`</w:body></w:document>`)

	w, err := zw.Create("word/document.xml")
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func xmlEscape(s string) string {
	var sb bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			sb.WriteString("&amp;")
		case '<':
			sb.WriteString("&lt;")
		case '>':
			sb.WriteString("&gt;")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// buildZipBombDocx constructs a zip whose word/document.xml entry
// decompresses to well over maxDocxEntryBytes, for the zip-bomb defense
// test — a single highly-repetitive text part compresses extremely well
// (deflate), so the zip file itself stays small while the decompressed
// output is huge.
func buildZipBombDocx(decompressedSize int) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		panic(err)
	}
	chunk := bytes.Repeat([]byte("a"), 1<<16)
	written := 0
	for written < decompressedSize {
		n := len(chunk)
		if written+n > decompressedSize {
			n = decompressedSize - written
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			panic(err)
		}
		written += n
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
