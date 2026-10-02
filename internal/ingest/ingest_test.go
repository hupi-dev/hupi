package ingest

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExtractDispatchesByContent(t *testing.T) {
	for name, att := range map[string]Attachment{
		"text":       {Filename: "notes.txt", Data: []byte("plain text content")},
		"pdf":        {Filename: "doc.pdf", Data: buildMinimalPDF("pdf content")},
		"docx":       {Filename: "doc.docx", Data: buildMinimalDocx("docx content")},
		"mislabeled": {Filename: "doc.pdf", Data: []byte("actually just plain text, despite the .pdf name")},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Extract(context.Background(), att)
			if err != nil {
				t.Fatalf("Extract(%s): %v", name, err)
			}
			if res.Text == "" {
				t.Errorf("Extract(%s).Text is empty, want real extracted content", name)
			}
		})
	}
}

// TestExtractDispatchIgnoresContentTypeHint confirms dispatch is based
// on sniffed magic bytes, not the caller-supplied ContentType/Filename
// hint — both are untrusted, and a client claiming "application/pdf"
// for plain text content must not be trusted blindly.
func TestExtractDispatchIgnoresContentTypeHint(t *testing.T) {
	att := Attachment{
		Filename:    "fake.pdf",
		ContentType: "application/pdf",
		Data:        []byte("this is plain text, not a real pdf"),
	}
	res, err := Extract(context.Background(), att)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Text, "plain text") {
		t.Errorf("Extract().Text = %q, want the plain text content extracted despite the .pdf hint", res.Text)
	}
}

// TestExtractTimesOutRatherThanHanging confirms Extract's own hard
// timeout fires even when the underlying extractor never returns —
// PDF/XML parsing on pathological input is a classic slow-path risk,
// and a single malformed attachment must not be able to hang the
// request indefinitely. doExtract is substituted with a stand-in that
// blocks forever, rather than needing a real pathological file to
// trigger a hang.
func TestExtractTimesOutRatherThanHanging(t *testing.T) {
	origTimeout, origExtract := extractTimeout, doExtract
	extractTimeout = 50 * time.Millisecond
	doExtract = func(Attachment) (Result, error) {
		select {} // blocks forever
	}
	defer func() { extractTimeout, doExtract = origTimeout, origExtract }()

	start := time.Now()
	_, err := Extract(context.Background(), Attachment{Filename: "slow"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Extract() on a never-returning extractor = nil error, want a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Extract() took %v, want it to return promptly once the timeout fires", elapsed)
	}
}

// TestExtractRecoversFromPanic confirms a panic inside a format-specific
// extractor is converted into a plain error, not propagated — one
// malformed attachment must never crash the shared gateway process.
func TestExtractRecoversFromPanic(t *testing.T) {
	origExtract := doExtract
	doExtract = func(Attachment) (Result, error) {
		panic("simulated extractor panic")
	}
	defer func() { doExtract = origExtract }()

	_, err := Extract(context.Background(), Attachment{Filename: "panics"})
	if err == nil {
		t.Fatal("Extract() on a panicking extractor = nil error, want a recovered error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("Extract() error = %v, want it to mention the recovered panic", err)
	}
}
