package store

import "testing"

func TestTokenize(t *testing.T) {
	got := tokenize("Meridian's job scheduler uses Rust and tokio!")
	// "s" (the leftover from splitting the possessive "Meridian's" on its
	// apostrophe) is deliberately absent — single-character tokens are
	// dropped, see tokenize's own doc comment for the real bug this
	// fixed.
	want := []string{"meridian", "job", "scheduler", "uses", "rust", "tokio"}
	if len(got) != len(want) {
		t.Fatalf("tokenize() = %v, want %v", got, want)
	}
	for i, term := range want {
		if got[i] != term {
			t.Errorf("tokenize()[%d] = %q, want %q", i, got[i], term)
		}
	}
}

func TestTokenizeDropsStopWords(t *testing.T) {
	got := tokenize("What is my favorite language?")
	for _, term := range got {
		if bm25StopWords[term] {
			t.Errorf("tokenize() kept stop word %q", term)
		}
	}
	// "favorite" and "language" are the only content words here.
	if len(got) != 2 {
		t.Fatalf("tokenize(%q) = %v, want exactly 2 content terms", "What is my favorite language?", got)
	}
}

func TestBM25ScoreZeroWhenNoTermsMatch(t *testing.T) {
	doc := newBM25Document("doc1", "Meridian uses Rust and tokio")
	docs := []bm25Document{doc}
	df := bm25DocFrequency(docs)
	avgLen := bm25AverageDocLength(docs)

	score := bm25Score(doc, tokenize("weather forecast tomorrow"), df, len(docs), avgLen)
	if score != 0 {
		t.Errorf("bm25Score() = %v for a document sharing no terms with the query, want 0", score)
	}
}

func TestBM25ScorePositiveWhenTermsMatch(t *testing.T) {
	doc := newBM25Document("doc1", "Meridian's scheduler is built on tokio, an async runtime")
	docs := []bm25Document{doc}
	df := bm25DocFrequency(docs)
	avgLen := bm25AverageDocLength(docs)

	score := bm25Score(doc, tokenize("what async runtime does Meridian use"), df, len(docs), avgLen)
	if score <= 0 {
		t.Errorf("bm25Score() = %v for overlapping terms, want > 0", score)
	}
}

// TestBM25RareTermScoresHigherThanCommonTerm is BM25's actual reason for
// existing alongside vector search: a term that appears in only one
// document out of many should score much higher (a specific, identifying
// match) than a term appearing in every document (an uninformative one),
// even at equal term frequency within the matched document.
func TestBM25RareTermScoresHigherThanCommonTerm(t *testing.T) {
	// "meridian" appears in only one of five documents (rare, identifying).
	// "project" appears in all five (common, uninformative).
	docs := []bm25Document{
		newBM25Document("doc1", "meridian project uses rust and tokio for scheduling"),
		newBM25Document("doc2", "project alpha uses go and postgres"),
		newBM25Document("doc3", "project beta is a react frontend"),
		newBM25Document("doc4", "project gamma handles billing in python"),
		newBM25Document("doc5", "project delta is a mobile app"),
	}
	df := bm25DocFrequency(docs)
	avgLen := bm25AverageDocLength(docs)

	rareTermScore := bm25Score(docs[0], tokenize("meridian"), df, len(docs), avgLen)
	commonTermScore := bm25Score(docs[0], tokenize("project"), df, len(docs), avgLen)

	if rareTermScore <= commonTermScore {
		t.Errorf("rare-term score (%v) should exceed common-term score (%v) for the same document", rareTermScore, commonTermScore)
	}
}

func TestBM25ScoreHandlesEmptyCorpus(t *testing.T) {
	score := bm25Score(newBM25Document("doc1", "anything"), tokenize("anything"), map[string]int{}, 0, 0)
	if score != 0 {
		t.Errorf("bm25Score() over an empty corpus = %v, want 0 (must not divide by zero avgDocLen)", score)
	}
}
