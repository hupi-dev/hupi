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
//
// Scans by quote/escape state rather than a naive strings.Split(",") —
// splitting on every comma would incorrectly break an item that itself
// contains a literal comma into multiple items, since TextArray only
// escapes '"' and '\', not ','. None of today's actual values (episode
// ids, dates, "kind:slug" entity ids) can contain a comma, but this is
// this function's own documented contract ("reverses TextArray"), and a
// future caller passing more general text through it shouldn't inherit a
// silent data-corruption bug.
func ParseTextArray(literal string) []string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(literal, "{"), "}")
	if trimmed == "" {
		return nil
	}

	var out []string
	var current strings.Builder
	inQuotes := false
	escaped := false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case escaped:
			current.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	out = append(out, current.String())
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
