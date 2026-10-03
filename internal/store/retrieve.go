package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

const (
	// vectorSimilarityThreshold is the exact line between "found nothing
	// trustworthy" (partial) and "found something" (full) — see
	// ARCHITECTURE.md's precise definition of the gate outcomes. Cosine
	// similarity, so a pgvector cosine *distance* of 1-threshold or less
	// counts as a hit.
	//
	// 0.75 was the original design value but turned out to be
	// miscalibrated for text-embedding-3-small: an initial single
	// measurement (a correctly grounded summary against a direct question
	// about its own content) came back only 0.50, despite being an exact
	// semantic match, dropping this to 0.40. A later, broader measurement
	// against a real (long, multi-fact) daily summary confirmed 0.40
	// rather than just replacing one guess with another:
	//
	//   0.0716  true negative  — "what's the weather like today?"
	//   0.1217  true negative  — "write me a haiku about the ocean"
	//   0.1792  true positive  — "what language do I prefer?" (a real
	//                            fact, but a single sentence buried in a
	//                            long summary about something else)
	//   0.1830  true negative  — "how do I set up a Kubernetes ingress?"
	//   0.2866  near-miss      — "how does memory retrieval work?"
	//   0.3023  true positive  — a dashboard fact, similarly buried
	//   0.3288  near-miss      — generic Postgres tuning question
	//   0.5226  true positive  — the summary's actual central topic, paraphrased
	//   0.5729  true positive  — same, a different central fact
	//   0.6301  true positive  — same, asked directly
	//
	// Sorted by similarity, true positives and true negatives *interleave*
	// below ~0.33 — a real fact can score lower than a wholly unrelated
	// question, because a long multi-topic summary embeds as an average
	// of everything it mentions, and a single buried sentence barely
	// moves that average. No threshold value fixes that; 0.40 is simply
	// the only clean gap with zero false positives in this measurement
	// (between the highest near-miss, 0.33, and the lowest strong true
	// positive, 0.52) — going lower to catch the buried facts would also
	// admit the near-misses sitting right next to them. The actual fix
	// for a buried fact is giving it its own focused embedding instead of
	// diluting it inside one long summary — see entityVectorSimilarityThreshold
	// and schema/0012_entity_embeddings.sql, which exist for exactly this
	// reason. Re-measure if the embedding model changes.
	vectorSimilarityThreshold = 0.40
	// entityVectorSimilarityThreshold is vectorSimilarityThreshold's
	// counterpart for entities (schema/0012_entity_embeddings.sql) — kept
	// separate rather than reusing vectorSimilarityThreshold because
	// entity text (a short "name (kind)\nkey: value" rendering, see
	// EntityEmbedText in internal/consolidation/store.go) has different
	// embedding characteristics than a multi-sentence narrative summary,
	// and 0.40 was calibrated specifically against summary prose.
	//
	// Empirically measured against a real deployment's data
	// (entity "preference:favorite-programming-language" = "Rust"):
	//   "what programming language do I prefer?"      -> 0.6811 (true positive)
	//   "what's my favorite programming language?"     -> 0.6666 (true positive)
	//   "do I like Rust?" vs the same entity            -> 0.5343 (true positive,
	//     the weakest one measured)
	//   "do I like Rust?" vs entity "skill:rust"        -> 0.6240 (also a
	//     legitimate match — a different, also-relevant entity)
	//   "prefer"/"favorite" queries vs "skill:rust"     -> 0.41-0.44 (a related
	//     but not-quite-right entity — correctly excluded)
	//   "what's the weather today?" vs any of the above -> 0.07-0.10 (genuinely
	//     unrelated, nowhere close)
	// 0.50 sits in the gap between the weakest true positive (0.53) and the
	// highest near-miss (0.44), with unrelated content an order of
	// magnitude below both — re-measure if the embedding model changes.
	entityVectorSimilarityThreshold = 0.50
	// recommendationEntitySimilarityThreshold/-MaxResults widen entity
	// retrieval specifically for a detected recommendation-seeking
	// question (looksLikeRecommendationRequest,
	// docs/LONGMEMEVAL_ACCURACY_PLAN.md category 1) — a real, observed
	// gap: "recommend a cultural event" shares no vocabulary at all with
	// a preference stated weeks earlier ("I want to practice my
	// Spanish"), so entityVectorSimilarityThreshold's normal 0.50 bar
	// under-admits exactly the standing facts this query shape needs.
	// Not yet measured against real embedding distances the way the
	// thresholds above were (0.40/0.50/0.55) — a reasoned starting point
	// (roughly half the normal bar, double the normal result cap),
	// flagged for real calibration once this is verified to help at all.
	recommendationEntitySimilarityThreshold = 0.25
	recommendationEntityMaxResults          = 10
	// orderingSummarySimilarityThreshold/-MaxResults widen SUMMARY
	// retrieval specifically for a detected multi-event/ordering-shaped
	// question (looksLikeOrderingRequest,
	// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 2) — the same
	// widening idea Category 2 cause 1 tried once already
	// (docs/LONGMEMEVAL_ACCURACY_PLAN.md) and reverted, but that attempt
	// only widened mmrSelect's final pick count, not the vector fetch's
	// own similarity threshold — real tracing (HUPI_DEBUG_FUSION) showed
	// every candidate in that case had vectorRank=-1: the vector search
	// itself never admitted a single summary above
	// vectorSimilarityThreshold (0.40) for that query, so widening how
	// many get *picked* from an empty vector pool did nothing. This
	// widens the threshold and fetch/keyword caps themselves — mirroring
	// recommendationEntitySimilarityThreshold's exact pattern, applied to
	// summaries instead of entities — real-verified need: the
	// sports-order LongMemEval case's needed summaries ("Spring Sprint
	// Triathlon," "Midsummer 5K Run") share no literal vocabulary with a
	// generic query like "order of sports events," so they score zero on
	// both BM25 keyword matching and (at the normal threshold) vector
	// similarity, and never enter the candidate pool at all — confirmed
	// via hupi-export-memory that the facts genuinely exist in
	// consolidated summaries; this is a pure retrieval-admission gap, not
	// a consolidation or context-budget one (see Phase D item 1, already
	// fixed separately). Not yet measured against real embedding
	// distances the way the thresholds above were — a reasoned starting
	// point (same 0.25/wider-cap shape as the entity case), flagged for
	// calibration once verified to help at all.
	orderingSummarySimilarityThreshold = 0.25
	orderingSummaryMaxResults          = 15
	// episodeVectorSimilarityThreshold is vectorSimilarityThreshold's
	// counterpart for individual episodes (vectorSearchEpisodes) — used
	// to reuse vectorSimilarityThreshold outright, which real measurement
	// showed was a genuine miscalibration, not just an unverified
	// assumption like the entity case started as.
	//
	// Measured against a real single-exchange episode (the user
	// explaining Meridian's architecture — job scheduler, Rust/tokio,
	// Postgres over Redis, 500 concurrent jobs/node):
	//   "what's the weather like today?"                    -> 0.0397 (TN)
	//   "write me a haiku about the ocean"                  -> 0.0985 (TN)
	//   "how do I set up a Kubernetes ingress controller?"  -> 0.1325 (TN)
	//   "what's a reasonable concurrency limit for a task
	//     queue?" (generic, not about this episode)          -> 0.3911 (near-miss)
	//   "tell me about my job scheduler's core architecture" -> 0.4477 (true positive,
	//     a broad paraphrase)
	//   "should I use Postgres or Redis for a job queue in
	//     general?" (generic, not about this episode)        -> 0.5110 (near-miss)
	//   "what database does Meridian use instead of Redis,
	//     and why?"                                          -> 0.6478 (true positive)
	//   "what async runtime does Meridian's scheduler use?"  -> 0.6544 (true positive)
	//   "how many concurrent jobs can Meridian handle per
	//     worker node?"                                      -> 0.6851 (true positive)
	//
	// At vectorSimilarityThreshold (0.40), the 0.5110 near-miss — a
	// generic Postgres-vs-Redis question with no connection to this
	// user's own data — would have been treated as a match. The clean
	// gap here sits between 0.51 (highest near-miss) and 0.65 (lowest of
	// the three direct-restatement true positives), at the cost of the
	// one broader/vaguer paraphrase (0.4477) no longer clearing it. That
	// trade is deliberate: episodes are a supplementary path for content
	// not yet folded into a summary (see this function's own doc
	// comment), not the primary retrieval surface the way summaries are,
	// and unlike a summary or entity, raw episode text has never been
	// through grounding — a false positive here means handing the model
	// unverified, possibly-hallucinated content and calling it "your own
	// memory," which is worse than an occasional missed broad paraphrase.
	// Re-measure if the embedding model changes.
	episodeVectorSimilarityThreshold = 0.55
	// defaultMaxVectorResults is maxVectorResults()'s fallback — see that
	// function's own doc comment for why it's overridable.
	defaultMaxVectorResults = 5
	// defaultContextCharBudget is contextCharBudget()'s fallback — a crude
	// stand-in for a real token budget (ARCHITECTURE.md mentions ~20% of
	// the model's context window) — a production build should count
	// tokens against the target model's tokenizer, not characters. Left
	// unchanged as the default (small, safe for a modest local model) so
	// no existing deployment's cost/latency profile shifts silently; see
	// contextCharBudget's own doc comment for why a larger-context model
	// needs to raise this explicitly instead.
	defaultContextCharBudget = 2000
)

// stage1SignalKeywords is the cheap, local, no-LLM-call pre-check from
// ARCHITECTURE.md's gate table: does this message look like it's worth
// searching for at all. Entity-name matches (stage1EntityMatches) are the
// other, usually stronger, half of stage 1.
var stage1SignalKeywords = []string{
	"remember", "recall", "decided", "decide", "prefer",
	"we discussed", "last time", "again", "what did", "earlier", "before",
}

// recommendationKeywords backs looksLikeRecommendationRequest
// (docs/LONGMEMEVAL_ACCURACY_PLAN.md category 1) — a real query-shape
// signal, same cheap substring-match pattern as stage1SignalKeywords,
// deliberately generous (a false positive here just widens entity
// retrieval a bit for one query, unlike stage1's own keywords, which
// gate whether search runs at all).
var recommendationKeywords = []string{
	"recommend", "suggest", "should i", "any tips", "any advice",
	"do you think", "any ideas", "what would you", "any suggestions",
	"could there be a reason", "why might", "any idea why",
}

// looksLikeRecommendationRequest detects a question asking for a
// personalized recommendation/suggestion — the shape that needs standing
// preference facts recalled even when the current question's own wording
// shares nothing with how that preference was originally stated.
func looksLikeRecommendationRequest(query string) bool {
	lower := strings.ToLower(query)
	for _, kw := range recommendationKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// orderingKeywords backs looksLikeOrderingRequest
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 2), grounded in
// the actual real LongMemEval questions that failed this way (see
// orderingSummarySimilarityThreshold's own doc comment) — same cheap,
// deliberately generous substring-match pattern as recommendationKeywords.
var orderingKeywords = []string{
	"order of", "which came first", "came first", "first or", "or first",
	"which task did i", "which item did i", "which did i",
	"how many months", "how many weeks", "how many days",
	"in a row", "consecutive", "earliest to latest", "earliest",
}

// looksLikeOrderingRequest detects a question asking about the sequence,
// count, or elapsed time between multiple distinct events — the shape
// that needs several different summaries recalled and combined, not just
// the single best match, and whose specific event names (e.g. "Spring
// Sprint Triathlon") often share no vocabulary with a generic question
// about "sports events" or "which came first."
func looksLikeOrderingRequest(query string) bool {
	lower := strings.ToLower(query)
	for _, kw := range orderingKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// Retrieve implements gateway.Retriever, delegating to retrieve for the
// actual read and logging exactly one audit_log row per call regardless
// of which of retrieve's return paths fired — see retrieve's doc comment
// for the retrieval logic itself.
func (s *Store) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message, now time.Time) (gateway.RetrievalResult, error) {
	result, err := s.retrieve(ctx, actingUser, workspace, messages, now)
	if err != nil {
		return result, err
	}

	// A separate transaction, not folded into retrieve's own (read-only)
	// ones: this is the first and only write Retrieve makes. Best-effort
	// — an audit-logging failure degrades observability, not the chat
	// turn itself, same availability-over-durability posture Capture
	// takes for the episode write (docs/GAP_CLOSURE_PLAN.md §4.3).
	auditErr := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventRetrieve,
			Actor:          actingUser.Owner,
			ActingScope:    actingUser,
			WorkspaceScope: workspace,
			Detail: map[string]any{
				"gate":      string(result.Gate),
				"ref_count": len(result.Refs),
			},
		})
	})
	if auditErr != nil {
		slog.Default().Error("audit log write failed for retrieval", "error", auditErr)
	}
	return result, nil
}

