package provider

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateForEmbeddingLeavesShortTextUnchanged(t *testing.T) {
	text := "a short piece of text well under the budget"
	if got := TruncateForEmbedding(text); got != text {
		t.Errorf("TruncateForEmbedding() = %q, want unchanged %q", got, text)
	}
}

// TestTruncateForEmbeddingCutsLongTextToBudget is the real, motivating
// case this function exists for: file/image attachment ingestion makes
// it realistic for a single episode's text to approach or exceed a real
// embedding model's own input-token limit (OpenAI's text-embedding-3-*
// family caps around 8191 tokens), which fails the whole Embed call
// outright rather than degrading gracefully — no caller anywhere in this
// codebase chunked or capped its input before this existed.
func TestTruncateForEmbeddingCutsLongTextToBudget(t *testing.T) {
	long := strings.Repeat("a", embedTruncateRuneBudget*2)
	got := TruncateForEmbedding(long)
	if n := len([]rune(got)); n != embedTruncateRuneBudget {
		t.Errorf("TruncateForEmbedding() returned %d runes, want exactly %d", n, embedTruncateRuneBudget)
	}
}

// TestTruncateForEmbeddingIsRuneSafe confirms the cut happens on a rune
// boundary, not a byte boundary — the same class of bug fixed elsewhere
// in this codebase (centeredExcerpt, keyFactEmbedMaxChars) for text
// containing multi-byte UTF-8.
func TestTruncateForEmbeddingIsRuneSafe(t *testing.T) {
	long := strings.Repeat("café ", embedTruncateRuneBudget) // multi-byte rune ('é') throughout
	got := TruncateForEmbedding(long)
	if !utf8.ValidString(got) {
		t.Errorf("TruncateForEmbedding() produced invalid UTF-8: %q", got[len(got)-20:])
	}
}
