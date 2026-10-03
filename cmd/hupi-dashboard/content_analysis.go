// Phase 2 of docs/DASHBOARD.md — the deliberately separate, opt-in
// "decrypt-on-view" theme extraction. Unlike every query in queries.go,
// this file genuinely decrypts episode/summary content — the one new
// place in this product where plaintext conversation text touches a
// request/response path instead of staying inside the
// retrieve-or-consolidate-then-reencrypt loop internal/store/retrieve.go
// and internal/consolidation already own. Gated behind two independent,
// default-off env vars (same honest opt-in convention as
// HUPI_ENABLE_KEYWORD_SEARCH/HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK):
//
//   - HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS: local keyword-frequency
//     extraction, no LLM call — decrypted text never leaves this process.
//   - HUPI_ENABLE_DASHBOARD_LLM_THEMES: a richer narrative summary,
//     reusing the same provider registry retrieve.go/consolidation
//     already trust with plaintext — a separate decision from the one
//     above, not bundled into it.
//
// Neither handler ever returns raw decrypted text — only an aggregated
// term list (2a) or a model-generated narrative paragraph (2b), and
// neither logs the decrypted text itself anywhere (only counts/errors).
package main

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
	"unicode"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

func contentAnalysisEnabled() bool {
	return os.Getenv("HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS") == "true"
}

func llmThemesEnabled() bool {
	return os.Getenv("HUPI_ENABLE_DASHBOARD_LLM_THEMES") == "true"
}

// defaultContentAnalysisDays/-MaxChars/-TopTerms are reasoned, not
// measured — same honest status as forgottenButImportant's own defaults.
// MaxChars bounds how much decrypted text a single request ever holds in
// memory or sends to an LLM, not just a performance nicety.
const (
	defaultContentAnalysisDays     = 30
	defaultContentAnalysisMaxChars = 200_000
	defaultContentAnalysisTopTerms = 30
)

// decryptRecentText gathers this scope's episode input/output text and
// current-summary prose from the last days, decrypting each row with its
// own stored key_version (same per-row GetVersion pattern
// internal/store/retrieve.go uses — a scope can have rows written under
// more than one key version after a rotation). Stops accumulating once
// maxChars is reached rather than decrypting the entire window
// unconditionally — this is a display feature, not a retrieval path, so
// an unbounded scope shouldn't make one dashboard request decrypt
// everything it has ever stored.
func decryptRecentText(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, days, maxChars int) (string, error) {
	var sb strings.Builder
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select input_text, output_text, key_version from episodes
			where type = 'interaction' and scope_kind = $1 and scope_owner = $2
			  and ts >= now() - make_interval(days => $3)
			  and (input_text is not null or output_text is not null)
			order by ts desc
		`, scope.Kind, scope.Owner, days)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if sb.Len() >= maxChars {
				break
			}
			var inputCT, outputCT []byte
			var keyVersion int
			if err := rows.Scan(&inputCT, &outputCT, &keyVersion); err != nil {
				return err
			}
			enc, err := keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				return err
			}
			if inputCT != nil {
				if text, err := enc.Decrypt(inputCT); err == nil {
					sb.WriteString(text)
					sb.WriteByte('\n')
				}
			}
			if outputCT != nil {
				if text, err := enc.Decrypt(outputCT); err == nil {
					sb.WriteString(text)
					sb.WriteByte('\n')
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	text := sb.String()
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return text, nil
}

// TermFrequency is one term's raw count across the decrypted window.
type TermFrequency struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

// contentAnalysisStopwords is a small, intentionally conservative list —
// same tradeoff internal/store/bm25.go's own tokenizer doc comment
// describes (dropping too aggressively risks losing a real, rare
// signal term; dropping too little just leaves common connective words
// in the output, which this feature can tolerate better than BM25's
// scoring can).
var contentAnalysisStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"to": true, "of": true, "in": true, "on": true, "for": true, "with": true,
	"that": true, "this": true, "it": true, "as": true, "at": true, "by": true,
	"i": true, "you": true, "we": true, "they": true, "he": true, "she": true,
	"do": true, "does": true, "did": true, "have": true, "has": true, "had": true,
	"not": true, "so": true, "if": true, "my": true, "your": true, "our": true,
	"can": true, "will": true, "would": true, "could": true, "should": true,
}

// localKeywordThemes counts word frequency over text, excluding
// stopwords and single-character tokens (the same possessive-splitting
// artifact internal/store/bm25.go's own tokenizer avoids), and returns
// the topN most frequent terms. Simple frequency, not real TF-IDF — this
// scope's own text is the only corpus in scope, so there's no second
// document set to compute an inverse-document-frequency term against.
func localKeywordThemes(text string, topN int) []TermFrequency {
	counts := map[string]int{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) <= 1 || contentAnalysisStopwords[word] {
			continue
		}
		counts[word]++
	}

	terms := make([]TermFrequency, 0, len(counts))
	for term, count := range counts {
		terms = append(terms, TermFrequency{Term: term, Count: count})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].Count != terms[j].Count {
			return terms[i].Count > terms[j].Count
		}
		return terms[i].Term < terms[j].Term // deterministic tie-break, same reasoning bm25_tiebreak_test.go documents
	})
	if len(terms) > topN {
		terms = terms[:topN]
	}
	return terms
}

const narrativeThemesSystemPrompt = `You summarize themes in a person's own private conversation history for a dashboard they are looking at themselves. Write 2-4 sentences, plain prose, third person ("they discussed..."), covering only what is actually present in the text. Never invent a theme that isn't there. Never quote the source text directly or reproduce specific sentences — describe the themes, don't excerpt the content.`

// llmNarrativeThemes asks the configured chat provider for a short prose
// summary of text's themes — richer than localKeywordThemes' bare term
// list, at the cost of sending decrypted content to that provider for
// this purpose specifically (HUPI_ENABLE_DASHBOARD_LLM_THEMES's own doc
// comment). chat is internal/provider.Registry's own Chat() profile —
// the same one retrieval/consolidation already trust with plaintext.
func llmNarrativeThemes(ctx context.Context, chat provider.Provider, text string) (string, error) {
	resp, err := chat.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: narrativeThemesSystemPrompt},
			{Role: provider.RoleUser, Content: text},
		},
	})
	if err != nil {
		return "", err
	}
	return resp.Message.Content, nil
}