// retrieve is Store.Retrieve's actual implementation, split out so
// Retrieve can wrap it uniformly with audit logging (see Retrieve's doc
// comment). It always returns the fixed anchor (self_model +
// latest-summary pointer, see ARCHITECTURE.md § Retrieval Engine)
// regardless of gate outcome — the gate governs only the *searched*
// content layered on top of that anchor. A client wanting no memory at
// all, not even the anchor, uses the X-Hupi-Memory: off opt-out, which
// bypasses this method entirely (see gateway.Handler).
//
// actingUser and workspace are split per docs/TIER3_PLAN.md D3: self_model
// always anchors to actingUser (the caller's own private scope), so voice
// stays personal even inside a team's workspace; everything searched
// (entities, summaries, episodes) is scoped to workspace, which is the
// same value as actingUser for a private request, or a specific team's
// shared scope for one routed through HandleTeamChatCompletions.
//
// This runs in two separate dbscope-managed transactions, not one, split
// around the embedding call in the middle (docs/HARDENING_PLAN.md D3):
// never hold a Postgres transaction open across a network call to an
// external provider.
func (s *Store) retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message, now time.Time) (gateway.RetrievalResult, error) {
	var anchor string
	var anchorRefs []identity.Ref
	var anchorCitations []gateway.Citation
	var matchedEntities []entityMatch
	var entityLines []string

	query := lastUserMessage(messages)

	err := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		var err error
		anchor, anchorRefs, anchorCitations, err = s.buildAnchor(ctx, tx, actingUser, workspace)
		if err != nil {
			return fmt.Errorf("build anchor: %w", err)
		}
		if query == "" {
			return nil
		}
		matchedEntities, err = s.stage1EntityMatches(ctx, tx, workspace, query)
		if err != nil {
			return fmt.Errorf("stage1 entity scan: %w", err)
		}

		// Same answer-time-reasoning hard filter fusedSearchSummaries/
		// keywordSearchEpisodes already apply
		// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md) — real, confirmed
		// necessary here too: a directly name-matched entity is rendered
		// unconditionally, with no date awareness at all, and this exact
		// path (an entity literally named "AI conference (2024-01-15)")
		// kept asserting a temporally-wrong date in the Phase E
		// adversarial case even after the other two paths were fixed.
		// Uses each entity's own last_updated (schema/0001_init.sql) as
		// its date — the most recent day any fact about it was touched,
		// the same recency signal a summary's period or an episode's ts
		// already provide.
		//
		// Deliberately NOT backed off to the unfiltered set when
		// excluding would leave nothing, unlike the other two paths: the
		// real adversarial case that motivated this fix is exactly a
		// single stage-1 match whose only date is wrong, and a backoff
		// would restore precisely that match, defeating the fix for its
		// own primary case. This is a safe asymmetry, not an
		// inconsistency — stage-1 entity matches are a cheap, coarse
		// substring pre-check, not this call's main retrieval surface;
		// fusedSearchSummaries, keywordSearchEpisodes, and
		// vectorSearchEntities all still run afterward regardless and can
		// surface the same or related content through a more robust
		// signal than a bare name/slug substring hit.
		if tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now); hasTimeframe {
			kept := make([]entityMatch, 0, len(matchedEntities))
			for _, m := range matchedEntities {
				if periodsOverlap(m.lastUpdated, m.lastUpdated.AddDate(0, 0, 1), tfStart, tfEnd) {
					kept = append(kept, m)
				}
			}
			matchedEntities = kept
		}

		for _, m := range matchedEntities {
			dateLabel := ""
			if rel := relativeDateLabel(m.lastUpdated, now); rel != "" {
				dateLabel = fmt.Sprintf(", last updated %s (%s)", m.lastUpdated.Format("2006-01-02"), rel)
			}
			line, err := s.formatEntity(ctx, tx, workspace, m.id, dateLabel)
			if err != nil {
				return fmt.Errorf("load matched entity %s: %w", m.id, err)
			}
			entityLines = append(entityLines, line)
		}
		return nil
	})
	if err != nil {
		return gateway.RetrievalResult{}, fmt.Errorf("store: %w", err)
	}

	if query == "" {
		return gateway.RetrievalResult{Gate: gateway.GateSkipped, ContextMessage: anchor, Refs: anchorRefs, Citations: anchorCitations}, nil
	}

	hasSignal := stage1KeywordSignal(query) || stage1QuestionSignal(query)

	// Stage 1 found nothing at all: skipped, no search ever ran.
	if len(matchedEntities) == 0 && !hasSignal {
		return gateway.RetrievalResult{Gate: gateway.GateSkipped, ContextMessage: anchor, Refs: anchorRefs, Citations: anchorCitations}, nil
	}

	// Stage 2: something looked worth searching for.
	var sb strings.Builder
	sb.WriteString(anchor)

	refs := append([]identity.Ref{}, anchorRefs...)
	citations := append([]gateway.Citation{}, anchorCitations...)
	strongHit := false

	matchedEntityIDs := make([]string, len(matchedEntities))
	for i, m := range matchedEntities {
		matchedEntityIDs[i] = m.id
		sb.WriteString("\n" + entityLines[i])
		ref := identity.Ref{Kind: identity.RefKindEntity, Scope: workspace, ID: m.id}
		refs = append(refs, ref)
		citations = append(citations, gateway.Citation{Ref: ref, Snippet: entityLines[i]})
		strongHit = true // an exact entity-key match is always a strong hit
	}

	// Embed the query once and reuse it for both searches below — no
	// reason to pay for the same embedding call twice. Deliberately
	// outside any transaction: this is a network call to the embedding
	// provider.
	embedResp, err := s.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(query)}})
	if err != nil {
		return gateway.RetrievalResult{}, fmt.Errorf("store: embed query: %w", err)
	}
	if len(embedResp.Vectors) == 0 {
		return gateway.RetrievalResult{}, errors.New("store: embedder returned no vectors")
	}
	queryVector := pgfmt.VectorLiteral(embedResp.Vectors[0])

	queryTerms := tokenize(query)
	if len(queryTerms) == 0 && keywordSearchEnabled() {
		// Reaching here means stage 1 passed and the embedding call above
		// already ran — a real, paid cost — but every keyword-search call
		// site below gates on len(queryTerms) > 0 too, so none of them
		// will run this turn. Counted once here, not at each of those
		// call sites, so this metric reflects turns, not redundant
		// per-mechanism skips of the same underlying cause (review
		// finding B13).
		metrics.KeywordSearchSkippedNoTermsTotal.Inc()
	}

	// docs/LONGMEMEVAL_ACCURACY_PLAN.md category 1: a recommendation-
	// seeking question ("what should I bake for..." months after the
	// user last mentioned baking preferences) often shares no real
	// vocabulary with the preference statement it needs to recall —
	// standing facts like this live in entities, so widen entity
	// retrieval specifically for this query shape rather than lowering
	// the threshold for everyone.
	entitySimilarityThreshold := entityVectorSimilarityThreshold
	entityMaxResults := maxVectorResults()
	if looksLikeRecommendationRequest(query) {
		entitySimilarityThreshold = recommendationEntitySimilarityThreshold
		entityMaxResults = recommendationEntityMaxResults
	}

	summarySimilarityThreshold := vectorSimilarityThreshold
	summaryMaxResults := maxVectorResults()
	isOrderingQuery := looksLikeOrderingRequest(query)
	if isOrderingQuery {
		summarySimilarityThreshold = orderingSummarySimilarityThreshold
		summaryMaxResults = orderingSummaryMaxResults
	}

	err = dbscope.Run(ctx, s.db, workspace, workspace, func(tx *sql.Tx) error {
		// Per-scope keyword-search governance (internal/metrics'
		// KeywordSearchTierTotal is this decision's own observability) —
		// computed once per retrieve() call, not once per mechanism,
		// since all three keyword-search call sites below share the same
		// scope and should agree on the same tier for one turn.
		tier, err := keywordSearchTierForScope(ctx, tx, workspace)
		if err != nil {
			return fmt.Errorf("determine keyword search tier: %w", err)
		}
		metrics.KeywordSearchTierTotal.WithLabelValues(tierLabel(tier)).Inc()

		// Summaries fuse their vector and keyword rankings into one MMR
		// selection (docs/BENCHMARK_IMPROVEMENT_PLAN.md step 4) — unlike
		// episodes/entities below, which stay two independently-run
		// searches (vector picks first, keyword only adds what's left
		// over) for now. See fusedSearchSummaries' own doc comment for
		// why summaries specifically.
		//
		// docs/LONGMEMEVAL_ACCURACY_PLAN.md category 2 originally
		// threaded an explicit finalK/lambda through this call to widen
		// it for detected ordering/counting questions — reverted after
		// real verification showed zero effect, because that attempt
		// only widened the final-selection stage, not the vector fetch's
		// own similarity threshold (every candidate had vectorRank=-1 —
		// nothing to select from). This second attempt
		// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 2) widens
		// the threshold and fetch caps themselves, the same pattern
		// already proven for recommendationEntitySimilarityThreshold —
		// see orderingSummarySimilarityThreshold's own doc comment for
		// the real evidence this stage, not final-selection, was the
		// actual bottleneck.
		summaryRefs, err := s.fusedSearchSummaries(ctx, tx, workspace, queryVector, queryTerms, &sb, &strongHit, &citations, summarySimilarityThreshold, summaryMaxResults, query, now, tier, matchedEntityIDs)
		if err != nil {
			return fmt.Errorf("fused search summaries: %w", err)
		}
		refs = append(refs, summaryRefs...)

		episodeRefs, err := s.vectorSearchEpisodes(ctx, tx, workspace, queryVector, queryTerms, &sb, &strongHit, &citations, query, now)
		if err != nil {
			return fmt.Errorf("vector search episodes: %w", err)
		}
		refs = append(refs, episodeRefs...)

		entityRefs, err := s.vectorSearchEntities(ctx, tx, workspace, queryVector, matchedEntityIDs, &sb, &strongHit, &citations, entitySimilarityThreshold, entityMaxResults, query, now)
		if err != nil {
			return fmt.Errorf("vector search entities: %w", err)
		}
		refs = append(refs, entityRefs...)

		// Keyword (BM25) search runs after its vector counterpart, over
		// episodes/entities, excluding whatever vector search already
		// surfaced — the same "second chance, skip duplicates" shape
		// vectorSearchEntities already uses for stage 1's own matches.
		// Summaries are handled above instead, by fusedSearchSummaries.
		if len(queryTerms) > 0 && tier != keywordSearchDisabled {
			episodeKeywordRefs, err := s.keywordSearchEpisodes(ctx, tx, workspace, queryTerms, refIDsOfKind(refs, identity.RefKindEpisode), &sb, &strongHit, &citations, query, now)
			if err != nil {
				return fmt.Errorf("keyword search episodes: %w", err)
			}
			refs = append(refs, episodeKeywordRefs...)

			entityKeywordRefs, err := s.keywordSearchEntities(ctx, tx, workspace, queryTerms, refIDsOfKind(refs, identity.RefKindEntity), &sb, &strongHit, &citations, query, now)
			if err != nil {
				return fmt.Errorf("keyword search entities: %w", err)
			}
			refs = append(refs, entityKeywordRefs...)
		}

		// Graph walk runs last, seeded from every entity found by every
		// mechanism above (stage 1 exact match, vector search, keyword
		// search combined) — a relationship connects two entities
		// regardless of *how* one of them was found, so this needs the
		// full set, not just one search's own results.
		graphRefs, err := s.graphWalkRelationships(ctx, tx, workspace, refIDsOfKind(refs, identity.RefKindEntity), &sb, &strongHit, &citations)
		if err != nil {
			return fmt.Errorf("graph walk relationships: %w", err)
		}
		refs = append(refs, graphRefs...)
		return nil
	})
	if err != nil {
		return gateway.RetrievalResult{}, fmt.Errorf("store: %w", err)
	}

	gate := gateway.GatePartial
	if strongHit {
		gate = gateway.GateFull
	}

	return gateway.RetrievalResult{
		Gate:                 gate,
		ContextMessage:       truncateToBudget(sb.String(), contextCharBudget()),
		Refs:                 refs,
		Citations:            citations,
		NeedsAggregationPass: isOrderingQuery,
	}, nil
}

// buildAnchor loads the fixed, unconditional part of the context: the
// self_model entity, always from actingUser regardless of workspace
// (docs/TIER3_PLAN.md D3), and a one-line pointer — not the full text —
// to the latest daily summary *in workspace* (what's relevant to being in
// that workspace, private or a team's). q is expected to be a transaction
// with both scope-variable pairs already set (dbscope.SetSession) — this
// is the one place in this package that genuinely reads two different
// scopes in one call.
func (s *Store) buildAnchor(ctx context.Context, q dbscope.Querier, actingUser, workspace identity.Scope) (string, []identity.Ref, []gateway.Citation, error) {
	var sb strings.Builder
	var refs []identity.Ref
	var citations []gateway.Citation

	var selfID, attrs string
	var attrsCT []byte
	var keyVersion int
	err := q.QueryRowContext(ctx, `
		select id, attributes, key_version from entities
		where kind = 'self_model' and scope_kind = $1 and scope_owner = $2
		limit 1
	`, actingUser.Kind, actingUser.Owner).Scan(&selfID, &attrsCT, &keyVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no self_model configured yet — anchor is just the summary pointer
	case err != nil:
		return "", nil, nil, fmt.Errorf("load self_model: %w", err)
	default:
		actingEnc, keyErr := s.keys.GetVersion(ctx, actingUser, keyVersion)
		if keyErr != nil {
			return "", nil, nil, fmt.Errorf("resolve encryption key for self_model: %w", keyErr)
		}
		attrs, err = actingEnc.Decrypt(attrsCT)
		if err != nil {
			return "", nil, nil, fmt.Errorf("decrypt self_model attributes: %w", err)
		}
		sb.WriteString("self_model: " + attrs + "\n")
		ref := identity.Ref{Kind: identity.RefKindEntity, Scope: actingUser, ID: selfID}
		refs = append(refs, ref)
		citations = append(citations, gateway.Citation{Ref: ref, Snippet: "self_model: " + attrs})
	}

	var latestPeriod string
	err = q.QueryRowContext(ctx, `
		select period from summaries s
		where level = 'daily' and scope_kind = $1 and scope_owner = $2
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		order by period desc limit 1
	`, workspace.Kind, workspace.Owner).Scan(&latestPeriod)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// day one: no summaries exist yet, nothing to point at
	case err != nil:
		return "", nil, nil, fmt.Errorf("load latest daily summary period: %w", err)
	default:
		sb.WriteString("(latest daily summary: " + latestPeriod + ")\n")
	}

	return sb.String(), refs, citations, nil
}

// entityMatch is stage1EntityMatches' own result shape — id plus
// last_updated (schema/0001_init.sql), needed by retrieve()'s own
// timeframe hard filter and date-label injection
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md answer-time reasoning
// follow-up). Kept local to this concern rather than widened into a
// general "entity summary" struct other callers might expect more from.
type entityMatch struct {
	id          string
	lastUpdated time.Time
}

