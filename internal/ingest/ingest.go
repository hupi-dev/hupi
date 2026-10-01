// Package ingest turns an uploaded file's raw bytes into plain text HUPI
// can fold into a captured episode the same way any other chat content
// is — see ARCHITECTURE.md § Capture and the file-ingestion design: every
// downstream consumer of episode content (consolidation prompts,
// embeddings, retrieval ranking) is string-typed with no multimodal
// awareness, so the only sound strategy is converting a file to text
// once, here, at ingest time.
//
// Pure functions only, no DB/network — a leaf package, same shape as
// internal/crypto and internal/pgfmt, so internal/gateway can import it
// with no cycle risk.
package ingest

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// Attachment is the raw input Extract works from. Filename/ContentType
// are caller-supplied hints only — Extract dispatches on sniffed magic
// bytes, never on these alone, since an uploader's claimed content-type
// or extension is untrusted.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Result is empty Text with a non-empty Warning for "parsed fine, but
// there was nothing to extract" (a scanned/image-only PDF, for
// instance) — distinct from an error, which means parsing itself
// failed.
type Result struct {
	Text    string
	Warning string
}

// extractTimeout bounds how long a single attachment's parse is allowed
// to run — PDF/XML parsing on pathological, attacker-influenced binary
// input is a classic slow-path source, and this is a hard ceiling
// independent of whatever timeout the caller's own context carries. A
// var, not a const, so tests can shrink it rather than waiting out the
// real duration.
var extractTimeout = 10 * time.Second

// doExtract is extractByFormat by default — a package-level var, not a
// direct call, so tests can substitute a never-returning or panicking
// stand-in to exercise Extract's own timeout/recovery behavior without
// needing a pathological real file to trigger it.
var doExtract = extractByFormat

// Extract dispatches on the attachment's sniffed format (magic bytes),
// recovering from any panic a format-specific extractor raises —
// third-party/hand-rolled parsers run against attacker-influenced binary
// data are a classic panic source, and one malformed attachment must
// never crash the shared gateway process serving other tenants'
// concurrent requests.
func Extract(ctx context.Context, att Attachment) (result Result, err error) {
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: fmt.Errorf("ingest: panic extracting %q: %v", att.Filename, r)}
			}
		}()
		res, err := doExtract(att)
		done <- outcome{result: res, err: err}
	}()

	select {
	case <-ctx.Done():
		return Result{}, fmt.Errorf("ingest: extracting %q: %w", att.Filename, ctx.Err())
	case o := <-done:
		return o.result, o.err
	}
}

type format int

const (
	formatText format = iota
	formatPDF
	formatDOCX
)

func extractByFormat(att Attachment) (Result, error) {
	switch sniffFormat(att.Data) {
	case formatPDF:
		return extractPDF(att.Data)
	case formatDOCX:
		return extractDOCX(att.Data)
	default:
		return extractText(att.Data)
	}
}

var (
	pdfMagic = []byte("%PDF-")
	zipMagic = []byte("PK\x03\x04")
)

// sniffFormat dispatches on actual bytes, not the caller-supplied
// Filename/ContentType — both are untrusted hints a client could set to
// anything.
func sniffFormat(data []byte) format {
	if bytes.HasPrefix(data, pdfMagic) {
		return formatPDF
	}
	if bytes.HasPrefix(data, zipMagic) && isDocx(data) {
		return formatDOCX
	}
	return formatText
}
