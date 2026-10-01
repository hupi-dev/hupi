package store

import (
	"testing"
)

// TestRankBM25_TiesAreBrokenDeterministicallyByID is a real regression
// test for review finding B10: rankBM25's sort had no tiebreaker beyond
// score, and the underlying SQL its real callers (keywordSearchEpisodes,
// keywordSearchEntities) issue has no ORDER BY — Postgres makes no
// guarantee about row order without one, so the exact same data could
// feed rankBM25 documents in a different order on a different run,
// and with no deterministic secondary sort key, which tied-score
// candidate ends up kept (once the maxVectorResults cap truncates the
// list) could vary run-to-run with identical underlying data — a real
// reproducibility risk this project's own iteration leans on avoiding
// (exact before/after benchmark comparisons).
//
// Every document here shares identical text, guaranteeing identical
// BM25 scores (a pure tie) for any query term they all contain. Fed to
// rankBM25 in descending-ID order deliberately — the fix must still
// return them in ascending-ID order regardless, since nothing about
// this input order should matter once scores are tied.
func TestRankBM25_TiesAreBrokenDeterministicallyByID(t *testing.T) {
	// rankBM25 caps its own output at maxVectorResults() — raised here so
	// this test's 15 tied documents aren't truncated before the full
	// ascending-by-id order can be asserted.
	t.Setenv("HUPI_MAX_VECTOR_RESULTS", "20")

	const text = "the quick brown fox jumps over the lazy dog"
	ids := []string{"z-doc", "y-doc", "x-doc", "w-doc", "v-doc", "u-doc", "t-doc", "s-doc", "r-doc", "q-doc", "p-doc", "o-doc", "n-doc", "m-doc", "l-doc"}
	docs := make([]bm25Document, len(ids))
	for i, id := range ids {
		docs[i] = newBM25Document(id, text)
	}

	got := rankBM25(docs, []string{"quick", "fox"})

	want := []string{"l-doc", "m-doc", "n-doc", "o-doc", "p-doc", "q-doc", "r-doc", "s-doc", "t-doc", "u-doc", "v-doc", "w-doc", "x-doc", "y-doc", "z-doc"}
	if len(got) != len(want) {
		t.Fatalf("rankBM25 returned %d ids, want %d: got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rankBM25 order = %v, want ascending-by-id %v (tied scores must be broken deterministically, not left to depend on input order)", got, want)
		}
	}
}