// stage1EntityMatches does a cheap, local (no LLM call) scan for known
// entity names/id-slugs appearing in the message, restricted to the given
// scope — the "known entity names" half of ARCHITECTURE.md's stage-1
// pre-check. Fetching the whole entity table per request is fine at
// personal-history scale; a higher-volume deployment should cache this
// list in memory and invalidate it on entity writes rather than query it
// on every turn.
func (s *Store) stage1EntityMatches(ctx context.Context, q dbscope.Querier, scope identity.Scope, query string) ([]entityMatch, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name, last_updated from entities
		where kind != 'self_model' and scope_kind = $1 and scope_owner = $2
	`, scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lowerQuery := strings.ToLower(query)
	var matches []entityMatch
	for rows.Next() {
		var id, name string
		var lastUpdated time.Time
		if err := rows.Scan(&id, &name, &lastUpdated); err != nil {
			return nil, err
		}
		if name != "" && strings.Contains(lowerQuery, strings.ToLower(name)) {
			matches = append(matches, entityMatch{id: id, lastUpdated: lastUpdated})
			continue
		}
		if slug := slugPart(id); slug != "" && strings.Contains(lowerQuery, strings.ToLower(slug)) {
			matches = append(matches, entityMatch{id: id, lastUpdated: lastUpdated})
		}
	}
	return matches, rows.Err()
}

// slugPart extracts the display part of an entity id, e.g.
// "project:hupi" -> "hupi".
func slugPart(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func stage1KeywordSignal(query string) bool {
	lower := strings.ToLower(query)
	for _, kw := range stage1SignalKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// stage1QuestionStarters backs stage1QuestionSignal — a bounded, common
// set of English interrogative/auxiliary words that open a question when
// the message isn't punctuated with a trailing "?". Includes the
// apostrophe-dropped contractions ("whats", "whos", ...) alongside their
// proper spellings — found via a real failure during live testing:
// "whats my project codename" (no apostrophe, a very ordinary way to
// type quickly) doesn't start with "what " and was missed until this was
// added, the same informal-spelling gap a fixed keyword list can't avoid
// without deliberately enumerating it.
var stage1QuestionStarters = []string{
	"what", "whats", "when", "whens", "where", "wheres",
	"who", "whos", "whom", "whose", "why", "whys", "how", "hows",
	"is", "are", "was", "were", "do", "does", "did",
	"can", "could", "should", "would", "will", "has", "have", "had",
}

// stage1QuestionSignal is stage1KeywordSignal's counterpart for
// interrogative-shaped messages, added after live testing found real
// recall questions with no stage1SignalKeywords phrase in them (e.g.
// "What do you remember about my project?" contains "remember" and
// passes; "What is my project's codename?" doesn't contain any fixed
// phrase and was silently skipped instead of searched).
// ARCHITECTURE.md's request-lifecycle table already describes stage 1 as
// catching "a question about something previously discussed" — this
// closes the gap between that documented intent and what the fixed
// keyword list actually caught. A trailing "?" or a leading
// interrogative/auxiliary word is treated as worth attempting a search
// for; vectorSimilarityThreshold and friends downstream are the actual
// precision gate (see their own measured-calibration comments, e.g.
// "write me a haiku about the ocean" at 0.0985 — well below threshold),
// so this only costs an occasional wasted embedding call on an
// unrelated question, never a wrong final answer.
func stage1QuestionSignal(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return false
	}
	if strings.HasSuffix(trimmed, "?") {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, w := range stage1QuestionStarters {
		if lower == w || strings.HasPrefix(lower, w+" ") {
			return true
		}
	}
	return false
}

func (s *Store) formatEntity(ctx context.Context, q dbscope.Querier, scope identity.Scope, id string, dateLabel string) (string, error) {
	var name string
	var attrsCT []byte
	var keyVersion int
	err := q.QueryRowContext(ctx, `
		select name, attributes, key_version from entities where scope_kind = $1 and scope_owner = $2 and id = $3
	`, scope.Kind, scope.Owner, id).Scan(&name, &attrsCT, &keyVersion)
	if err != nil {
		return "", err
	}
	enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return "", fmt.Errorf("resolve encryption key: %w", err)
	}
	attrs, err := enc.Decrypt(attrsCT)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("entity %s%s (%s): %s", id, dateLabel, name, attrs), nil
}

// vectorSearchSummaries searches the current (non-superseded) summaries
// within scope by pgvector cosine distance against an already-embedded
// query vector, appending matched content to sb and returning refs for
// the summaries used. This is the primary retrieval surface — summaries
// are what consolidation produces specifically to be searched (see
// ARCHITECTURE.md § Retrieval Engine).
//
// "Current" means no other summary's supersedes column points at this
// row's id — NOT "this row's own supersedes is null". A corrected
// summary (hupi-correct) is a brand-new row whose *own* supersedes
// column points backward at the row it replaces; the replaced row's
// supersedes stays null forever, since it never gets updated in place
// (internal/consolidation/store.go only ever inserts). Filtering on
// `supersedes is null` — the original, wrong version of this query —
// therefore returned exactly the superseded, pre-correction summaries
// and silently excluded every corrected one, discovered by actually
// running a correction end-to-end and watching the model answer with
// the stale, "corrected-away" fact instead of the fix.
// rrfK is Reciprocal Rank Fusion's smoothing constant. The original RRF
// literature's usual k=60 was tuned for TREC-scale rankings (hundreds of
// results) — at that scale a document's exact rank barely moves its
// score, which is the point (robustness to any one ranker's noise). This
// candidate pool never exceeds maxVectorResults()*summaryOverfetchFactor
// (a few dozen at most); k=60 would flatten a #1-ranked candidate and a
// #20-ranked one to nearly the same fused score, defeating the purpose
// of fusing rankings at all. k=1 preserves meaningful separation between
// ranks at this much smaller scale.
const rrfK = 1.0

func reciprocalRank(rank int) float64 {
	return 1.0 / (rrfK + float64(rank+1))
}

// fusedSearchSummaries replaces running vectorSearchSummaries and
// keywordSearchSummaries as two independent searches — one gets first
// pick of maxVectorResults() slots, the other only adds whatever wasn't
// already claimed — with a single fused ranking
// (docs/BENCHMARK_IMPROVEMENT_PLAN.md step 4): both mechanisms' own
// rankings feed into one Reciprocal Rank Fusion score per candidate, so
// a summary found by *both* (even at a modest rank in each) outranks one
// found strongly by only one — real, checkable evidence from two
// independent signals agreeing beats a single signal's own confidence.
// The same MMR diversity selection from step 3 (mmrSelect) applies on
// top of the fused score, not on raw vector similarity alone, so
// redundant summaries are penalized regardless of which mechanism found
// them.
//
// The keyword half still pays BM25's real, deliberate cost: BM25 over
// application-encrypted text (ARCHITECTURE.md § Storage security) has no
// index to search with, since Postgres's own full-text search machinery
// can't see through ciphertext, so it decrypts and scores every matching
// summary in scope on every call — fine at personal/team-history scale,
// genuinely bad if a scope's corpus ever grew past what a single query
// should fully decrypt. Not restricted to "embedding is not null" the
// way vector search is: a summary missing an embedding (an
// embedding-provider failure at creation time, say) shouldn't also be
// invisible to keyword search — a genuine, small coverage improvement
// vector search alone can't offer.
//
// bm25Score's own score cutoff (kept as "> 0", not a calibrated positive
// value) is measured, not guessed — the same way vectorSimilarityThreshold
// and friends were (a constructed set of real true-positive/near-miss/
// true-negative query-document pairs, scored with this package's actual
// bm25Score, not hand-estimated), against an 18-document corpus mixing
// several genuinely distinct topics:
//
//	11.10  true positive  — exact rare-term match, direct question
//	 6.91  true positive  — exact rare-term match, different phrasing
//	 5.29  true positive  — exact rare-term match, different topic
//	 5.19  near-miss      — same rare term, but the wrong specific
//	                        document (query asks about the job queue,
//	                        this document is about migrating a
//	                        different service to use it)
//	 3.39  true positive
//	 3.11  true positive
//	 2.55  near-miss      — a shared moderately-common word ("favorite"),
//	                        wrong topic entirely
//	 1.80  true positive
//	 1.29  near-miss      — query uses "async runtime", document says
//	                        "tokio" — BM25 can't bridge that gap (no
//	                        notion that the words are related), the one
//	                        remaining shared token is a rare name
//	 0.00  every true negative, with zero exceptions
//
// True positives and near-misses interleave throughout the positive
// range — the same finding vectorSimilarityThreshold's own comment
// documents for its metric, for the same underlying reason: a document
// sharing one real, rare token with the query scores comparably whether
// it's actually what's being asked about or just adjacent to it. The one
// real, clean gap in this entire measurement is between 0 and any
// positive score — every true negative landed at exactly 0, with no
// exceptions — which is exactly the cutoff already implemented. RRF's
// own rank-based fusion (this function, above) is what now actually
// separates a true positive from a same-topic near-miss in practice, on
// top of that cutoff. Re-measure if the tokenizer's stopword list or the
// BM25 k1/b constants ever change.
func (s *Store) fusedSearchSummaries(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, queryTerms []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation, similarityThreshold float64, maxResults int, query string, now time.Time, tier keywordSearchTier, matchedEntityIDs []string) ([]identity.Ref, error) {
	type candidate struct {
		id          string
		text        string
		period      string
		enc         *crypto.Encryptor
		vectorRank  int // -1 if not found by vector search
		keywordRank int // -1 if not found by keyword search
		factRank    int // -1 if not found by per-fact search
	}
	byID := make(map[string]*candidate)

	// Vector search: same overfetch + threshold vectorSearchSummaries
	// used before this refactor — see summaryOverfetchFactor's own doc
	// comment for why overfetching matters.
	//
	// embedding_model = $5 (or null, for rows predating that column —
	// see currentEmbeddingModel's own doc comment) excludes vectors from
	// a *different*, no-longer-active embedding model — docs/CODEBASE_SURVEY_AND_REVIEW.md
	// finding A3: without this, a cosine distance between a query vector
	// and a stored vector from a different model is closer to noise than
	// a real similarity signal, and pgvector's fixed column length means
	// nothing else catches the mismatch — a provider switch would
	// silently degrade retrieval until a full hupi-reembed completes,
	// with no error anywhere.
	vecRows, err := q.QueryContext(ctx, `
		select id, summary, key_version, period, (embedding <=> $1::vector) as distance
		from summaries s
		where embedding is not null
		  and (embedding_model is null or embedding_model = $5)
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		  and scope_kind = $2 and scope_owner = $3
		order by embedding <=> $1::vector
		limit $4
	`, queryVector, scope.Kind, scope.Owner, maxResults*summaryOverfetchFactor, s.currentEmbeddingModel())
	if err != nil {
		return nil, err
	}
	vecRank := 0
	for vecRows.Next() {
		var id string
		var summaryCT []byte
		var keyVersion int
		var period string
		var distance float64
		if err := vecRows.Scan(&id, &summaryCT, &keyVersion, &period, &distance); err != nil {
			vecRows.Close()
			return nil, err
		}
		// Cosine distance -> similarity for a normalized embedding space;
		// see vectorSimilarityThreshold's doc comment for the cutoff.
		similarity := 1 - distance
		if similarity < similarityThreshold {
			continue
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			vecRows.Close()
			return nil, fmt.Errorf("resolve encryption key for summary %s: %w", id, err)
		}
		text, err := enc.Decrypt(summaryCT)
		if err != nil {
			vecRows.Close()
			return nil, err
		}
		byID[id] = &candidate{id: id, text: text, period: period, enc: enc, vectorRank: vecRank, keywordRank: -1, factRank: -1}
		vecRank++
	}
	if err := vecRows.Err(); err != nil {
		vecRows.Close()
		return nil, err
	}
	// Fully drained and closed before the keyword query below opens a
	// second cursor on the same connection — q is often a
	// single-connection *sql.Tx; see the "driver: bad connection" note
	// this package's own existing tests already caught on this exact
	// gotcha.
	vecRows.Close()

	if tier != keywordSearchDisabled && len(queryTerms) > 0 {
		// keywordSearchNarrowed only narrows when stage 1 actually found an
		// entity to narrow by (keywordSearchTier's own doc comment) — with
		// no match, this falls through to the exact same unrestricted scan
		// keywordSearchFull runs, rather than silently dropping recall.
		narrow := tier == keywordSearchNarrowed && len(matchedEntityIDs) > 0
		kwQuery := `
			select id, summary, key_version, period
			from summaries s
			where summary is not null
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
			  and scope_kind = $1 and scope_owner = $2
		`
		kwArgs := []any{scope.Kind, scope.Owner}
		if narrow {
			kwQuery += " and entities_touched && $3::text[]"
			kwArgs = append(kwArgs, pgfmt.TextArray(matchedEntityIDs))
		}
		kwRows, err := q.QueryContext(ctx, kwQuery, kwArgs...)
		if err != nil {
			return nil, err
		}
		var docs []bm25Document
		kwText := make(map[string]string)
		kwEnc := make(map[string]*crypto.Encryptor)
		kwPeriod := make(map[string]string)
		for kwRows.Next() {
			var id string
			var summaryCT []byte
			var keyVersion int
			var period string
			if err := kwRows.Scan(&id, &summaryCT, &keyVersion, &period); err != nil {
				kwRows.Close()
				return nil, err
			}
			kwPeriod[id] = period
			// Already decrypted by the vector pass above — reuse it
			// rather than paying for a second decrypt of the same
			// ciphertext.
			if c, ok := byID[id]; ok {
				docs = append(docs, newBM25Document(id, c.text))
				continue
			}
			enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				kwRows.Close()
				return nil, fmt.Errorf("resolve encryption key for summary %s: %w", id, err)
			}
			text, err := enc.Decrypt(summaryCT)
			if err != nil {
				kwRows.Close()
				return nil, err
			}
			kwText[id] = text
			kwEnc[id] = enc
			docs = append(docs, newBM25Document(id, text))
		}
		if err := kwRows.Err(); err != nil {
			kwRows.Close()
			return nil, err
		}
		kwRows.Close()

		docFreq := bm25DocFrequency(docs)
		avgDocLen := bm25AverageDocLength(docs)
		type scored struct {
			id    string
			score float64
		}
		var matches []scored
		for _, doc := range docs {
			if score := bm25Score(doc, queryTerms, docFreq, len(docs), avgDocLen); score > 0 {
				matches = append(matches, scored{id: doc.id, score: score})
			}
		}
		// Tie-broken by id, not just score (review finding B10): the
		// underlying SQL has no ORDER BY, so Postgres makes no guarantee
		// about the row order docs itself arrives in across repeated runs
		// — without a deterministic secondary key, which tied-score
		// candidate survives kwCap below (and ultimately maxResults) could
		// vary run-to-run with identical data, a real reproducibility risk
		// for a project that leans heavily on exact before/after benchmark
		// comparisons. Breaking ties by id makes the final order fully
		// deterministic regardless of what order docs arrived in, without
		// needing to also add an ORDER BY to the query itself.
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].score != matches[j].score {
				return matches[i].score > matches[j].score
			}
			return matches[i].id < matches[j].id
		})
		kwCap := maxResults * summaryOverfetchFactor
		if len(matches) > kwCap {
			matches = matches[:kwCap]
		}
		for rank, m := range matches {
			if c, ok := byID[m.id]; ok {
				c.keywordRank = rank
				continue
			}
			byID[m.id] = &candidate{id: m.id, text: kwText[m.id], period: kwPeriod[m.id], enc: kwEnc[m.id], vectorRank: -1, keywordRank: rank, factRank: -1}
		}
	}

	// Per-fact search: a third fusion signal alongside the two above,
	// catching a summary whose *own* overall embedding/keyword profile
	// doesn't match the query at all, but which contains one specific,
	// correctly-grounded fact that does — a real, confirmed gap
	// (docs/LONGMEMEVAL_ACCURACY_PLAN.md's 852ce960 case: a mortgage
	// pre-approval amount buried in an otherwise unrelated day's summary
	// never entered the candidate pool, because fusedSearchSummaries
	// only ever scored whole summaries, and per-fact ranking
	// (loadKeyFacts/rankKeyFacts below) only ever runs on summaries that
	// already made the pool). grounded = true matches the same trust bar
	// loadKeyFacts itself applies — this never surfaces a fact retrieval
	// wouldn't otherwise be willing to show. The same similarityThreshold
	// cutoff as the vector half above is the real false-positive guard:
	// a summary only enters via this path when one of its facts is a
	// genuinely close match, not merely the closest of a bad lot.
	factRows, err := q.QueryContext(ctx, `
		select s.id, s.summary, s.key_version, s.period, min(f.embedding <=> $1::vector) as best_distance
		from summary_key_facts f
		join summaries s on s.id = f.summary_id
		where f.embedding is not null
		  and (f.embedding_model is null or f.embedding_model = $5)
		  and f.grounded = true
		  and f.scope_kind = $2 and f.scope_owner = $3
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		group by s.id
		order by best_distance
		limit $4
	`, queryVector, scope.Kind, scope.Owner, maxResults*summaryOverfetchFactor, s.currentEmbeddingModel())
	if err != nil {
		return nil, err
	}
	factRank := 0
	for factRows.Next() {
		var id string
		var summaryCT []byte
		var keyVersion int
		var period string
		var distance float64
		if err := factRows.Scan(&id, &summaryCT, &keyVersion, &period, &distance); err != nil {
			factRows.Close()
			return nil, err
		}
		similarity := 1 - distance
		if similarity < similarityThreshold {
			continue
		}
		if c, ok := byID[id]; ok {
			c.factRank = factRank
			factRank++
			continue
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			factRows.Close()
			return nil, fmt.Errorf("resolve encryption key for summary %s: %w", id, err)
		}
		text, err := enc.Decrypt(summaryCT)
		if err != nil {
			factRows.Close()
			return nil, err
		}
		byID[id] = &candidate{id: id, text: text, period: period, enc: enc, vectorRank: -1, keywordRank: -1, factRank: factRank}
		factRank++
	}
	if err := factRows.Err(); err != nil {
		factRows.Close()
		return nil, err
	}
	factRows.Close()

	if len(byID) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	// Deterministic order before mmrSelect's own greedy tie-breaking —
	// map iteration order is randomized in Go, and a tie shouldn't
	// depend on that.
	sort.Strings(ids)

	// Resolved once for the whole call, not per candidate — Phase E
	// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): real-verified need, a
	// synthetic adversarial test confirmed a temporally-wrong but
	// lexically-closer summary (fused=1.0) outranking the temporally-
	// correct one (fused=0.667) for a query implying "last month," with
	// nothing in the existing ranking aware of either summary's own
	// period at all.
	tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now)

	// poolIDs is ids, filtered to only the candidates whose period
	// overlaps a confidently-resolved timeframe — the answer-time
	// reasoning follow-up to Phase E's own boost
	// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): real-verified that the
	// boost alone wasn't enough. Even after it correctly raised the
	// temporally-right candidate's fused score *and* the mmrSelect
	// ordering fix correctly put it first in context, the answering
	// model still picked the temporally-wrong one on pure lexical match
	// ("AI conference" literally in the wrong summary's text). A
	// same-topic distractor sitting right next to the correct answer in
	// context is exactly the shape of error a ranking boost can't fully
	// prevent — removing it from context entirely is the stronger fix.
	//
	// Excluded rather than merely deprioritized only for candidates with
	// a *parseable* period that provably doesn't overlap — a candidate
	// with no parseable period is kept (can't judge it, and Phase C/D's
	// own precedent throughout this codebase is to never destroy
	// information on an unclear signal). And if excluding would leave
	// nothing at all (e.g. consolidation simply never ran for the
	// implied period), the filter backs off entirely rather than
	// returning an empty context — the same safe-degrade direction
	// groundingCheck's own count-mismatch handling already uses.
	poolIDs := ids
	excludedByTimeframe := make(map[string]bool)
	if hasTimeframe {
		kept := make([]string, 0, len(ids))
		for _, id := range ids {
			c := byID[id]
			if pStart, pEnd, ok := parsePeriodRange(c.period); ok && !periodsOverlap(pStart, pEnd, tfStart, tfEnd) {
				excludedByTimeframe[id] = true
				continue
			}
			kept = append(kept, id)
		}
		if len(kept) > 0 {
			poolIDs = kept
		} else {
			excludedByTimeframe = make(map[string]bool)
		}
	}

	pool := make([]mmrCandidate, len(poolIDs))
	for i, id := range poolIDs {
		c := byID[id]
		var fused float64
		if c.vectorRank >= 0 {
			fused += reciprocalRank(c.vectorRank)
		}
		if c.keywordRank >= 0 {
			fused += reciprocalRank(c.keywordRank)
		}
		if c.factRank >= 0 {
			fused += reciprocalRank(c.factRank)
		}
		if hasTimeframe {
			if pStart, pEnd, ok := parsePeriodRange(c.period); ok && periodsOverlap(pStart, pEnd, tfStart, tfEnd) {
				// A full reciprocal-rank-0 contribution's worth of boost
				// (comparable to being the single best vector or keyword
				// match) — real-verified strong enough to flip the exact
				// adversarial case above, without being an unconditional
				// override: a candidate with a much stronger textual
				// match can still win if its own fused score clears this
				// margin some other way. Still applied on top of the
				// hard filter above (redundant once every remaining
				// candidate already overlaps, but harmless, and keeps
				// this branch correct on its own if the filter above
				// ever changes).
				fused += temporalRelevanceBoost
			}
		}
		pool[i] = mmrCandidate{relevance: fused, tokens: tokenSet(c.text)}
	}
	picked := mmrSelect(pool, maxResults, mmrLambda())

	// HUPI_DEBUG_FUSION is a real, permanent diagnostic escape hatch, not
	// throwaway debug code -- added while investigating a real multi-hop
	// regression (docs/BENCHMARK_IMPROVEMENT_PLAN.md, full-scale
	// re-verification section) precisely because there was previously no
	// way to see each candidate's own vector/keyword rank and fused
	// score, only the final assembled context. Off by default, zero
	// cost when unset, same pattern as every other env-var override in
	// this file.
	if os.Getenv("HUPI_DEBUG_FUSION") != "" {
		pickedSet := make(map[int]bool, len(picked))
		for _, idx := range picked {
			pickedSet[idx] = true
		}
		poolIndex := make(map[string]int, len(poolIDs))
		for i, id := range poolIDs {
			poolIndex[id] = i
		}
		for _, id := range ids {
			if excludedByTimeframe[id] {
				fmt.Fprintf(os.Stderr, "FUSION_DEBUG id=%s vectorRank=%d keywordRank=%d factRank=%d fused=excluded(timeframe) picked=false\n",
					id, byID[id].vectorRank, byID[id].keywordRank, byID[id].factRank)
				continue
			}
			i := poolIndex[id]
			fmt.Fprintf(os.Stderr, "FUSION_DEBUG id=%s vectorRank=%d keywordRank=%d factRank=%d fused=%.4f picked=%v\n",
				id, byID[id].vectorRank, byID[id].keywordRank, byID[id].factRank, pool[i].relevance, pickedSet[i])
		}
	}

	type pickedSummary struct {
		c     *candidate
		label string
		facts []keyFact
	}
	picks := make([]pickedSummary, 0, len(picked))
	for _, idx := range picked {
		c := byID[poolIDs[idx]]
		// Real, deliberate observability, same reasoning as the
		// graph-walk match marker: a candidate two independent signals
		// agree on is worth being able to tell apart from one only a
		// single signal found.
		label := ""
		switch {
		case c.vectorRank >= 0 && c.keywordRank >= 0:
			label = ", vector+keyword match"
		case c.keywordRank >= 0:
			label = ", keyword match"
		}
		facts, err := loadKeyFacts(ctx, q, c.id, c.enc, queryVector, s.currentEmbeddingModel())
		if err != nil {
			return nil, err
		}
		picks = append(picks, pickedSummary{c: c, label: label, facts: facts})
	}

	if os.Getenv("HUPI_DEBUG_FUSION") != "" {
		for _, p := range picks {
			order, best := rankKeyFacts(p.facts, queryTerms)
			mode := "lexical"
			if len(order) > 0 && p.facts[order[0]].similarity.Valid {
				mode = "semantic"
			}
			top := order
			if len(top) > 5 {
				top = top[:5]
			}
			fmt.Fprintf(os.Stderr, "FACTRANK_DEBUG summary=%s mode=%s n=%d top=%v guaranteed=%d\n", p.c.id, mode, len(p.facts), top, best)
		}
	}

	// Two real passes over picks, not one interleaved loop — see
	// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 1. An
	// earlier, interleaved version (guaranteed content + that same
	// summary's own depth, then the next summary's guaranteed content +
	// depth, and so on) still let one busy summary's depth section push
	// a *later* summary's guarantee past the truncation point once
	// several summaries were picked — real-verified against the
	// charity-events LongMemEval case. Writing every picked summary's
	// guarantee first, before any summary's depth, means a global
	// truncateToBudget cut (still the final backstop) can only ever
	// land on depth, never on a guarantee that hasn't been written yet
	// — never land on one that's already been written, that is: see
	// guaranteeBudgetPerSummary's own doc comment for a second, real
	// regression this same "write every guarantee first" design still
	// had, now also fixed — several picks' guarantee lines, with no
	// per-summary cap, could together exhaust the budget before a
	// lower-ranked pick's own guarantee was ever reached.
	//
	// Pass 2 doesn't repeat "related memory (summary %s...)" — just the
	// depth content itself — so each summary's id still appears exactly
	// once in the assembled context
	// (TestRetrieve_FusedSearchLabelsSummaryFoundByBothMechanisms).
	//
	// perSummaryGuaranteeCap bounds each pick's own guarantee line to a
	// fair share of the budget (guaranteeBudgetPerSummary's own doc
	// comment has the real regression this fixes) — computed once per
	// call from how many summaries were actually picked, not a flat
	// constant, since "fair" depends on how many are competing for room.
	perSummaryGuaranteeCap := guaranteeBudgetPerSummary(len(picks))
	for _, p := range picks {
		// dateLabel hands the answering model an already-computed
		// relative date, not just a raw one (docs/CONSOLIDATION_COMPLETENESS_PLAN.md
		// answer-time reasoning follow-up) — real-verified that even with
		// the correct fact and its date both present in context, the
		// model can still get date arithmetic wrong (e.g. "9 weeks ago"
		// against a gold "3 weeks ago" for a 20-day gap). Computing it
		// here removes that arithmetic from the model's task entirely.
		dateLabel := ""
		if pStart, _, ok := parsePeriodRange(p.c.period); ok {
			if rel := relativeDateLabel(pStart, now); rel != "" {
				dateLabel = fmt.Sprintf(", dated %s (%s)", p.c.period, rel)
			}
		}
		// factCap reserves whatever the prefix actually costs for *this*
		// summary (its own id/date-label/match-type length all vary) out
		// of the shared per-summary line cap, then guarantees at least
		// guaranteeMinFactChars for the fact itself regardless — see
		// guaranteeBudgetPerSummary's own doc comment for why capping
		// only the fact text, and ignoring real prefix cost, was the bug
		// in this fix's first version. Uses hardTruncate, not
		// truncateToBudget — a second real miscalculation found during
		// re-verification: truncateToBudget's own "...[truncated to fit
		// context budget]" marker (37 chars) was itself being added once
		// per picked summary, and with 9 real picks that overhead alone
		// was enough to still exceed the budget before the lowest-ranked
		// one was reached, even after this cap. That marker earns its
		// cost once, at the final whole-context cut, where it tells the
		// model there was more it isn't seeing — repeating it on every
		// individual guarantee line adds the same cost 9 times over for
		// an expected, minor per-line shortening, not a meaningful signal.
		prefix := fmt.Sprintf("\nrelated memory (summary %s%s%s): ", p.c.id, dateLabel, p.label)
		factCap := perSummaryGuaranteeCap - len(prefix)
		if factCap < guaranteeMinFactChars {
			factCap = guaranteeMinFactChars
		}
		guarantee := hardTruncate(guaranteedFact(p.c.text, p.facts, queryTerms), factCap)
		sb.WriteString(prefix + guarantee)
	}

	var refs []identity.Ref
	for _, p := range picks {
		// summaryDepthCap bounds the whole depth block's length, not its
		// content — depthText itself still builds the complete picture
		// (TestDepthTextIncludesProseAndAllFacts), capped here, at the
		// write site, the same way pass 1 caps guaranteedFact's result
		// rather than capping inside it. See summaryDepthCap's own doc
		// comment for the real regression this exists to fix.
		sb.WriteString("\n" + hardTruncate(depthText(p.c.text, p.facts, queryTerms), summaryDepthCap))
		refs = append(refs, identity.Ref{Kind: identity.RefKindSummary, Scope: scope, ID: p.c.id})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindSummary, Scope: scope, ID: p.c.id},
			Snippet: summaryCitationSnippet(p.c.text, p.facts, queryTerms),
		})
		*strongHit = true
	}
	return refs, nil
}

// appendKeyFacts writes a summary's grounded key_facts (see
// schema/0001_init.sql's summary_key_facts table) after its prose in sb,
// one per line — found necessary by tracing real cmd/hupi-bench QA
// misses against a real cloud model: consolidation already extracts and
// grounds specific, checkable facts per summary (a daily/weekly prose
// summary is inherently lossy, dropping concrete details like an exact
// location or date in favor of a thematic narrative), and stores them in
// summary_key_facts — but until this fix, nothing at retrieval time ever
// read that table back. A question whose answer was a specific fact
// (e.g. "where has Melanie camped?" -> "beach, mountains, forest")
// consistently failed even when the *correct* summary was retrieved,
// because the summary's own prose paraphrased the underlying facts away
// ("family camping trips") without the specifics the question asked for.
// Only grounded facts are surfaced — an ungrounded one already failed
// groundingCheck's own re-verification against the source text and
// shouldn't be presented as reliable.
//
// docs/BENCHMARK_IMPROVEMENT_PLAN.md step 6: originally wrote every
// grounded fact in plain insertion order, no query-awareness at all —
// generation defects (GF/GRF) stayed high across every EvalMem run this
// session even after the answer-style prompt fix and a larger context
// budget, consistent with the right fact sometimes genuinely being
// present but buried a few bullets down in a summary with several
// key_facts, unordered by relevance, for the model to find on its own.
// When one fact clearly shares more query vocabulary than the others (a
// real signal, not every summary's facts are about equally
// (ir)relevant), it's promoted to the front and marked
// "(most relevant)" — real, deliberate emphasis, the same reasoning as
// the graph-walk/fusion match markers. Left in original order,
// unmarked, whenever nothing actually stands out (every fact scores 0,
// or ties for the top score) — a fabricated "most relevant" label on an
// arbitrary pick would be worse than no reordering at all.
// loadKeyFacts is appendKeyFacts' original DB-loading half, split out
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 1) so
// fusedSearchSummaries can decide what to do with a summary's facts —
// guarantee one, then bound the rest — before anything gets written to
// sb, instead of appendKeyFacts writing everything the moment it's
// loaded.
// keyFact is one summary_key_facts row as loaded for ranking: the
// decrypted fact text plus, when schema/0021's embedding column is
// populated under the currently-active embedding model, its cosine
// similarity to this retrieval's query embedding. similarity is invalid
// (Valid == false) for a fact that was never embedded, or embedded under
// a since-changed model — rankKeyFacts treats that as "no semantic signal
// for this fact" and falls back to lexical scoring for the whole summary
// rather than mixing two incomparable scales (see rankKeyFacts' own doc
// comment).
type keyFact struct {
	text       string
	similarity sql.NullFloat64
}

// loadKeyFacts loads a summary's grounded facts along with each one's
// cosine similarity to the query, computed in SQL (`1 - (embedding <=>
// $queryVector)`) so Go never has to parse a 1536-float vector just to
// rank a handful of facts — the same division of labor
// vectorSearchEntities already uses. similarity is NULL whenever a fact
// has no embedding yet (not reembedded since schema/0021, or the
// consolidation-time embed call failed — see
// internal/consolidation/store.go's embedKeyFacts, best-effort by
// design) or was embedded under a different model than the one currently
// active (embedding_model != $3 — mixing vectors across models produces
// noise, not a real signal, same reasoning as summaries/entities).
func loadKeyFacts(ctx context.Context, q dbscope.Querier, summaryID string, enc *crypto.Encryptor, queryVector string, embeddingModel string) ([]keyFact, error) {
	rows, err := q.QueryContext(ctx, `
		select fact,
		       case when embedding is not null and embedding_model = $3
		            then 1 - (embedding <=> $2::vector) end as similarity
		from summary_key_facts
		where summary_id = $1 and grounded = true
		order by id
	`, summaryID, queryVector, embeddingModel)
	if err != nil {
		return nil, fmt.Errorf("load key facts for summary %s: %w", summaryID, err)
	}
	defer rows.Close()

	var facts []keyFact
	for rows.Next() {
		var factCT []byte
		var similarity sql.NullFloat64
		if err := rows.Scan(&factCT, &similarity); err != nil {
			return nil, err
		}
		fact, err := enc.Decrypt(factCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt key fact for summary %s: %w", summaryID, err)
		}
		facts = append(facts, keyFact{text: fact, similarity: similarity})
	}
	return facts, rows.Err()
}

// keyFactTexts extracts plain fact text, for the lexical scoring
// functions (factScores/rankFactsByRelevance/mostRelevantFactIndex) that
// predate per-fact embeddings and operate on []string — kept as the
// always-available fallback/tie-breaker rather than rewritten to take
// []keyFact directly.
func keyFactTexts(facts []keyFact) []string {
	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.text
	}
	return texts
}

// semanticFactRankingEnabled is an opt-out kill switch, same pattern as
// keywordSearchEnabled — lets the semantic path be A/B tested or disabled
// on an existing binary without a rebuild.
func semanticFactRankingEnabled() bool {
	return os.Getenv("HUPI_ENABLE_SEMANTIC_FACT_RANKING") != "false"
}

// factMarkerFusedMargin is the minimum fused-RRF-score gap over the
// runner-up required before rankKeyFacts marks a fact "(most relevant)"
// rather than just ranking it first — a reasoned starting value in the
// same RRF units reciprocalRank produces (not yet calibrated against real
// fused-score distributions the way this file's other thresholds have
// been), flagged here for that future calibration. Mirrors
// mostRelevantFactIndex's own refusal to fabricate a "most relevant"
// label on a near-tie.
const factMarkerFusedMargin = 0.1

// rankKeyFacts orders a summary's facts for both writeKeyFacts (every
// fact, most relevant first) and guaranteedFact/summaryCitationSnippet
// (just the top one) — one ranking shared by all three call sites, the
// same "single scoring definition" invariant factScores already
// establishes for the lexical path.
//
// Semantic ranking (by cosine similarity to the query) is considered only
// when *every* fact in this summary has a valid similarity — a mixed
// state should only be transient in practice (consolidation embeds a
// summary's facts in one batched call; see internal/reembed for
// backfilling pre-schema/0021 rows), and cosine similarity and integer
// word-overlap counts aren't on the same scale, so ranking embedded and
// unembedded facts directly against each other would be meaningless.
//
// Fuses the lexical rank and the semantic rank via Reciprocal Rank Fusion
// (the same mechanism fusedSearchSummaries already uses to combine vector
// and keyword search at the summary level — see reciprocalRank/rrfK)
// rather than letting semantic ranking override lexical outright. A first
// version did exactly that (semantic primary, lexical only a tie-break on
// a near-identical score) and real-verified *worse* on a real case this
// fix was built to help:
//
//   - Real motivating case (gpt4_45189cb4, "what is the order of the
//     sports events I watched in January", a 123-grounded-fact busy-day
//     summary): the correct fact ("watched the Chiefs defeat the
//     Bills... NFL playoffs") and an unrelated climate-change fact both
//     scored exactly one shared lexical term ("watched" vs. "events"
//     respectively) under factScores — a tie that rankFactsByRelevance's
//     stable sort broke in favor of whichever fact happened to be
//     extracted first, the wrong one. Direct cosine-similarity
//     measurement ranked the real fact #1 of 123 for this query.
//   - Real regression the semantic-primary version introduced, caught by
//     live end-to-end re-verification (not assumed): the *same* question,
//     a *different* picked summary (5 facts, nowhere near busy-day
//     scale). factScores correctly, unambiguously ranks "the user and
//     their dad watched the College Football National Championship
//     game..." first (the only fact sharing "watched" with the query —
//     no tie at all here). But direct embedding measurement showed
//     text-embedding-3-small itself scores an unrelated fact ("planned to
//     check out The Witcher and The Mandalorian TV shows") *higher*
//     (0.293 vs. 0.218) — a real embedding-model false positive, not a
//     bug in this ranking code. Semantic-overrides-lexical let that
//     single noisy embedding comparison discard a clean, unambiguous
//     lexical signal, and the championship fact was truncated out of the
//     final context as a result — fixing the original miss by
//     introducing a new one on the same question.
//
// RRF fuses both signals instead of either one unilaterally winning: a
// fact with no real lexical signal (tied at the bottom with dozens of
// others, the original bug's shape) is rescued by a strong semantic rank,
// but a fact with a clean, unambiguous top lexical rank isn't casually
// outvoted by one noisy embedding comparison the way raw similarity
// comparison allowed. Reuses rrfK as-is for now (tuned for
// fusedSearchSummaries' smaller, few-dozen-candidate summary pool, not
// yet separately calibrated for fact pools that can run into the
// hundreds) — flagged here, like this file's other reasoned-but-not-yet-
// calibrated constants, for future tuning rather than re-derived from
// scratch in this pass.
//
// Falls back to the existing lexical order whenever semantic ranking
// isn't available (kill switch off, no facts, or any fact missing a
// valid similarity) — byte-identical to this file's pre-embedding
// behavior, which is exactly what keeps every lexical-only test in
// keyfacts_test.go passing unchanged.
func rankKeyFacts(facts []keyFact, queryTerms []string) (order []int, bestIdx int) {
	texts := keyFactTexts(facts)
	lexOrder := rankFactsByRelevance(texts, queryTerms)
	lexBest := mostRelevantFactIndex(texts, queryTerms)

	if !semanticFactRankingEnabled() || len(facts) == 0 {
		return lexOrder, lexBest
	}
	for _, f := range facts {
		if !f.similarity.Valid {
			return lexOrder, lexBest
		}
	}

	lexRank := make([]int, len(facts))
	for rank, idx := range lexOrder {
		lexRank[idx] = rank
	}
	semOrder := make([]int, len(facts))
	for i := range semOrder {
		semOrder[i] = i
	}
	sort.SliceStable(semOrder, func(a, b int) bool {
		return facts[semOrder[a]].similarity.Float64 > facts[semOrder[b]].similarity.Float64
	})
	semRank := make([]int, len(facts))
	for rank, idx := range semOrder {
		semRank[idx] = rank
	}

	fused := make([]float64, len(facts))
	for i := range facts {
		fused[i] = reciprocalRank(lexRank[i]) + reciprocalRank(semRank[i])
	}
	order = make([]int, len(facts))
	for i := range order {
		order[i] = i
	}
	// Stable on ties, same as rankFactsByRelevance — falls through to
	// original insertion order rather than an arbitrary one.
	sort.SliceStable(order, func(a, b int) bool { return fused[order[a]] > fused[order[b]] })

	best := -1
	if len(order) >= 2 && fused[order[0]]-fused[order[1]] >= factMarkerFusedMargin {
		best = order[0]
	}
	return order, best
}

// writeKeyFacts is appendKeyFacts' original writing half — one bullet
// per fact, in descending relevance order (rankFactsByRelevance), the
// most query-relevant one (if any clearly stands out) additionally
// marked. Real, measured fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
// this used to write facts in raw insertion order beyond the single
// promoted winner — fine for a normal busy day's worth of facts, but a
// real 112-fact day (41 episodes clustered into one summary) buried the
// one fact that actually answered the question under a hundred unrelated
// ones, with no way to find it short of reading the entire list.
// Ordering by relevance means it isn't just "marked if it happens to be
// the single standout" — it's near the front regardless, and the
// eventual truncateToBudget backstop (for a summary big enough to still
// need one even after this) lands on the least relevant tail instead of
// whatever cluster-merge order happened to put last.
func writeKeyFacts(sb *strings.Builder, facts []keyFact, queryTerms []string) {
	order, best := rankKeyFacts(facts, queryTerms)
	for _, idx := range order {
		if idx == best {
			sb.WriteString(fmt.Sprintf("\n  - (most relevant) %s", facts[idx].text))
		} else {
			sb.WriteString(fmt.Sprintf("\n  - %s", facts[idx].text))
		}
	}
}

// guaranteedFactMaxCount bounds how many facts get unconditionally
// guaranteed together, ahead of picking just one — real, confirmed fix
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md, Phase E adversarial case):
// when a summary has only a couple of key facts, choosing just one to
// guarantee is a real lottery, not a safe simplification. A real
// consolidation run split "attended a robotics event" and "actuators and
// control systems were featured" into two separate facts instead of
// one combined fact (real, observed non-determinism in how consolidation
// phrases things run to run); with no query-vocabulary overlap to break
// the tie, guaranteedFact fell back to facts[0] — the bare "attended an
// event" fragment, with the one fact carrying the actual answer left in
// the depth section alone. Measured directly: at temperature 0 (the
// model's own most-confident completion), the model answered "not
// mentioned" 9 times out of 10 with only the bare fragment guaranteed,
// vs. 10 times out of 10 correct once both facts were guaranteed
// together. Left as a real ranking pick (not "guarantee everything")
// once a summary has more facts than this — depthText's own uncapped
// facts are what already protects a busy summary from real truncation,
// and guaranteeing dozens of facts unconditionally would defeat Phase D
// item 1's whole point.
const guaranteedFactMaxCount = 3

