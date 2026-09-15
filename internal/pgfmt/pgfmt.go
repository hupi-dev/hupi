// Package pgfmt formats Go values as Postgres literal strings so they can
// be bound as plain string parameters (cast with `::text[]` / `::vector`
// in SQL) instead of requiring a driver-specific array/vector type. Used
// by both internal/store and internal/consolidation, which is why it's
// its own package rather than living in either.
package pgfmt

import (
	"strconv"
	"strings"
)

// TextArray formats a Go string slice as a Postgres array literal
// (`{"a","b"}`).
func TextArray(items []string) string {
	if len(items) == 0 {
		return "{}"
	}
	quoted := make([]string, len(items))
	for i, it := range items {
		escaped := strings.ReplaceAll(it, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		quoted[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// ParseTextArray reverses TextArray for values read back out of Postgres
// (e.g. episodes.retrieved_summary_ids on a trace-inspection read path).
func ParseTextArray(literal string) []string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(literal, "{"), "}")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		p = strings.TrimPrefix(strings.TrimSuffix(p, `"`), `"`)
		p = strings.ReplaceAll(p, `\"`, `"`)
		p = strings.ReplaceAll(p, `\\`, `\`)
		out[i] = p
	}
	return out
}

// Nullable turns an empty string into a real SQL NULL rather than binding
// "" — required for columns with a check constraint over a fixed set of
// values (e.g. memory_gate, rating) or an FK (e.g. refers_to, supersedes),
// where "" would violate the constraint instead of correctly meaning
// "not applicable."
func Nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// VectorLiteral formats an embedding as the pgvector text literal
// (`[0.1,0.2,...]`).
func VectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
