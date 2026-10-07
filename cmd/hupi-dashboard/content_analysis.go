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
//
// One explicit, deliberate exception to "never raw text": the memory-map
// feature's topics overlay (memory_map.go's handleMemoryMapTopics, built
// on decryptConversations/aggregateConversationTopics below) returns a
// short, hard-capped excerpt of each conversation's own decrypted text
// alongside its extracted topic terms — a real product decision (shown
// per-conversation on the map, not just an aggregate term list), gated
// behind this same HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS flag rather
// than a new one: the risk profile is identical (decrypt, local keyword
// frequency only, never cached, never logged), so a second flag would
// just fragment "is decrypt-on-view enabled" into two toggles an
// operator has to reason about together for no behavioral difference.
package main

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
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

// ConversationContent is one episode's own decrypted text, already
// capped per-row (see decryptConversations' own doc comment for why a
// per-row cap is required here in addition to decryptRecentText's
// existing total-budget cap).
type ConversationContent struct {
	EpisodeID string
	TS        time.Time
	Text      string
}

// decryptConversations is decryptRecentText's sibling for the memory
// map's topics overlay (memory_map.go): keyed by an explicit episode-id
// list — the same set conversationsInRange already returned for the
// graph endpoint — instead of a day window, and returns one decrypted
// string per episode instead of one concatenated blob, since the
// memory map attributes topics/excerpts back to individual conversation
// nodes rather than showing one scope-wide aggregate.
//
// maxCharsPerConversation bounds a single row in addition to
// maxTotalChars' existing whole-request budget (same constant shape as
// decryptRecentText's maxChars) — without a per-row cap, one
// pathologically long conversation sorted to the front of the window
// (ordered by recency) could consume the entire total budget and starve
// every other conversation's own topics/excerpt.
func decryptConversations(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, episodeIDs []string, maxCharsPerConversation, maxTotalChars int) ([]ConversationContent, error) {
	if len(episodeIDs) == 0 {
		return nil, nil
	}
	var out []ConversationContent
	totalChars := 0
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, ts, input_text, output_text, key_version from episodes
			where scope_kind = $1 and scope_owner = $2 and id = any($3::text[])
			  and (input_text is not null or output_text is not null)
			order by ts desc
		`, scope.Kind, scope.Owner, pgfmt.TextArray(episodeIDs))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if totalChars >= maxTotalChars {
				break
			}
			var id string
			var ts time.Time
			var inputCT, outputCT []byte
			var keyVersion int
			if err := rows.Scan(&id, &ts, &inputCT, &outputCT, &keyVersion); err != nil {
				return err
			}
			enc, err := keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				return err
			}
			var sb strings.Builder
			if inputCT != nil {
				if text, err := enc.Decrypt(inputCT); err == nil {
					sb.WriteString(text)
					sb.WriteByte('\n')
				}
			}
			if outputCT != nil {
				if text, err := enc.Decrypt(outputCT); err == nil {
					sb.WriteString(text)
				}
			}
			text := sb.String()
			if len(text) > maxCharsPerConversation {
				text = text[:maxCharsPerConversation]
			}
			out = append(out, ConversationContent{EpisodeID: id, TS: ts, Text: text})
			totalChars += len(text)
		}
		return rows.Err()
	})
	return out, err
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
	// Extended after the memory map's topic nodes surfaced these as
	// noise on short, real conversations (e.g. "how"/"any"/"up"/"far"
	// tying with genuine signal terms at count=1) — still the same
	// "conservative" tradeoff this list's own doc comment describes,
	// just covering more of the common-function-word space that a
	// handful of real sentences actually exercises.
	"how": true, "any": true, "up": true, "far": true, "about": true,
	"out": true, "just": true, "now": true, "than": true, "too": true,
	"very": true, "really": true, "much": true, "more": true, "most": true,
	"some": true, "such": true, "no": true, "nor": true, "only": true,
	"own": true, "same": true, "few": true, "both": true, "each": true,
	"where": true, "when": true, "why": true, "what": true, "which": true,
	"who": true, "whom": true, "there": true, "here": true, "then": true,
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

// ConversationTopics is one conversation's own extracted topic terms —
// localKeywordThemes run over just that conversation's decrypted text,
// not the whole window's combined text the way handleContentThemes'
// scope-wide panel does.
type ConversationTopics struct {
	EpisodeID string
	Topics    []TermFrequency
}

// aggregateConversationTopics sums each conversation's own topic-term
// counts into one scope-wide ranked list — the memory map's global topic
// nodes. Pure and DB-free, deliberately: this is the one easily
// unit-testable piece of the topics overlay's otherwise decrypt-dependent
// path, same reasoning localKeywordThemes' own separation from
// decryptRecentText already follows.
func aggregateConversationTopics(perConv []ConversationTopics, topN int) []TermFrequency {
	counts := map[string]int{}
	for _, c := range perConv {
		for _, t := range c.Topics {
			counts[t.Term] += t.Count
		}
	}
	terms := make([]TermFrequency, 0, len(counts))
	for term, count := range counts {
		terms = append(terms, TermFrequency{Term: term, Count: count})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].Count != terms[j].Count {
			return terms[i].Count > terms[j].Count
		}
		return terms[i].Term < terms[j].Term // deterministic tie-break, same reasoning localKeywordThemes' own sort uses
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