// guaranteedFact returns the piece of text worth protecting for a
// summary ahead of every picked summary's depth section (see
// fusedSearchSummaries' two-pass doc comment for why a guarantee, ahead
// of any depth, is needed at all). Below guaranteedFactMaxCount, every
// fact is guaranteed together — see that constant's own doc comment for
// why picking just one is a real lottery at that scale. At or above it,
// prefers the single highest-scoring fact
// (rankFactsByRelevance(facts, queryTerms)[0]); falls back to a short
// prose snippet only when the summary has no grounded key facts at all
// (rare — see loadKeyFacts/writeKeyFacts' own doc comment on why only
// grounded facts are ever surfaced).
//
// Used to call mostRelevantFactIndex directly instead, falling back to
// facts[0] whenever it returned -1 — real-verified against a genuine
// 41-episode/112-fact busy day (the same investigation that produced
// rankFactsByRelevance for writeKeyFacts) that this was a real, distinct
// bug: with that many candidates, several facts legitimately tie at the
// top score (e.g. two facts both sharing one query term, everything
// else scoring 0), and mostRelevantFactIndex's winner-take-all tie-break
// treats that exactly like "no signal at all," discarding the ranking
// and falling back to whatever fact happened to be extracted first —
// which, on the real busy day this was found against, was an entirely
// unrelated tire-pressure fact guaranteed into context ahead of the
// NFL-playoffs fact the question actually needed, with that correct
// fact left stranded in depthText's own lower-priority section where a
// tight budget cut it away entirely. rankFactsByRelevance never gives up
// this way — ties still produce a real top-scorer (stable on further
// ties), and the true "nothing matches" case (every fact scores 0)
// already degrades to the original facts[0], identical to the old
// fallback — so this is a strict improvement, not a behavior change for
// the cases that already worked.
//
// Lexical-overlap scoring alone still isn't perfect — real-verified
// against the charity-events LongMemEval case that it can promote the
// wrong fact on a summary with many candidates, the same real limitation
// that sank the preference-ranking "1b" attempt elsewhere in this file.
// rankKeyFacts now ranks by cosine similarity instead, when every fact in
// the summary has a valid embedding (see its own doc comment) — this is
// exactly why that fallback exists, not a hypothetical; depthText's own
// (uncapped) facts remain real insurance beyond this one guaranteed pick
// either way, for the cases where semantic ranking isn't available.
func guaranteedFact(prose string, facts []keyFact, queryTerms []string) string {
	if len(facts) == 0 {
		return truncateToBudget(prose, guaranteedProseFallbackChars)
	}
	if len(facts) <= guaranteedFactMaxCount {
		return strings.Join(keyFactTexts(facts), " ")
	}
	order, _ := rankKeyFacts(facts, queryTerms)
	return facts[order[0]].text
}

