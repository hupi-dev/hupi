package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// maxDocxEntryBytes bounds how many decompressed bytes any single zip
// entry may produce — read via io.LimitReader against the actual bytes
// coming out of the decompressor, not the zip header's own declared
// UncompressedSize64 (which a crafted zip can lie about). A .docx is a
// zip of small XML parts; nothing legitimate needs anywhere close to
// this before hitting a hard error, independent of the overall
// attachment-size cap enforced before extraction ever starts.
const maxDocxEntryBytes = 64 * 1024 * 1024

// docxBodyEntry is the one required part of a well-formed .docx;
// header/footer/footnote parts are optional and read best-effort.
const docxBodyEntry = "word/document.xml"

var docxOptionalEntryPrefixes = []string{
	"word/header", "word/footer", "word/footnotes.xml", "word/endnotes.xml",
}

func isDocx(data []byte) bool {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	for _, f := range r.File {
		if f.Name == docxBodyEntry {
			return true
		}
	}
	return false
}

func extractDOCX(data []byte) (Result, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, fmt.Errorf("ingest: open docx zip: %w", err)
	}

	var sb strings.Builder
	bodyFound := false
	for _, f := range r.File {
		isBody := f.Name == docxBodyEntry
		isOptional := isBody || hasAnyPrefix(f.Name, docxOptionalEntryPrefixes)
		if !isOptional {
			continue
		}
		text, err := extractDocxPart(f)
		if err != nil {
			if isBody {
				return Result{}, fmt.Errorf("ingest: read %s: %w", docxBodyEntry, err)
			}
			// Header/footer/footnote parts are best-effort — a
			// malformed or missing one isn't fatal to the body text.
			continue
		}
		if isBody {
			bodyFound = true
		}
		if sb.Len() > 0 && text != "" {
			sb.WriteString("\n")
		}
		sb.WriteString(text)
	}
	if !bodyFound {
		return Result{}, fmt.Errorf("ingest: docx missing %s", docxBodyEntry)
	}
	return Result{Text: sb.String()}, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// extractDocxPart walks one zip entry's XML token stream, accumulating
// text inside <w:t> elements and inserting a newline at each </w:p>
// (paragraph) end — a minimal, stdlib-only reader for exactly the
// structure a .docx body/header/footer part has. Deliberately not a
// full OOXML parser: this only needs the plain text content, not
// formatting, styles, or any other structure.
//
// Reading through encoding/xml.Decoder is also a genuine security win
// over a libxml2-backed parser: it has no external-entity/DTD expansion
// by default (no custom Decoder.Entity is ever set here), so classic
// XXE/billion-laughs isn't reachable through it the way it would be
// through a C XML library.
func extractDocxPart(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	limited := io.LimitReader(rc, maxDocxEntryBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if len(data) > maxDocxEntryBytes {
		return "", fmt.Errorf("entry %s exceeds %d byte decompressed limit", f.Name, maxDocxEntryBytes)
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	var sb strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", f.Name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}
