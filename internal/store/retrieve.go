package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
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
	// entityEmbedText in internal/consolidation/store.go) has different
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
	maxVectorResults                = 5
	// contextCharBudget is a crude stand-in for a real token budget
	// (ARCHITECTURE.md mentions ~20% of the model's context window) — a
	// production build should count tokens against the target model's
	// tokenizer, not characters.
	contextCharBudget = 2000
)

// stage1SignalKeywords is the cheap, local, no-LLM-call pre-check from
// ARCHITECTURE.md's gate table: does this message look like it's worth
// searching for at all. Entity-name matches (stage1EntityMatches) are the
// other, usually stronger, half of stage 1.
var stage1SignalKeywords = []string{
	"remember", "recall", "decided", "decide", "prefer",
	"we discussed", "last time", "again", "what did", "earlier", "before",
}

// Retrieve implements gateway.Retriever, delegating to retrieve for the
// actual read and logging exactly one audit_log row per call regardless
// of which of retrieve's return paths fired — see retrieve's doc comment
// for the retrieval logic itself.
func (s *Store) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (gateway.RetrievalResult, error) {
	result, err := s.retrieve(ctx, actingUser, workspace, messages)
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
func (s *Store) retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (gateway.RetrievalResult, error) {
	var anchor string
	var anchorRefs []identity.Ref
	var matchedEntityIDs []string
	var entityLines []string

	query := lastUserMessage(messages)

	err := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		var err error
		anchor, anchorRefs, err = s.buildAnchor(ctx, tx, actingUser, workspace)
		if err != nil {
			return fmt.Errorf("build anchor: %w", err)
		}
		if query == "" {
			return nil
		}
		matchedEntityIDs, err = s.stage1EntityMatches(ctx, tx, workspace, query)
		if err != nil {
			return fmt.Errorf("stage1 entity scan: %w", err)
		}
		for _, id := range matchedEntityIDs {
			line, err := s.formatEntity(ctx, tx, workspace, id)
			if err != nil {
				return fmt.Errorf("load matched entity %s: %w", id, err)
			}
			entityLines = append(entityLines, line)
		}
		return nil
	})
	if err != nil {
		return gateway.RetrievalResult{}, fmt.Errorf("store: %w", err)
	}

	if query == "" {
		return gateway.RetrievalResult{Gate: gateway.GateSkipped, ContextMessage: anchor, Refs: anchorRefs}, nil
	}

	hasKeywordSignal := stage1KeywordSignal(query)

	// Stage 1 found nothing at all: skipped, no search ever ran.
	if len(matchedEntityIDs) == 0 && !hasKeywordSignal {
		return gateway.RetrievalResult{Gate: gateway.GateSkipped, ContextMessage: anchor, Refs: anchorRefs}, nil
	}

	// Stage 2: something looked worth searching for.
	var sb strings.Builder
	sb.WriteString(anchor)

	refs := append([]identity.Ref{}, anchorRefs...)
	strongHit := false

	for i, id := range matchedEntityIDs {
		sb.WriteString("\n" + entityLines[i])
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: workspace, ID: id})
		strongHit = true // an exact entity-key match is always a strong hit
	}

	// Embed the query once and reuse it for both searches below — no
	// reason to pay for the same embedding call twice. Deliberately
	// outside any transaction: this is a network call to the embedding
	// provider.
	embedResp, err := s.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{query}})
	if err != nil {
		return gateway.RetrievalResult{}, fmt.Errorf("store: embed query: %w", err)
	}
	if len(embedResp.Vectors) == 0 {
		return gateway.RetrievalResult{}, errors.New("store: embedder returned no vectors")
	}
	queryVector := pgfmt.VectorLiteral(embedResp.Vectors[0])

	err = dbscope.Run(ctx, s.db, workspace, workspace, func(tx *sql.Tx) error {
		summaryRefs, err := s.vectorSearchSummaries(ctx, tx, workspace, queryVector, &sb, &strongHit)
		if err != nil {
			return fmt.Errorf("vector search summaries: %w", err)
		}
		refs = append(refs, summaryRefs...)

		episodeRefs, err := s.vectorSearchEpisodes(ctx, tx, workspace, queryVector, &sb, &strongHit)
		if err != nil {
			return fmt.Errorf("vector search episodes: %w", err)
		}
		refs = append(refs, episodeRefs...)

		entityRefs, err := s.vectorSearchEntities(ctx, tx, workspace, queryVector, matchedEntityIDs, &sb, &strongHit)
		if err != nil {
			return fmt.Errorf("vector search entities: %w", err)
		}
		refs = append(refs, entityRefs...)
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
		Gate:           gate,
		ContextMessage: truncateToBudget(sb.String(), contextCharBudget),
		Refs:           refs,
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
func (s *Store) buildAnchor(ctx context.Context, q dbscope.Querier, actingUser, workspace identity.Scope) (string, []identity.Ref, error) {
	var sb strings.Builder
	var refs []identity.Ref

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
		return "", nil, fmt.Errorf("load self_model: %w", err)
	default:
		actingEnc, keyErr := s.keys.GetVersion(ctx, actingUser, keyVersion)
		if keyErr != nil {
			return "", nil, fmt.Errorf("resolve encryption key for self_model: %w", keyErr)
		}
		attrs, err = actingEnc.Decrypt(attrsCT)
		if err != nil {
			return "", nil, fmt.Errorf("decrypt self_model attributes: %w", err)
		}
		sb.WriteString("self_model: " + attrs + "\n")
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: actingUser, ID: selfID})
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
		return "", nil, fmt.Errorf("load latest daily summary period: %w", err)
	default:
		sb.WriteString("(latest daily summary: " + latestPeriod + ")\n")
	}

	return sb.String(), refs, nil
}