// guaranteedProseFallbackChars caps guaranteedFact's no-facts fallback —
// small on purpose, a fallback for a summary with nothing grounded to
// guarantee, not meant to substitute for real prose depth.
const guaranteedProseFallbackChars = 300

// guaranteeSectionFraction/guaranteeMinPerSummary bound how much of the
// total context budget a single picked summary's guarantee line
// (fusedSearchSummaries' pass 1, written before any pass-2 depth) is
// allowed to cost — a real, confirmed production regression, not a
// hypothetical. Pass 1's own doc comment explains why a guarantee is
// written before any depth: so the final truncateToBudget backstop can
// only ever land on depth, never on a guarantee. That protection has a
// real blind spot once *several* summaries are picked for the same
// question and nothing caps each one's share: with no per-summary cap,
// the guarantee lines written first (highest relevance first) can,
// together, consume the *entire* budget before a lower-ranked-but-still-
// correctly-matched summary's own guarantee is ever reached — not cut
// off mid-line by truncateToBudget, simply never written in time to
// survive it.
//
// Real-verified against gpt4_e072b769 ("how many weeks ago did I start
// using Ibotta") — a real regression introduced by guaranteedFact's own
// fix (above): 9 summaries matched this generic query, the Ibotta
// summary's fused score was the lowest of the 9, and its guarantee line
// sat past the real production-default 2000-char budget. Confirmed via
// a temporary budget override that the fact was present and correct the
// whole time ("3 weeks ago") — just unreachable in write order, not
// missing. guaranteedFact's fix (picking a real relevant fact on a tie
// instead of an arbitrary one) made this worse without changing the
// underlying architecture: a genuinely relevant fact is sometimes longer
// text than the arbitrary facts[0] fallback it replaced, which shifted
// how many characters several higher-ranked summaries' guarantee lines
// cost, enough to push Ibotta's past the cutoff where it used to just
// barely fit.
//
// The fix: cap each picked summary's whole guarantee *line* — not just
// its fact text — to a fair share of the budget, computed from how many
// summaries were actually picked. With few picks, the share is generous
// — most real facts (short, atomic, per summarySystemPrompt's own
// instruction) are well under it, so this changes nothing for the
// common case. With many picks, each gets a smaller but *guaranteed*
// slice instead of a first-come-first-served race for the whole budget.
//
// A first version of this fix capped only the fact text returned by
// guaranteedFact, not the "related memory (summary <id>, dated ...,
// ... match): " prefix wrapped around it in fusedSearchSummaries' own
// write loop — and real re-verification showed it didn't actually fix
// the Ibotta regression at all. Summary IDs in this real test data run
// ~130-160 characters on their own; 9 picks' worth of *prefixes alone*
// already exceeded the 2000-char default budget before any fact text
// was even considered, so capping only the fact text left the real
// bottleneck untouched. guaranteeMinPerSummary is sized to comfortably
// cover a realistic prefix (measured against this real data) plus a
// real short fact, not reverse-engineered to one specific ID format —
// the write loop separately reserves guaranteeMinFactChars for the fact
// itself regardless of how long a given summary's own prefix happens to
// be, so an unusually long ID in some other deployment can't silently
// crowd the fact out to zero the same way.
const guaranteeSectionFraction = 0.5
const guaranteeMinPerSummary = 220
const guaranteeMinFactChars = 60

// guaranteeBudgetPerSummary computes the per-summary *line* cap
// described above (prefix + fact together). Pure arithmetic — see that
// constant's doc comment for the real regression this exists to fix.
func guaranteeBudgetPerSummary(numPicks int) int {
	if numPicks <= 0 {
		return 0
	}
	share := int(float64(contextCharBudget())*guaranteeSectionFraction) / numPicks
	if share < guaranteeMinPerSummary {
		return guaranteeMinPerSummary
	}
	return share
}

// proseDepthCap bounds only a summary's full PROSE contribution to its
// depth section — key facts (writeKeyFacts) are deliberately NOT capped
// here. Facts are already engineered to be short and atomic
// (summarySystemPrompt asks for "a single concrete, checkable fact" per
// entry), and a Phase B (topic-clustered) busy day can legitimately have
// 20+ of them, each individually cheap; prose is where the real bulk
// lives (Phase B's multi-paragraph merged summaries) and is "for human
// skimming only, not treated as fact" per that same prompt — the right
// place to spend a tight cap when something has to give. Real-verified
// against the charity-events LongMemEval case: capping facts and prose
// together (the original version of this fix) let prose alone exhaust
// the cap before ever reaching a fact several bullets down the list,
// even though every fact was individually far cheaper than the prose
// that crowded it out.
const proseDepthCap = 500

