package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateToBudget_DoesNotSplitMultiByteRune is a real regression
// test for review finding B9: truncateToBudget compared maxChars against
// len(s) (byte length) and cut with raw byte-slicing (s[:maxChars]) —
// for any non-ASCII content, that can land in the middle of a
// multi-byte UTF-8 sequence, producing invalid UTF-8, and can also
// trigger an unnecessary truncation when every actual character would
// have fit. "café" is 4 runes but 5 bytes (é is a 2-byte UTF-8
// sequence): byte-slicing to 4 bytes keeps "caf" plus é's lone first
// byte — an incomplete, invalid sequence — even though all 4 real
// characters fit within a 4-character budget.
func TestTruncateToBudget_DoesNotSplitMultiByteRune(t *testing.T) {
	got := truncateToBudget("café", 4)
	if !utf8.ValidString(got) {
		t.Errorf("truncateToBudget(%q, 4) = %q, which is not valid UTF-8", "café", got)
	}
	if got != "café" {
		t.Errorf(`truncateToBudget(%q, 4) = %q, want %q unchanged — all 4 characters fit a 4-character budget`, "café", got, "café")
	}
}

// TestTruncateToBudget_TruncatesAtARuneBoundaryWhenOverBudget confirms
// truncation past the budget still produces valid UTF-8 and the marker,
// cutting at a whole-character boundary rather than a byte offset.
func TestTruncateToBudget_TruncatesAtARuneBoundaryWhenOverBudget(t *testing.T) {
	got := truncateToBudget("café résumé", 6) // "café r" — 6 runes, multi-byte 'é' mid-string
	if !utf8.ValidString(got) {
		t.Errorf("truncateToBudget result is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "café r") {
		t.Errorf("truncateToBudget(%q, 6) = %q, want it to start with the first 6 whole characters %q", "café résumé", got, "café r")
	}
	if !strings.Contains(got, "[truncated to fit context budget]") {
		t.Errorf("truncateToBudget(%q, 6) = %q, want the truncation marker present", "café résumé", got)
	}
}

// TestHardTruncate_DoesNotSplitMultiByteRune is hardTruncate's own
// version of the same real regression.
func TestHardTruncate_DoesNotSplitMultiByteRune(t *testing.T) {
	got := hardTruncate("café", 4)
	if !utf8.ValidString(got) {
		t.Errorf("hardTruncate(%q, 4) = %q, which is not valid UTF-8", "café", got)
	}
	if got != "café" {
		t.Errorf(`hardTruncate(%q, 4) = %q, want %q unchanged — all 4 characters fit a 4-character budget`, "café", got, "café")
	}
}

// TestHardTruncate_TruncatesAtARuneBoundaryWhenOverBudget mirrors
// TestTruncateToBudget_TruncatesAtARuneBoundaryWhenOverBudget for
// hardTruncate's own (marker-free) truncation.
func TestHardTruncate_TruncatesAtARuneBoundaryWhenOverBudget(t *testing.T) {
	got := hardTruncate("café résumé", 6)
	if !utf8.ValidString(got) {
		t.Errorf("hardTruncate result is not valid UTF-8: %q", got)
	}
	if got != "café r" {
		t.Errorf("hardTruncate(%q, 6) = %q, want exactly the first 6 whole characters %q", "café résumé", got, "café r")
	}
}

// TestTruncateToBudget_AsciiBehaviorUnchanged confirms the common
// (pure-ASCII) case keeps behaving exactly as before — this fix only
// changes outcomes for non-ASCII content, where byte count and rune
// count used to diverge.
func TestTruncateToBudget_AsciiBehaviorUnchanged(t *testing.T) {
	if got := truncateToBudget("hello world", 5); got != "hello\n...[truncated to fit context budget]" {
		t.Errorf(`truncateToBudget("hello world", 5) = %q, want the first 5 ASCII characters plus the marker`, got)
	}
	if got := truncateToBudget("hello", 10); got != "hello" {
		t.Errorf(`truncateToBudget("hello", 10) = %q, want %q unchanged`, got, "hello")
	}
}