// stage1EntityMatches does a cheap, local (no LLM call) scan for known
// entity names/id-slugs appearing in the message, restricted to the given
// scope — the "known entity names" half of ARCHITECTURE.md's stage-1
// pre-check. Fetching the whole entity table per request is fine at
// personal-history scale; a higher-volume deployment should cache this
// list in memory and invalidate it on entity writes rather than query it
// on every turn.
func (s *Store) stage1EntityMatches(ctx context.Context, q dbscope.Querier, scope identity.Scope, query string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name from entities
		where kind != 'self_model' and scope_kind = $1 and scope_owner = $2
	`, scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lowerQuery := strings.ToLower(query)
	var matches []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if name != "" && strings.Contains(lowerQuery, strings.ToLower(name)) {
			matches = append(matches, id)
			continue
		}
		if slug := slugPart(id); slug != "" && strings.Contains(lowerQuery, strings.ToLower(slug)) {
			matches = append(matches, id)
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

func (s *Store) formatEntity(ctx context.Context, q dbscope.Querier, scope identity.Scope, id string) (string, error) {
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
	return fmt.Sprintf("entity %s (%s): %s", id, name, attrs), nil
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
func (s *Store) vectorSearchSummaries(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, sb *strings.Builder, strongHit *bool) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, summary, key_version, (embedding <=> $1::vector) as distance
		from summaries s
		where embedding is not null
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		  and scope_kind = $2 and scope_owner = $3
		order by embedding <=> $1::vector
		limit $4
	`, queryVector, scope.Kind, scope.Owner, maxVectorResults)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []identity.Ref
	for rows.Next() {
		var id string
		var summaryCT []byte
		var keyVersion int
		var distance float64
		if err := rows.Scan(&id, &summaryCT, &keyVersion, &distance); err != nil {
			return nil, err
		}
		// Cosine distance -> similarity for a normalized embedding space;
		// see vectorSimilarityThreshold's doc comment for the cutoff.
		similarity := 1 - distance
		if similarity < vectorSimilarityThreshold {
			continue
		}
		enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for summary %s: %w", id, err)
		}
		text, err := enc.Decrypt(summaryCT)
		if err != nil {
			return nil, err
		}
		sb.WriteString(fmt.Sprintf("\nrelated memory (summary %s): %s", id, text))
		refs = append(refs, identity.Ref{Kind: identity.RefKindSummary, Scope: scope, ID: id})
		*strongHit = true
	}
	return refs, rows.Err()
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
func (s *Store) vectorSearchEntities(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, excludeIDs []string, sb *strings.Builder, strongHit *bool) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name, attributes, key_version, (embedding <=> $1::vector) as distance
		from entities
		where embedding is not null and kind != 'self_model'
		  and not (id = any($2::text[]))
		  and scope_kind = $3 and scope_owner = $4
		order by embedding <=> $1::vector
		limit $5
	`, queryVector, pgfmt.TextArray(excludeIDs), scope.Kind, scope.Owner, maxVectorResults)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []identity.Ref
	for rows.Next() {
		var id, name string
		var attrsCT []byte
		var keyVersion int
		var distance float64
		if err := rows.Scan(&id, &name, &attrsCT, &keyVersion, &distance); err != nil {
			return nil, err
		}
		similarity := 1 - distance
		if similarity < entityVectorSimilarityThreshold {
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
		sb.WriteString(fmt.Sprintf("\nrelated memory (entity %s, %s): %s", id, name, attrs))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: id})
		*strongHit = true
	}
	return refs, rows.Err()
}

// vectorSearchEpisodes is vectorSearchSummaries' sibling over `episodes`
// instead of `summaries` — the "high-importance episodes" half of
// ARCHITECTURE.md's retrieval engine, closing docs/DESIGN_VS_BUILT.md #3.
// Only episodes consolidation has already embedded show up here (see
// Runner.embedHighImportanceEpisodes) — most single exchanges are only
// ever reachable via the daily summary that later folds them in; this
// exists for the case where something important hasn't been consolidated
// into a summary yet (or ever, if consolidation is behind or fails).
func (s *Store) vectorSearchEpisodes(ctx context.Context, q dbscope.Querier, scope identity.Scope, queryVector string, sb *strings.Builder, strongHit *bool) ([]identity.Ref, error) {
	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version, (embedding <=> $1::vector) as distance
		from episodes
		where embedding is not null and type = 'interaction'
		  and scope_kind = $2 and scope_owner = $3
		order by embedding <=> $1::vector
		limit $4
	`, queryVector, scope.Kind, scope.Owner, maxVectorResults)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []identity.Ref
	for rows.Next() {
		var id string
		var inputCT, outputCT []byte
		var keyVersion int
		var distance float64
		if err := rows.Scan(&id, &inputCT, &outputCT, &keyVersion, &distance); err != nil {
			return nil, err
		}
		similarity := 1 - distance
		if similarity < vectorSimilarityThreshold {
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
		sb.WriteString(fmt.Sprintf("\nrelated exchange (episode %s): USER: %s ASSISTANT: %s", id, input, output))
		refs = append(refs, identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: id})
		*strongHit = true
	}
	return refs, rows.Err()
}

func truncateToBudget(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...[truncated to fit context budget]"
}

func lastUserMessage(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}