// summaryDepthCap bounds the *whole* depth block (facts + prose
// together) that fusedSearchSummaries' pass 2 writes per picked summary
// — applied at the write site via hardTruncate, not inside depthText
// itself (see that function's own doc comment for why).
//
// depthText's facts were previously left fully uncapped on purpose —
// "20+ of them, each individually cheap" — and an earlier version of
// this cap (capping facts and prose together, back when facts were
// still written in raw insertion order) was tried and reverted for the
// charity-events case: prose alone exhausted that combined cap before
// ever reaching a fact, since nothing yet ranked facts by relevance.
// That's no longer the shape of the problem. Real-verified against
// `60bf93ed` ("how many days did my backpack take to arrive"): a single
// picked summary legitimately had **120** key facts — a LongMemEval
// `_abs` haystack artifact cramming many sessions' worth of unrelated
// content onto one calendar day — and depthText's own uncapped output
// for just that one summary ran to several thousand characters,
// consuming the entire default 2000-char budget by itself. The fact the
// question actually needed (the purchase date) was never extracted as
// a summary key_fact at all and only survived in raw episode text,
// appended to context *after* every summary — which never got a chance
// to contribute anything, not because the fact was missing, but because
// one summary's own depth section alone ate the whole budget. Confirmed
// via a budget override: the episode text is there, and the question
// answers correctly once nothing is truncated away first.
//
// Safe to reintroduce now, where it wasn't before: writeKeyFacts already
// writes facts in descending-relevance order (rankFactsByRelevance), so
// a tail cut here only ever drops the least-relevant facts, the same
// "least valuable content, as late as possible" property proseDepthCap
// already relies on — not an arbitrary cut into "whichever facts
// happened to be first." A flat per-summary constant, not split fairly
// across multiple picks the way guaranteeBudgetPerSummary is: depth is
// deliberately secondary "insurance" content (guaranteedFact's own doc
// comment), not the primary mechanism any already-fixed case depends
// on, so a simple, uniform bound is enough here without that added
// complexity.
const summaryDepthCap = 700

// depthText is every picked summary's "extra depth," written in pass 2
// of fusedSearchSummaries' two-pass render (see that function's own doc
// comment) — every key fact, uncapped, followed by a capped prose
// snippet. summaryDepthCap (applied by the caller, not here — see its
// own doc comment) is the real backstop against one summary's depth
// consuming the whole context budget; the eventual global
// truncateToBudget call is the final-final backstop if even multiple
// capped depth sections, episodes, and everything else together still
// exceed it.
func depthText(prose string, facts []keyFact, queryTerms []string) string {
	var sb strings.Builder
	writeKeyFacts(&sb, facts, queryTerms)
	sb.WriteString("\n" + truncateToBudget(prose, proseDepthCap))
	return sb.String()
}

// summaryCitationSnippet builds a citation's Snippet for a summary: its
// prose plus, when one stands out, its most query-relevant fact — reuses
// rankKeyFacts rather than a second ranking scheme, so the citation
// always agrees with what appendKeyFacts actually promoted in the
// injected context.
func summaryCitationSnippet(prose string, facts []keyFact, queryTerms []string) string {
	if _, best := rankKeyFacts(facts, queryTerms); best >= 0 {
		return prose + "\n  - (most relevant) " + facts[best].text
	}
	return prose
}

// factScores computes a per-fact query-relevance score — the one shared
// metric behind both mostRelevantFactIndex (pick the single fact that
// clearly stands out) and rankFactsByRelevance (order all of them). A
// single scoring definition, not two, so the two can never silently
// disagree about what "relevant" means.
//
// Uses BM25 (bm25Score, already relied on for summaries/episodes/
// entities elsewhere in this file) over this one summary's own facts as
// the corpus, rather than a flat distinct-term-overlap count. Real,
// measured fix (5809eb10, "what year did the construction of the house
// begin," a 62-fact summary about one legal case): the flat count gave a
// fact merely repeating the case's own generic identifying words
// ("Bajimaya", "Reward Homes Pty Ltd", "case" — shared by ~15 of the 62
// facts) a higher score (7) than the one fact actually naming the
// answer, "The construction of the house began in 2014" (score 3, since
// it doesn't repeat those same generic words) — common terms and rare,
// genuinely distinguishing ones counted identically. Direct measurement
// (live gpt-4.1 embeddings, real decrypted facts) confirmed BM25's IDF
// weighting — a term shared by many of a corpus's documents counts for
// less — fixes exactly this: the answer fact ranked outside the top 10
// under the flat count, #1 under BM25, moving it from missing the
// guaranteed/top-5 context entirely to comfortably inside it once fused
// with the semantic (embedding) signal rankKeyFacts already combines
// this with.
func factScores(facts []string, queryTerms []string) []float64 {
	docs := make([]bm25Document, len(facts))
	for i, f := range facts {
		docs[i] = newBM25Document("", f)
	}
	docFreq := bm25DocFrequency(docs)
	scores := make([]float64, len(facts))
	for i, doc := range docs {
		// bm25ScoreNoLengthNorm, not bm25Score — see its own doc comment
		// for why key-fact ranking specifically needs length
		// normalization disabled.
		scores[i] = bm25ScoreNoLengthNorm(doc, queryTerms, docFreq, len(docs))
	}
	return scores
}

// mostRelevantFactIndex returns the index (within facts, in its original
// order) of the fact sharing the most query vocabulary, or -1 if there
// are fewer than 2 facts, no query terms, or every fact ties (including
// a 0-0 tie — nothing to distinguish "most relevant" from the rest).
func mostRelevantFactIndex(facts []string, queryTerms []string) int {
	if len(facts) < 2 || len(queryTerms) == 0 {
		return -1
	}
	scores := factScores(facts, queryTerms)
	best, bestScore, tied := -1, 0.0, false
	for i, score := range scores {
		switch {
		case score > bestScore:
			best, bestScore, tied = i, score, false
		case score == bestScore && i != best:
			tied = true
		}
	}
	if bestScore == 0 || tied {
		return -1
	}
	return best
}

// rankFactsByRelevance returns every index into facts, ordered by
// descending query-relevance score (factScores' own metric) — stable on
// ties, so equally-scored facts keep their original relative order
// rather than one winning arbitrarily. Real, measured fix
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): mostRelevantFactIndex's own
// winner-take-all tie-break degrades as a summary's fact count grows — a
// real 112-fact busy day (41 episodes clustered into one summary) tied
// out entirely (two or more facts sharing the same top score), falling
// back to raw cluster-merge insertion order with no relevance signal
// applied at all, burying the one fact that actually answered the
// question under a hundred unrelated ones. This doesn't change the
// scoring itself (same crude-but-cheap word overlap as before) — it just
// stops discarding that signal the moment two facts tie, and stops
// leaving everything beyond a single winner in arbitrary order. No fact
// is ever dropped here, only reordered — when the existing global
// truncateToBudget backstop does have to cut something at extreme scale,
// it now lands on the least relevant tail, not an arbitrary one.
func rankFactsByRelevance(facts []string, queryTerms []string) []int {
	order := make([]int, len(facts))
	for i := range order {
		order[i] = i
	}
	if len(queryTerms) == 0 {
		return order
	}
	scores := factScores(facts, queryTerms)
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	return order
}

// vectorSearchEntities is vectorSearchSummaries' sibling over `entities`
// (schema/0012_entity_embeddings.sql) — a second chance for a query that
// doesn't literally contain a known entity's name/slug (stage1EntityMatches'
// substring check, the only other way an entity ever gets found) to still
// surface an entity that's semantically the answer. Found necessary via
// live testing: "what programming language do I prefer?" matched no
// entity by substring and no summary above vectorSimilarityThreshold,
// producing an honest "I don't know" for a fact that was genuinely on
// record as an entity the whole time.
//
// excludeIDs is stage 1's own matchedEntityIDs — already rendered into
// the context and already counted as a strong hit, so this only ever adds
// entities stage 1 didn't already find, never a duplicate. self_model is
// excluded the same way stage1EntityMatches excludes it: it's handled
// unconditionally by buildAnchor regardless of query content, not
// something that should ever compete for a vector-search slot.
func (s *Store) vectorSearchEntities(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, excludeIDs []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation, similarityThreshold float64, maxResults int, query string, now time.Time) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name, attributes, key_version, last_updated, (embedding <=> $1::vector) as distance
		from entities
		where embedding is not null and kind != 'self_model'
		  and (embedding_model is null or embedding_model = $6)
		  and not (id = any($2::text[]))
		  and scope_kind = $3 and scope_owner = $4
		order by embedding <=> $1::vector
		limit $5
	`, queryVector, pgfmt.TextArray(excludeIDs), scope.Kind, scope.Owner, maxResults, s.currentEmbeddingModel())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Same answer-time-reasoning hard filter stage1EntityMatches/
	// keywordSearchEntities already apply
	// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md) — a fifth, separate
	// retrieval path this investigation found also needed it: once
	// entities.last_updated was fixed to actually reflect the
	// consolidated date instead of real current_date, re-verifying the
	// Phase E adversarial case found this embedding-similarity path is
	// the one actually surfacing the wrong entity now, bypassing the
	// other two entity paths' own fixes entirely (a semantic match needs
	// no literal substring/keyword hit). Same no-backoff choice as those
	// two, for the same reason: the real motivating case is exactly one
	// semantically-matched entity whose only date is wrong, and a
	// backoff would restore precisely that match.
	tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now)

	var refs []identity.Ref
	for rows.Next() {
		var id, name string
		var attrsCT []byte
		var keyVersion int
		var lastUpdated time.Time
		var distance float64
		if err := rows.Scan(&id, &name, &attrsCT, &keyVersion, &lastUpdated, &distance); err != nil {
			return nil, err
		}
		similarity := 1 - distance
		if similarity < similarityThreshold {
			continue
		}
		if hasTimeframe && !periodsOverlap(lastUpdated, lastUpdated.AddDate(0, 0, 1), tfStart, tfEnd) {
			continue
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for entity %s: %w", id, err)
		}
		attrs, err := enc.Decrypt(attrsCT)
		if err != nil {
			return nil, err
		}
		dateLabel := ""
		if rel := relativeDateLabel(lastUpdated, now); rel != "" {
			dateLabel = fmt.Sprintf(", last updated %s (%s)", lastUpdated.Format("2006-01-02"), rel)
		}
		sb.WriteString(fmt.Sprintf("\nrelated memory (entity %s%s, %s): %s", id, dateLabel, name, attrs))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: id})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: id},
			Snippet: fmt.Sprintf("entity %s (%s): %s", id, name, attrs),
		})
		*strongHit = true
	}
	return refs, rows.Err()
}

// episodeExchangeCap bounds each matched episode's raw USER/ASSISTANT
// exchange, written fully uncapped before this fix — real-verified
// necessary alongside summaryDepthCap, not a standalone guess: fixing
// summaryDepthCap alone (so a busy summary's own depth section stops
// consuming the whole budget) still wasn't enough for `60bf93ed`. A real
// "episode" here turned out to be an entire multi-turn session, not a
// single exchange — `hupi-trace` on the one episode this question
// matched showed several back-and-forth turns about an unrelated
// wireless mouse, with the actual needed detail (a backpack's purchase
// date) mentioned in passing partway through a later turn, ~2000+
// characters into that one episode's own text. A plain head-truncate
// (this constant's first version) can never reach that — the cap would
// have to be nearly as large as the whole episode to work at all,
// defeating the point of capping. See centeredExcerpt below for the
// real fix: center the kept window on a matched query term instead of
// always keeping the start.
const episodeExchangeCap = 400

// centeredExcerpt returns a window of text around the densest cluster of
// matched query terms, instead of always keeping the text's head the
// way hardTruncate does — the real fix episodeExchangeCap's own doc
// comment describes. Falls back to a plain head-truncate when no query
// term is found in the text at all, which is still strictly better than
// returning nothing.
//
// Centers on the densest cluster, not just the first occurrence of any
// term — a real, measured correction found during re-verification
// against the `60bf93ed` case. An earlier version centered on the
// *first* matched term's position, which picked a passing, less
// relevant "backpack" mention early in the text; the passage that
// actually answers the question ("I bought it from Amazon on 1/15")
// sits near a denser cluster of several query terms together (laptop,
// backpack, *bought*) later on. Scoring every occurrence by how many
// other occurrences fall within its own candidate window correctly
// favors that denser, more relevant cluster.
func centeredExcerpt(text string, queryTerms []string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	// Term search stays byte-based (strings.Index is cheap and correct
	// for finding occurrences), but every position found is converted to
	// a rune index before any window/centering math or slicing runs —
	// text[start:end] on raw byte offsets could land mid-character on
	// multi-byte UTF-8 (smart quotes, accented names), the same class of
	// bug already fixed in chunkText/truncateToBudget/hardTruncate.
	lower := strings.ToLower(text)
	var positions []int // rune positions, not byte offsets
	for _, t := range queryTerms {
		if t == "" {
			continue
		}
		for i := 0; i+len(t) <= len(lower); {
			idx := strings.Index(lower[i:], t)
			if idx < 0 {
				break
			}
			bytePos := i + idx
			positions = append(positions, utf8.RuneCountInString(text[:bytePos]))
			i = bytePos + len(t)
		}
	}
	if len(positions) == 0 {
		return hardTruncate(text, maxChars)
	}
	// Center on whichever occurrence's own window (±half) covers the
	// most other occurrences, not just the first one found — real-
	// verified necessary against the backpack case: "backpack" alone
	// appears earlier in a passing, less relevant remark, while the
	// passage that actually answers the question ("I bought it from
	// Amazon on 1/15") sits near a denser cluster of several query terms
	// together (laptop, backpack, bought) — the first-occurrence version
	// of this function centered on the earlier, sparser mention and
	// still missed the detail needed.
	half := maxChars / 2
	best := positions[0]
	bestCount := -1
	for _, p := range positions {
		count := 0
		for _, q := range positions {
			if q >= p-half && q <= p+half {
				count++
			}
		}
		if count > bestCount {
			bestCount = count
			best = p
		}
	}
	pos := best
	start := pos - maxChars/2
	if start < 0 {
		start = 0
	}
	end := start + maxChars
	if end > len(runes) {
		end = len(runes)
		start = end - maxChars
		if start < 0 {
			start = 0
		}
	}
	excerpt := string(runes[start:end])
	if start > 0 {
		excerpt = "...[excerpt] " + excerpt
	}
	if end < len(runes) {
		excerpt += " ...[truncated]"
	}
	return excerpt
}

// vectorSearchEpisodes is vectorSearchSummaries' sibling over `episodes`
// instead of `summaries` — the "high-importance episodes" half of
// ARCHITECTURE.md's retrieval engine, closing docs/DESIGN_VS_BUILT.md #3.
// Only episodes consolidation has already embedded show up here (see
// Runner.embedHighImportanceEpisodes) — most single exchanges are only
// ever reachable via the daily summary that later folds them in; this
// exists for the case where something important hasn't been consolidated
// into a summary yet (or ever, if consolidation is behind or fails). Uses
// episodeVectorSimilarityThreshold, not vectorSimilarityThreshold — see
// that constant's own doc comment for the real measurement showing they
// need to differ.
func (s *Store) vectorSearchEpisodes(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, queryTerms []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation, query string, now time.Time) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version, ts, (embedding <=> $1::vector) as distance
		from episodes
		where embedding is not null and type = 'interaction'
		  and (embedding_model is null or embedding_model = $5)
		  and scope_kind = $2 and scope_owner = $3
		order by embedding <=> $1::vector
		limit $4
	`, queryVector, scope.Kind, scope.Owner, maxVectorResults(), s.currentEmbeddingModel())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type match struct {
		id            string
		input, output string
		ts            time.Time
	}
	var matches []match
	for rows.Next() {
		var id string
		var inputCT, outputCT []byte
		var keyVersion int
		var ts time.Time
		var distance float64
		if err := rows.Scan(&id, &inputCT, &outputCT, &keyVersion, &ts, &distance); err != nil {
			return nil, err
		}
		similarity := 1 - distance
		if similarity < episodeVectorSimilarityThreshold {
			continue
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for episode %s: %w", id, err)
		}
		input, err := enc.Decrypt(inputCT)
		if err != nil {
			return nil, err
		}
		output, err := enc.Decrypt(outputCT)
		if err != nil {
			return nil, err
		}
		matches = append(matches, match{id: id, input: input, output: output, ts: ts})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Same answer-time-reasoning hard filter keywordSearchEpisodes applies
	// to this exact same table (docs/CONSOLIDATION_COMPLETENESS_PLAN.md,
	// docs/CODEBASE_SURVEY_AND_REVIEW.md finding B7): a raw episode
	// vector-matched directly (bypassing consolidation/summaries and
	// keywordSearchEpisodes' own equivalent fix entirely — a semantic
	// match needs no literal keyword hit) can surface a temporally-wrong
	// exchange verbatim for a "last month"-shaped query, the one retrieval
	// path into this table that had no such filter at all. Same backoff
	// behavior as keywordSearchEpisodes/fusedSearchSummaries (not
	// vectorSearchEntities' own deliberate no-backoff choice): excludes
	// only matches with a confidently-resolved timeframe whose own exact
	// ts clearly falls outside it, backing off to the unfiltered set if
	// excluding would leave nothing.
	tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now)
	if hasTimeframe {
		kept := make([]match, 0, len(matches))
		for _, m := range matches {
			if m.ts.Before(tfStart) || !m.ts.Before(tfEnd) {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) > 0 {
			matches = kept
		}
	}

	var refs []identity.Ref
	for _, m := range matches {
		dateLabel := ""
		if rel := relativeDateLabel(m.ts, now); rel != "" {
			dateLabel = fmt.Sprintf(", dated %s (%s)", m.ts.Format("2006-01-02"), rel)
		}
		sb.WriteString(fmt.Sprintf("\nrelated exchange (episode %s%s): %s", m.id, dateLabel, centeredExcerpt(fmt.Sprintf("USER: %s ASSISTANT: %s", m.input, m.output), queryTerms, episodeExchangeCap)))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: m.id})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: m.id},
			Snippet: fmt.Sprintf("USER: %s ASSISTANT: %s", m.input, m.output),
		})
		*strongHit = true
	}
	return refs, nil
}

