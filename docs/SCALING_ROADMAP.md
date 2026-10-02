# Scaling roadmap — Postgres/pgvector past single-instance scale

Status: **items 1-2 done and merged (PR #86)**. Items 3-5 are deliberately
**not yet built** — recorded here, in order, as the next options for when
real usage data shows the per-scope governance fix (1-2) isn't enough on
its own. See [PROCESS_REFERENCE.md](PROCESS_REFERENCE.md) for how
retrieval and consolidation actually work today; this document is about
what changes if a deployment's own Postgres instance becomes the
bottleneck, not about the retrieval algorithm itself.

## 1. Background

Every text field HUPI stores is encrypted at rest, application-level
(see [ARCHITECTURE.md](../ARCHITECTURE.md) § Storage security). Vector
search tolerates this fine — pgvector's HNSW indexes
(`summaries_embedding_idx`, `episodes_embedding_idx`,
`entities_embedding_idx`) operate on the embedding column directly, not
on the encrypted content, so similarity search stays index-accelerated
regardless of corpus size. BM25 keyword search doesn't have that luxury:
Postgres can't build a full-text index over ciphertext, so it decrypts
and live-scores every matching row in scope on every call that has real
query terms. That's the one genuinely corpus-size-linear cost in the
whole retrieval path — fine at personal/team-history scale, a real
concern once a single deployment serves many users with growing per-scope
history (the scenario this plan was written against: a ~200-user Tier 3
deployment).

Five options were considered, ranked by how invasive they are. Don't
reach for a later one until an earlier one is proven insufficient — in
particular, don't reach for 5 unless 1-4 genuinely don't hold, since it's
the only option with a real, deliberate security regression attached.

## 2. Done: per-scope BM25 governance (items 1 and 2)

Merged as one feature, PR #86. Full design and the real file/line
references live inline in `internal/store/retrieve.go`'s own doc
comments and in [PROCESS_REFERENCE.md](PROCESS_REFERENCE.md) § 2.2.4's
"Per-scope governance" section — summarized here just enough to explain
why items 3-5 below are framed the way they are.

**Item 1 — make the existing kill switch per-scope, not global.**
`HUPI_ENABLE_KEYWORD_SEARCH` was already a deployment-wide on/off switch.
The real fix: track corpus size per scope (`scope_corpus_size`, refreshed
once a day by `internal/consolidation.Runner.RunDaily` →
`updateScopeCorpusSize`, for any scope with new episodes that day) and
let retrieval decide, per call, how expensive keyword search is allowed
to be *for that scope* — not for the whole deployment at once. Most
scopes never cross the threshold; the ones that do are exactly the ones
where live-scoring everything gets expensive.

**Item 2 — pre-filter before paying the decrypt cost, using what's
already plaintext.** `summaries.entities_touched` is a plain `text[]`,
not encrypted. Before BM25's full decrypt-and-score pass over every
summary in scope, the narrowing tier adds
`and entities_touched && $N::text[]` — a plaintext array-overlap check
Postgres evaluates before any row is decrypted — restricting the
candidate set to summaries that share an already-known entity with the
query (whatever stage 1 matched). This doesn't change BM25's scoring at
all, just shrinks what it has to decrypt per query for a busy scope. When
stage 1 found no entity to narrow by, this falls back to the exact same
full scan as today, rather than silently dropping recall for the "rare
term with no entity anchor" case BM25 exists to catch.

The resulting three tiers — `full` / `narrowed` / `disabled` — are
decided once per `retrieve()` call by `keywordSearchTierForScope`. New
metric `hupi_keyword_search_tier_total` (labels `full`/`narrowed`/
`disabled`, deliberately no scope-owner label) gives a real,
deployment-wide tier distribution. **Check this metric first** before
deciding any of items 3-5 below are actually warranted — it's the
evidence that would justify the next step, not a guess.

## 3. Read replica for the retrieval path specifically

Retrieval (`internal/store/retrieve.go`) is already a clean, read-only,
two-transaction flow (`dbscope.Run`, split around the one embedding
network call) — nothing about it requires the primary. Routing
retrieval's reads to a streaming replica while keeping
capture/consolidation writes on the primary would let decrypt-and-score
load scale independently of write load.

RLS policies replicate fine (they're just SQL objects); session
variables (`hupi.acting_scope_kind`, etc. — see `dbscope.Run`) are
per-connection, so they work identically against a replica connection as
against the primary.

## 4. Shard by scope once a single Postgres genuinely can't hold it

The one option that actually removes the ceiling rather than pushing it
out further. Every query in the live path is already scope-filtered and
RLS-isolated with zero cross-scope reads — there's no architectural
reason all of a deployment's scopes need to live in one Postgres
instance. Hash `scope_owner` to one of N shards, route at the connection
layer.

The one real catch: `cmd/hupi-consolidate`'s `loadActiveScopes` currently
assumes one DB connection iterating every scope. Sharding means running
it once per shard instead — a small, mechanical change to that one
entry point, not a redesign of consolidation itself.

## 5. Searchable/deterministic encryption — last resort

The root tension is encryption vs. indexability. A searchable-encryption
scheme (deterministic or blind-index-style, à la CipherSweet) could give
Postgres something it can index without full plaintext — but
deterministic encryption leaks term-frequency patterns, a real,
deliberate security regression from what HUPI has today, not a free
upgrade. Treat this as a last resort if items 3-4 genuinely don't hold,
not a first move.
