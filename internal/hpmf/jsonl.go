package hpmf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// jsonlWriter appends one JSON object per line to a file, creating parent
// directories as needed — the JSONL half of MEMORY_FORMAT.md's directory
// layout (episodes and entities; summaries are one JSON array per file
// instead, see exportSummaries).
type jsonlWriter struct {
	f *os.File
	w *bufio.Writer
}

func newJSONLWriter(path string) (*jsonlWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("hpmf: create directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hpmf: open %s: %w", path, err)
	}
	return &jsonlWriter{f: f, w: bufio.NewWriter(f)}, nil
}

func (j *jsonlWriter) Write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("hpmf: marshal jsonl record: %w", err)
	}
	if _, err := j.w.Write(data); err != nil {
		return fmt.Errorf("hpmf: write jsonl record: %w", err)
	}
	if err := j.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("hpmf: write jsonl newline: %w", err)
	}
	return nil
}

func (j *jsonlWriter) Close() error {
	if err := j.w.Flush(); err != nil {
		j.f.Close()
		return fmt.Errorf("hpmf: flush %s: %w", j.f.Name(), err)
	}
	if err := j.f.Close(); err != nil {
		return fmt.Errorf("hpmf: close %s: %w", j.f.Name(), err)
	}
	return nil
}