// keywordSearchEnabled is the installing admin's own escape hatch for
// BM25/keyword search's real, documented cost: it has no index over
// application-encrypted text (Postgres full-text search can't see
// through ciphertext — see keywordSearchSummaries' own doc comment), so
// it decrypts and scores every matching row in scope on every query
// where stage 1 found any signal at all. That's fine at the
// personal/team-history scale this product targets by default — real
// measurement (see keywordSearchSummaries' calibration table) found
// nothing wrong with the results themselves — but it's the one
// mechanism in this file whose cost genuinely scales with how much
// history a scope has accumulated, unlike vector search's index-
// accelerated top-K, which stays fast regardless of corpus size.
//
// Defaults to enabled: this is real, working functionality with a
// measured-correct threshold, not an experimental feature, and the
// vast majority of deployments (a single person's or team's own
// history) will never accumulate enough episodes for the cost to
// matter. The flag exists specifically for the informed minority who
// know their deployment doesn't fit that assumption — a very large
// or very long-lived corpus, or a latency-sensitive setting where
// even a few extra milliseconds of decrypt-and-score per turn isn't
// acceptable — and want vector search's index-accelerated top-K as
// the only retrieval mechanism, at the cost of losing BM25's exact-
// term recall (a specific name, ID, or acronym vector search can
// dilute or miss — see keywordSearchSummaries' own doc comment) and
// losing the ability to reach episodes consolidation never deemed
// important enough to embed.
//
// Checked on every call rather than cached at startup: os.Getenv reads
// an in-memory copy of the process environment, not a syscall, so
// there's no real cost to re-checking it, and this way a change takes
// effect on the next request rather than requiring a restart.
func keywordSearchEnabled() bool {
	return os.Getenv("HUPI_ENABLE_KEYWORD_SEARCH") != "false"
}

// keywordSearchTier governs how expensive a retrieval call lets BM25
// keyword search be for one scope, based on that scope's own real
// corpus size (internal/consolidation's updateScopeCorpusSize writes
// scope_corpus_size once per day, per active scope) — BM25 has no
// database index (every stored field is encrypted at rest), so it
// decrypts and live-scores the entire in-scope corpus on every call, a
// real, corpus-size-linear cost unlike vector search's own HNSW-indexed
// lookups.
type keywordSearchTier int

const (
	// keywordSearchFull is today's exact behavior — every keyword search
	// scans its table's entire in-scope matching set, unrestricted.
	keywordSearchFull keywordSearchTier = iota
	// keywordSearchNarrowed only changes fusedSearchSummaries' own
	// keyword half: when stage 1 already found a known entity in the
	// query, the scan is narrowed to summaries whose (already-plaintext)
	// entities_touched overlaps it — cheaper, and arguably more precise,
	// not just faster. With no entity match to narrow by, this tier runs
	// full BM25 anyway rather than silently dropping recall for the
	// exact "rare term with no entity anchor" case BM25 exists to catch
	// — see fusedSearchSummaries' own call site for exactly where that
	// fallback happens. keywordSearchEpisodes/keywordSearchEntities have
	// no entities_touched-equivalent plaintext column to narrow by
	// safely, so this tier behaves identically to keywordSearchFull for
	// both of them.
	keywordSearchNarrowed
	// keywordSearchDisabled skips keyword search entirely, across all
	// three mechanisms — either because a scope's corpus has grown past
	// keywordSearchDisableThreshold, or because the existing global
	// HUPI_ENABLE_KEYWORD_SEARCH=false opt-out is set (that check always
	// wins first, with no DB read, preserving today's zero-cost path for
	// anyone already using it).
	keywordSearchDisabled
)

// defaultKeywordSearchNarrowThreshold/-DisableThreshold are reasoned
// starting points, not yet calibrated against real traffic — the same
// honest status this file's own mmrLambda/clusterSimilarityThreshold
// carry elsewhere. hupi_keyword_search_tier_total (internal/metrics)
// exists specifically to give a real, deployment-wide tier distribution
// to calibrate these against once there's real usage behind them.
// Combined episode+summary count, not either alone — both tables pay the
// same live-decrypt cost, so what matters is the total a keyword search
// this scope might need to scan.
const defaultKeywordSearchNarrowThreshold = 500
const defaultKeywordSearchDisableThreshold = 3000

