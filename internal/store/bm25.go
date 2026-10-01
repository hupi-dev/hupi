package store

import (
	"math"
	"strings"
	"unicode"
)

// BM25 tuning constants — the standard Okapi BM25 defaults. Unlike
// vectorSimilarityThreshold and friends (empirically calibrated against
// real measured queries — see those constants' own doc comments), these
// have no real-usage data to calibrate against yet; re-measure once BM25
// search has real query traffic behind it.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// bm25StopWords is deliberately small and English-only — just enough to
// stop near-universal function words from contributing noise score (a
// word appearing in almost every document still gets a small positive
// IDF under the smoothed formula below, not zero). Not a linguistic
// stopword list for search quality in general; BM25 here is specifically
// meant to catch exact content-word matches vector search misses (IDs,
// names, acronyms), so filtering harder than this would work against
// that goal.
var bm25StopWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "am": true, "in": true, "on": true,
	"at": true, "to": true, "of": true, "and": true, "or": true, "but": true,
	"for": true, "with": true, "what": true, "when": true, "where": true,
	"who": true, "how": true, "do": true, "does": true, "did": true, "it": true,
	"this": true, "that": true, "my": true, "i": true, "you": true, "your": true,
}

// tokenize is BM25's shared tokenizer for both documents and queries —
// lowercase, split on anything that isn't a letter or digit, drop
// bm25StopWords. Deliberately no stemming: a memory product's queries
// and documents are short enough that exact-token overlap is the whole
// point of adding this alongside vector search, not something to blur
// further with fuzzy matching vector search already covers.
func tokenize(text string) []string {
	raw := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(raw))
	for _, t := range raw {
		// Splitting on anything non-alphanumeric (including apostrophes)
		// turns every possessive into a spurious single-letter "s" token
		// ("Meridian's" -> "meridian", "s"; "what's" -> "what", "s") —
		// found via real calibration measurement: an unrelated
		// true-negative query scored identically to a genuine near-miss
		// purely because both happened to contain a possessive. Bare
		// single-character tokens carry no real lexical content anyway,
		// so dropping them fixes this rather than special-casing
		// apostrophes specifically.
		if len(t) < 2 {
			continue
		}
		if !bm25StopWords[t] {
			terms = append(terms, t)
		}
	}
	return terms
}

// bm25Document is one candidate document's precomputed token data, built
// once per decrypted document and reused when scoring against every
// query term.
type bm25Document struct {
	id        string
	termFreq  map[string]int
	docLength int
}

func newBM25Document(id, text string) bm25Document {
	terms := tokenize(text)
	tf := make(map[string]int, len(terms))
	for _, t := range terms {
		tf[t]++
	}
	return bm25Document{id: id, termFreq: tf, docLength: len(terms)}
}

// bm25DocFrequency computes, across the given already-tokenized corpus,
// how many documents each distinct term appears in at least once — the
// "document frequency" BM25's IDF term needs. Computed fresh from the
// same decrypted set newBM25Document built its term frequencies from,
// not maintained as a persistent index: see keywordSearchSummaries' doc
// comment for why.
func bm25DocFrequency(docs []bm25Document) map[string]int {
	df := make(map[string]int)
	for _, doc := range docs {
		for term := range doc.termFreq {
			df[term]++
		}
	}
	return df
}

func bm25AverageDocLength(docs []bm25Document) float64 {
	if len(docs) == 0 {
		return 0
	}
	total := 0
	for _, doc := range docs {
		total += doc.docLength
	}
	return float64(total) / float64(len(docs))
}

// bm25Score scores one document against a set of query terms, given
// corpus-wide stats computed from the same decrypted candidate set (see
// bm25DocFrequency). Uses the smoothed/robust IDF form (ln(1 + (N-df+0.5)/
// (df+0.5))) rather than the original Robertson-Spärck-Jones formula,
// which can go negative for a term appearing in more than half the
// corpus — this form stays non-negative for any df <= corpusSize, the
// same choice modern implementations (e.g. Lucene since v6) made for the
// same reason. Uses the standard bm25B length-normalization weight — see
// bm25ScoreNoLengthNorm for why key-fact ranking deliberately doesn't.
func bm25Score(doc bm25Document, queryTerms []string, docFreq map[string]int, corpusSize int, avgDocLen float64) float64 {
	return bm25ScoreWithB(doc, queryTerms, docFreq, corpusSize, avgDocLen, bm25B)
}

// bm25ScoreNoLengthNorm is bm25Score with length normalization disabled
// (b=0) — used for ranking a summary's own key facts (factScores,
// internal/store/retrieve.go), not summaries/episodes/entities. BM25's
// length normalization exists to stop a long, topically-diffuse document
// from matching a query term "by chance" among lots of unrelated content
// — a real concern for a full episode or summary, but key facts are
// already short, single-topic, atomic statements by construction (that's
// the point of extracting them), so length differences between them are
// just incidental phrasing, not a dilution signal. Real, measured
// regression without this: the carefully-verified fix for gpt4_45189cb4
// (rankKeyFacts' own doc comment — the NFL-playoffs fact vs. an unrelated
// climate fact vs. a scavenger-hunt fact, each sharing exactly one query
// term) depends on those three facts tying lexically so RRF's semantic
// signal can break the tie correctly; with standard length normalization,
// the longer NFL sentence scored lower than the shorter scavenger-hunt
// one purely from its length, even though both match exactly one term —
// breaking that tie the wrong way before semantic ranking ever got a
// chance to weigh in. b=0 restores the tie (confirmed against the same
// fixture) while still applying real IDF weighting — exactly what
// 5809eb10 ("what year did construction begin," see factScores' own doc
// comment) needed and flat word-overlap counting didn't have at all.
func bm25ScoreNoLengthNorm(doc bm25Document, queryTerms []string, docFreq map[string]int, corpusSize int) float64 {
	return bm25ScoreWithB(doc, queryTerms, docFreq, corpusSize, 0, 0)
}

func bm25ScoreWithB(doc bm25Document, queryTerms []string, docFreq map[string]int, corpusSize int, avgDocLen float64, b float64) float64 {
	// b == 0 (bm25ScoreNoLengthNorm) never touches avgDocLen at all, so
	// an empty/zero-length corpus can't divide by zero there — only the
	// standard, length-normalized path (b == bm25B) needs the original
	// guard.
	if b != 0 && avgDocLen == 0 {
		return 0
	}
	var score float64
	for _, term := range queryTerms {
		tf := doc.termFreq[term]
		if tf == 0 {
			continue
		}
		df := docFreq[term]
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (float64(corpusSize)-float64(df)+0.5)/(float64(df)+0.5))
		numerator := float64(tf) * (bm25K1 + 1)
		lengthNorm := 1.0
		if b != 0 {
			lengthNorm = 1 - b + b*float64(doc.docLength)/avgDocLen
		}
		denominator := float64(tf) + bm25K1*lengthNorm
		score += idf * numerator / denominator
	}
	return score
}