func keywordSearchNarrowThreshold() int {
	if v := os.Getenv("HUPI_KEYWORD_SEARCH_NARROW_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultKeywordSearchNarrowThreshold
}

func keywordSearchDisableThreshold() int {
	if v := os.Getenv("HUPI_KEYWORD_SEARCH_DISABLE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultKeywordSearchDisableThreshold
}

// keywordSearchTierForScope decides this call's keyword-search tier —
// the existing global HUPI_ENABLE_KEYWORD_SEARCH=false switch always
// wins first, with no DB read (today's exact zero-cost path for anyone
// already using it); otherwise one point-lookup against
// scope_corpus_size. A missing row (a brand-new scope with no
// consolidation run yet) defaults to keywordSearchFull, not
// keywordSearchDisabled — a scope with no recorded corpus size has
// nothing expensive to protect against yet. A real query error also
// falls back to keywordSearchFull rather than keywordSearchDisabled —
// this package's established "never destroy information on an unclear
// signal" posture (the same direction resolveQueryTimeframe's own
// backoff-when-ambiguous rule and groundingCheck's safe-degrade both
// take): a transient hiccup on this side-channel lookup shouldn't
// silently cost a legitimate small scope its keyword search.
func keywordSearchTierForScope(ctx context.Context, q dbscope.Querier, scope identity.Scope) (keywordSearchTier, error) {
	if !keywordSearchEnabled() {
		return keywordSearchDisabled, nil
	}
	var episodeCount, summaryCount int
	err := q.QueryRowContext(ctx, `
		select episode_count, summary_count from scope_corpus_size
		where scope_kind = $1 and scope_owner = $2
	`, scope.Kind, scope.Owner).Scan(&episodeCount, &summaryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return keywordSearchFull, nil
	}
	if err != nil {
		return keywordSearchFull, fmt.Errorf("load scope corpus size: %w", err)
	}
	total := episodeCount + summaryCount
	switch {
	case total > keywordSearchDisableThreshold():
		return keywordSearchDisabled, nil
	case total > keywordSearchNarrowThreshold():
		return keywordSearchNarrowed, nil
	default:
		return keywordSearchFull, nil
	}
}

// tierLabel is hupi_keyword_search_tier_total's own label value — a
// fixed, bounded three-value enum-to-string mapping, never derived from
// scope data, matching every other label in internal/metrics.
func tierLabel(tier keywordSearchTier) string {
	switch tier {
	case keywordSearchNarrowed:
		return "narrowed"
	case keywordSearchDisabled:
		return "disabled"
	default:
		return "full"
	}
}

// graphWalkEnabled defaults to *disabled* (review finding B12) — the
// inverse of keywordSearchEnabled's own opt-out posture, and
// deliberately so: three independent measurements (docs/BENCHMARK_IMPROVEMENT_PLAN.md
// step 2 — a stale v3 ablation, the EvalMem integration's 0/32
// firing-rate finding, and a real LoCoMo category-1 on-vs-off re-check,
// 106 questions, 35.3% vs. 35.4%, "a settled finding, not an open
// question") all independently agree this mechanism contributes nothing
// measurable on either public benchmark, while still spending real,
// bounded-but-nonzero cost competing for the same fixed context budget
// findings A4/A5 show is a real, recurring bottleneck — a cost with no
// offsetting, demonstrated benefit is the wrong default. Opt in with
// HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK=true for a deployment whose own
// relationship graph is denser or more load-bearing than either
// benchmark's — graphWalkMaxHops/graphWalkMaxResults already bound the
// cost tightly for exactly that "informed minority" case.
func graphWalkEnabled() bool {
	return os.Getenv("HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK") == "true"
}

// contextCharBudget is the same "crude character stand-in for a real
// token budget" defaultContextCharBudget's own doc comment describes,
// made overridable rather than a flat constant. Found via a real cloud
// benchmark run that 2000 characters (~500 tokens) — fine for a modest
// local model — was hard-truncating the retrieved context well before a
// large-context model like GPT-4.1 needed to stop: adding a summary's
// grounded key_facts (see appendKeyFacts) made each surfaced summary
// bigger, which meant *fewer* summaries fit before hitting this cap,
// silently dropping ones a smaller per-summary size would have kept.
// Left as a small default for the same reason keywordSearchEnabled and
// graphWalkEnabled default the way they do — this is the "informed
// minority" override, not a new default for every deployment.
func contextCharBudget() int {
	if v := os.Getenv("HUPI_CONTEXT_CHAR_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultContextCharBudget
}

// maxVectorResults is the same "informed minority override" pattern
// contextCharBudget is — a real cloud benchmark run found the fixed cap
// of 5 was excluding genuinely relevant summaries/entities/episodes from
// consideration entirely, before contextCharBudget even got a chance to
// include them: for a long-running scope with many accumulated
// summaries, "top 5 by vector similarity" can leave out a relevant-but-
// not-quite-top-5 summary regardless of how generous the char budget is.
// Left as a small default for the same reason contextCharBudget,
// keywordSearchEnabled, and graphWalkEnabled default the way they do —
// this is the override for a deployment pairing HUPI with a
// large-context model and evaluating/tuning recall, not a new default
// for every deployment.
func maxVectorResults() int {
	if v := os.Getenv("HUPI_MAX_VECTOR_RESULTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxVectorResults
}

// summaryOverfetchFactor controls how many candidates
// vectorSearchSummaries pulls before MMR selection narrows them down to
// maxVectorResults() — MMR has nothing to select *from* if it's only
// ever handed exactly as many candidates as it's asked to return.
// docs/BENCHMARK_IMPROVEMENT_PLAN.md's own real finding motivating this:
// raising maxVectorResults from 5 to 12 alone was neutral (not a win)
// because a two-speaker conversation's summaries mostly repeat the same
// well-covered facts -- more candidates competing for the same fixed
// char budget, not more distinct coverage. 3x is enough slack for MMR to
// actually have a meaningful choice without querying the whole scope's
// summary history on every request.
const summaryOverfetchFactor = 3

// mmrLambda is the same "informed minority override" pattern as
// contextCharBudget/maxVectorResults. Standard Maximal Marginal
// Relevance: score = lambda*relevance - (1-lambda)*maxSimilarityToAlreadySelected.
// 0.7 favors relevance over diversity as the default (a deployment's
// first, most-similar match is usually still what the user is asking
// about) while still meaningfully penalizing a near-duplicate of
// something already selected -- not calibrated against real measured
// queries yet, unlike vectorSimilarityThreshold and friends; re-measure
// once this has real traffic behind it.
func mmrLambda() float64 {
	if v := os.Getenv("HUPI_MMR_LAMBDA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 0.7
}

// jaccardOverlap is the diversity signal mmrSelectSummaries uses instead
// of a second round of embedding math: two summaries repeating the same
// well-covered narrative ("Caroline and Melanie catch up...") share most
// of their content words even when phrased slightly differently run to
// run, which real inspection of this exact redundancy problem
// (docs/EVALMEM_INTEGRATION_PLAN.md §7) confirmed is the actual shape of
// the near-duplicates costing budget space -- lexical overlap is a
// simple, dependency-free, no-extra-query proxy for that, reusing
// tokenize (bm25.go) rather than a second embedding round-trip per
// candidate pair.
func jaccardOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(large) < len(small) {
		small, large = large, small
	}
	intersection := 0
	for t := range small {
		if large[t] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenSet is tokenize's output as a set, for jaccardOverlap.
func tokenSet(text string) map[string]bool {
	tokens := tokenize(text)
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		set[t] = true
	}
	return set
}

// mmrCandidate is the minimal shape mmrSelect needs: something rankable
// by query-relevance and comparable pairwise for redundancy. Kept
// generic (not summary-specific) so the same selection logic could later
// serve entities/episodes too, per docs/BENCHMARK_IMPROVEMENT_PLAN.md's
// own note that steps 3 and 4 share this refactor point.
type mmrCandidate struct {
	relevance float64
	tokens    map[string]bool
}

// mmrSelect greedily picks up to k candidates from pool, balancing each
// candidate's own query-relevance against its maximum similarity to
// whatever's already been picked -- standard Maximal Marginal Relevance.
// Returns the *indices* into pool, in selection order (most relevant
// first) -- the caller's own candidate slice already carries the rest of
// each candidate's data (id/text/enc for summaries), so this only needs
// to hand back which ones won and in what order.
func mmrSelect(pool []mmrCandidate, k int, lambda float64) []int {
	if k >= len(pool) {
		// Still sorted by relevance descending, not left in whatever order
		// the caller's candidate slice happened to be built in — a real,
		// previously untested bug (docs/CONSOLIDATION_COMPLETENESS_PLAN.md
		// Phase E verification): with few enough candidates that none get
		// discarded here, this branch was the one actually taken for the
		// exact case Phase D item 2's widened threshold and Phase E's
		// temporal boost exist to reorder, silently returning candidates
		// in raw pool order and discarding both fixes' entire effect on
		// what the model actually sees first.
		out := make([]int, len(pool))
		for i := range pool {
			out[i] = i
		}
		sort.Slice(out, func(i, j int) bool { return pool[out[i]].relevance > pool[out[j]].relevance })
		return out
	}
	remaining := make([]int, len(pool))
	for i := range pool {
		remaining[i] = i
	}
	var selected []int
	for len(selected) < k && len(remaining) > 0 {
		bestPos, bestScore := 0, math.Inf(-1)
		for pos, idx := range remaining {
			maxSim := 0.0
			for _, sIdx := range selected {
				if sim := jaccardOverlap(pool[idx].tokens, pool[sIdx].tokens); sim > maxSim {
					maxSim = sim
				}
			}
			score := lambda*pool[idx].relevance - (1-lambda)*maxSim
			if score > bestScore {
				bestPos, bestScore = pos, score
			}
		}
		selected = append(selected, remaining[bestPos])
		remaining = append(remaining[:bestPos], remaining[bestPos+1:]...)
	}
	return selected
}

// graphWalkMaxHops/graphWalkMaxResults are the "own hop-limit and token
// budget" docs/ENTITY_RELATIONSHIPS_PLAN.md §6 called for before this was
// written — an ungated walk on a densely-connected scope could pull in
// far more context than any one query needs. 2 hops is enough to answer
// a genuine two-step connection (LoCoMo's own "multi-hop" category is
// exactly this shape) without approaching an unbounded graph traversal;
// 10 total connected entities caps the worst case (a hub entity with
// many edges) from dominating the context budget on its own.
const (
	graphWalkMaxHops    = 2
	graphWalkMaxResults = 10
)

// graphWalkRelationships surfaces entities connected to seedEntityIDs
// (whatever stage 1 exact matches, vector search, and keyword search
// already found) by walking outward along entity_relationships edges —
// see docs/ENTITY_RELATIONSHIPS_PLAN.md §6. This is what lets a query
// that never names the connected entity still surface it (e.g. "who did
// Caroline meet through Melanie" when only "Melanie" was matched
// directly), rather than depending on the connection happening to be
// restated in the same retrieved summary's prose.
//
// Only walks currently-valid edges (valid_until is null) — a
// superseded/historical relationship (schema/0015's whole bi-temporal
// point) isn't a *current* connection between the two entities, so
// including it here would surface a stale connection as if it still
// held. A future "what was true as of date X" retrieval mode would need
// to relax this, but nothing today asks that question.
func (s *Store) graphWalkRelationships(ctx context.Context, q dbscope.Querier, scope identity.Scope, seedEntityIDs []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation) ([]identity.Ref, error) {
	if len(seedEntityIDs) == 0 || !graphWalkEnabled() {
		return nil, nil
	}

	visited := make(map[string]bool, len(seedEntityIDs))
	for _, id := range seedEntityIDs {
		visited[id] = true
	}
	frontier := append([]string{}, seedEntityIDs...)
	var discovered []string

	for hop := 0; hop < graphWalkMaxHops && len(frontier) > 0 && len(discovered) < graphWalkMaxResults; hop++ {
		rows, err := q.QueryContext(ctx, `
			select subject_id, object_id from entity_relationships
			where scope_kind = $1 and scope_owner = $2
			  and (subject_id = any($3::text[]) or object_id = any($3::text[]))
			  and valid_until is null
		`, scope.Kind, scope.Owner, pgfmt.TextArray(frontier))
		if err != nil {
			return nil, fmt.Errorf("graph walk hop %d: %w", hop, err)
		}

		var nextFrontier []string
		for rows.Next() {
			var subjectID, objectID string
			if err := rows.Scan(&subjectID, &objectID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan graph walk row: %w", err)
			}
			for _, candidate := range []string{subjectID, objectID} {
				if visited[candidate] || len(discovered) >= graphWalkMaxResults {
					continue
				}
				visited[candidate] = true
				discovered = append(discovered, candidate)
				nextFrontier = append(nextFrontier, candidate)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate graph walk rows: %w", err)
		}
		rows.Close()
		frontier = nextFrontier
	}

	refs := make([]identity.Ref, 0, len(discovered))
	for _, id := range discovered {
		line, err := s.formatEntity(ctx, q, scope, id, "")
		if err != nil {
			return nil, fmt.Errorf("load graph-walked entity %s: %w", id, err)
		}
		// ", relationship graph" mirrors the existing ", keyword match"
		// suffix convention (keywordSearchSummaries et al.) -- without
		// this, a graph-walked entity is textually indistinguishable
		// from one stage1EntityMatches found by a direct name mention in
		// the query, making it impossible to tell from ContextMessage
		// alone whether the graph walk actually contributed anything.
		sb.WriteString("\n" + line + ", relationship graph")
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: id})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: id},
			Snippet: line + ", relationship graph",
		})
		*strongHit = true // a graph-connected entity is as strong a signal as a directly-matched one
	}
	return refs, nil
}

// refIDsOfKind extracts the IDs of every already-collected ref of the
// given kind — used to build keywordSearchSummaries/Episodes' excludeIDs
// from whatever vector search already found for that content type.
func refIDsOfKind(refs []identity.Ref, kind string) []string {
	var ids []string
	for _, r := range refs {
		if r.Kind == kind {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// keywordSearchEpisodes shares fusedSearchSummaries' keyword half's
// design (see that function's doc comment): why this decrypts the whole
// matching set rather than using an index. Unlike summaries, this one
// isn't fused with vectorSearchEpisodes — episodes are a supplementary
// path (see vectorSearchEpisodes' own doc comment), not the primary
// retrieval surface summaries are, so the smaller, independent-searches
// shape hasn't been worth revisiting yet.
//
// Deliberately NOT restricted to "embedding is not null" the way
// vectorSearchEpisodes is: that restriction exists there because a
// vector index only has entries for episodes consolidation actually
// embedded (the smaller, "high-importance" subset), but BM25 needs no
// embedding to exist at all. Covering every interaction episode, not
// just the embedded subset, is the entire point of adding this — a
// low-importance episode consolidation never deemed worth embedding is
// exactly the kind of thing vector search can structurally never find,
// and BM25 finding an exact term there is a real capability gain, not
// just parity with vector search.
//
// This is a genuinely bigger decrypt cost than keywordSearchSummaries
// pays, and worth being explicit about: episodes accumulate per turn,
// not per day like summaries, so this corpus grows much faster over a
// long-lived deployment. Still fine at personal/team-history scale
// (same trade-off stage1EntityMatches already makes elsewhere in this
// file), but the first mechanism in this file where that scale
// assumption is worth re-checking if it ever stops holding.
func (s *Store) keywordSearchEpisodes(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryTerms []string, excludeIDs []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation, query string, now time.Time) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version, ts
		from episodes
		where type = 'interaction'
		  and not (id = any($1::text[]))
		  and scope_kind = $2 and scope_owner = $3
	`, pgfmt.TextArray(excludeIDs), scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type exchange struct {
		input, output string
		ts            time.Time
	}
	byID := make(map[string]exchange)
	var docs []bm25Document
	for rows.Next() {
		var id string
		var inputCT, outputCT []byte
		var keyVersion int
		var ts time.Time
		if err := rows.Scan(&id, &inputCT, &outputCT, &keyVersion, &ts); err != nil {
			return nil, err
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for episode %s: %w", id, err)
		}
		input, err := enc.Decrypt(inputCT)
		if err != nil {
			return nil, err
		}
		output, err := enc.Decrypt(outputCT)
		if err != nil {
			return nil, err
		}
		byID[id] = exchange{input: input, output: output, ts: ts}
		docs = append(docs, newBM25Document(id, input+" "+output))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	matches := rankBM25(docs, queryTerms)

	// Same answer-time-reasoning hard filter fusedSearchSummaries applies
	// to summaries (docs/CONSOLIDATION_COMPLETENESS_PLAN.md) — real,
	// confirmed necessary here too, not just there: a raw episode
	// keyword-matched directly (bypassing consolidation/summaries
	// entirely) is exactly what surfaced a temporally-wrong exchange
	// verbatim in the adversarial "AI conference" test, even after
	// summaries' own filter correctly excluded the equivalent summary —
	// this path has its own separate route into context and needs its
	// own separate fix. Excludes only matches with a confidently-resolved
	// timeframe whose own exact ts clearly falls outside it; backs off to
	// the unfiltered set if excluding would leave nothing, same
	// safe-degrade direction used everywhere else this pattern appears.
	tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now)
	if hasTimeframe {
		kept := make([]string, 0, len(matches))
		for _, m := range matches {
			ts := byID[m].ts
			if ts.Before(tfStart) || !ts.Before(tfEnd) {
				continue
			}
			kept = append(kept, m)
		}
		if len(kept) > 0 {
			matches = kept
		}
	}

	var refs []identity.Ref
	for _, m := range matches {
		ex := byID[m]
		// Same computed-not-raw date label fusedSearchSummaries writes
		// for summaries — an episode's own ts is even more precise than
		// a summary's period, so this is at least as reliable.
		dateLabel := ""
		if rel := relativeDateLabel(ex.ts, now); rel != "" {
			dateLabel = fmt.Sprintf(", dated %s (%s)", ex.ts.Format("2006-01-02"), rel)
		}
		sb.WriteString(fmt.Sprintf("\nrelated exchange (episode %s%s, keyword match): %s", m, dateLabel, centeredExcerpt(fmt.Sprintf("USER: %s ASSISTANT: %s", ex.input, ex.output), queryTerms, episodeExchangeCap)))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: m})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: m},
			Snippet: fmt.Sprintf("USER: %s ASSISTANT: %s", ex.input, ex.output),
		})
		*strongHit = true
	}
	return refs, nil
}

// keywordSearchEntities is keywordSearchSummaries' sibling over
// `entities` — see that function's doc comment for the shared design.
// excludeIDs should be every entity ID already found by this point
// (stage 1's own substring matches plus whatever vectorSearchEntities
// just added — both already live in retrieve()'s single, growing refs
// slice by the time this runs), same exclusion vectorSearchEntities
// itself applies against stage 1. self_model is excluded the same way
// stage1EntityMatches and vectorSearchEntities both exclude it: handled
// unconditionally by buildAnchor, never something that should compete
// for a keyword-search slot.
//
// BM25 document text is name + decrypted attributes, not attributes
// alone — name is already plaintext (stage1EntityMatches substring-
// matches it directly with no decryption step), so including it costs
// nothing and lets a query matching an entity's name but not its stored
// attribute values still score.
func (s *Store) keywordSearchEntities(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryTerms []string, excludeIDs []string, sb *strings.Builder, strongHit *bool, citations *[]gateway.Citation, query string, now time.Time) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name, attributes, key_version, last_updated
		from entities
		where attributes is not null and kind != 'self_model'
		  and not (id = any($1::text[]))
		  and scope_kind = $2 and scope_owner = $3
	`, pgfmt.TextArray(excludeIDs), scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type entity struct {
		name, attrs string
		lastUpdated time.Time
	}
	byID := make(map[string]entity)
	var docs []bm25Document
	for rows.Next() {
		var id, name string
		var attrsCT []byte
		var keyVersion int
		var lastUpdated time.Time
		if err := rows.Scan(&id, &name, &attrsCT, &keyVersion, &lastUpdated); err != nil {
			return nil, err
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for entity %s: %w", id, err)
		}
		attrs, err := enc.Decrypt(attrsCT)
		if err != nil {
			return nil, err
		}
		byID[id] = entity{name: name, attrs: attrs, lastUpdated: lastUpdated}
		docs = append(docs, newBM25Document(id, name+" "+attrs))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	matches := rankBM25(docs, queryTerms)

	// Same answer-time-reasoning hard filter fusedSearchSummaries/
	// keywordSearchEpisodes/stage1EntityMatches already apply
	// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a fourth, separate
	// retrieval path this investigation found also needed it — BM25
	// keyword ranking over the whole entity corpus, distinct from
	// stage1EntityMatches' narrow substring pre-check, and unaffected by
	// that path's own fix. Deliberately does NOT back off when excluding
	// would leave nothing, matching stage1EntityMatches' own choice, not
	// fusedSearchSummaries/keywordSearchEpisodes': the real adversarial
	// case that motivated this had exactly one keyword-matched entity
	// (the same single "AI conference" entity stage1 also matched, once
	// stage1's own filter has already excluded it from excludeIDs), so a
	// backoff here would restore the exact match the fix exists to
	// remove — the same trap a backoff would spring for stage1. Entities
	// are a supplementary signal either way; the actual facts and dates
	// live in summaries/episodes, where the backoff guarantee still
	// applies.
	tfStart, tfEnd, hasTimeframe := resolveQueryTimeframe(query, now)
	if hasTimeframe {
		kept := make([]string, 0, len(matches))
		for _, m := range matches {
			lu := byID[m].lastUpdated
			if periodsOverlap(lu, lu.AddDate(0, 0, 1), tfStart, tfEnd) {
				kept = append(kept, m)
			}
		}
		matches = kept
	}

	var refs []identity.Ref
	for _, m := range matches {
		e := byID[m]
		dateLabel := ""
		if rel := relativeDateLabel(e.lastUpdated, now); rel != "" {
			dateLabel = fmt.Sprintf(", last updated %s (%s)", e.lastUpdated.Format("2006-01-02"), rel)
		}
		sb.WriteString(fmt.Sprintf("\nrelated memory (entity %s%s, %s, keyword match): %s", m, dateLabel, e.name, e.attrs))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: m})
		*citations = append(*citations, gateway.Citation{
			Ref:     identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: m},
			Snippet: fmt.Sprintf("entity %s (%s): %s", m, e.name, e.attrs),
		})
		*strongHit = true
	}
	return refs, nil
}

// rankBM25 scores every document against queryTerms using corpus stats
// computed from that same document set (see bm25DocFrequency's doc
// comment), returns IDs of every positively-scored document ordered
// highest-score-first, capped at maxVectorResults — the same result cap
// vector search uses, so keyword search can't unboundedly outweigh it in
// the final context.
func rankBM25(docs []bm25Document, queryTerms []string) []string {
	docFreq := bm25DocFrequency(docs)
	avgDocLen := bm25AverageDocLength(docs)

	type scored struct {
		id    string
		score float64
	}
	var matches []scored
	for _, doc := range docs {
		if score := bm25Score(doc, queryTerms, docFreq, len(docs), avgDocLen); score > 0 {
			matches = append(matches, scored{id: doc.id, score: score})
		}
	}
	// Tie-broken by id, not just score (review finding B10) — see
	// fusedSearchSummaries' own identical fix for the full reasoning:
	// the underlying SQL has no ORDER BY, so without a deterministic
	// secondary key here, which tied-score candidate survives the
	// maxVectorResults cap below could vary run-to-run with identical
	// data.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].id < matches[j].id
	})
	if len(matches) > maxVectorResults() {
		matches = matches[:maxVectorResults()]
	}

	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.id
	}
	return ids
}

// truncateToBudget and hardTruncate both count and cut by rune, not byte
// (review finding B9): maxChars has always meant character count, by
// name and by every caller's own intent, but raw byte-slicing (s[:n])
// can land in the middle of a multi-byte UTF-8 sequence for any
// non-ASCII content — an accented letter, CJK text, an emoji — producing
// invalid UTF-8 at the cut point. Identical to the old byte-based
// behavior for pure ASCII text (every rune is one byte there), the
// common case this went uncaught in.
func truncateToBudget(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	return string([]rune(s)[:maxChars]) + "\n...[truncated to fit context budget]"
}

// hardTruncate is truncateToBudget without the "...[truncated]" marker
// — used by fusedSearchSummaries' per-summary guarantee-line cap
// (guaranteeBudgetPerSummary's own doc comment), where the marker's own
// 37 characters, repeated once per picked summary, was itself enough to
// blow the fair per-summary budget this cap exists to enforce. The
// marker is worth its cost exactly once, at the final whole-context
// truncateToBudget call — telling the model there was more it isn't
// seeing — not on every individual, already-expected minor shortening.
func hardTruncate(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	return string([]rune(s)[:maxChars])
}

func lastUserMessage(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}
