# Entity Relationships Plan — closing the "limited temporal/relationship reasoning" gap

Prompted by an external comparison (ChatGPT's own read of HUPI vs.
Mem0/Zep-Graphiti/Letta/Supermemory/Hindsight) rating HUPI "⚠️ Limited"
on temporal/relationship reasoning, next to Zep/Graphiti's "✅ Core
strength" and Mem0's "✅ Graph option." Checked against the actual
schema and code rather than taken on faith — the rating is accurate.
This plan is the design for closing it, sequenced to land *before* the
real cloud-model LoCoMo/LongMemEval benchmark run (see the benchmark
harness plan), since LoCoMo's own "multi-hop" question category is
exactly where this gap already showed up in real (local-model) testing.

## 1. What's actually missing, confirmed against the real code

- **No relationship/edge concept at all.** `entities` (`schema/0001_init.sql`)
  is a flat record — `id, kind, name, first_seen, last_updated,
  attributes`. Nothing connects two entities to each other. Zep/Graphiti's
  entire architecture *is* a temporal knowledge graph; this is the single
  biggest structural gap.
- **Entity attributes are last-write-wins, not time-versioned.**
  `mergeAttributes` (`internal/consolidation/store.go`) does exactly
  `merged[k] = v` — the old value of a changed attribute is simply gone,
  with no record of what it was before or when it changed.
- **What already exists and isn't being thrown away**: summary-level
  temporal history is real — consolidation is date-bucketed
  (daily/weekly/monthly/yearly) and corrections use `supersedes`/
  `correction_reason` to retain a superseded version rather than deleting
  it. This plan extends that same "supersede, don't destroy" philosophy
  down to the relationship level, rather than inventing a different one.

## 2. Non-goals

- **Not a general-purpose graph database.** No Cypher-like query
  language, no arbitrary-depth graph traversal exposed to clients. Just
  enough structure to answer "how are these two entities connected" and
  "what changed and when" — the two concrete gaps identified above.
- **Not replacing vector/keyword retrieval.** Relationship traversal is
  an *addition* to existing retrieval (BM25 + vector + entity match), not
  a replacement — most queries will still be answered the existing way.
- **Not modeling every possible relationship type up front.** Starting
  with whatever predicates consolidation actually extracts from real
  conversations (free text, lightly sanitized — see §4), not a
  hand-curated taxonomy decided in advance.

## 3. Schema

```sql
create table entity_relationships (
    id                 text primary key,
    scope_kind         text not null,
    scope_owner        text not null,
    subject_id         text not null references entities(id),
    predicate          text not null,   -- "works_at", "married_to", "friends_with", ...
    object_id          text not null references entities(id),
    valid_from         date,            -- null = unknown when it started
    valid_until        date,            -- null = still current
    source_summary_id  text references summaries(id),
    created_at         timestamptz not null default now()
);

create index entity_relationships_subject_idx on entity_relationships (scope_kind, scope_owner, subject_id);
create index entity_relationships_object_idx  on entity_relationships (scope_kind, scope_owner, object_id);
```

Same RLS pattern every other scope-owned table already uses
(`schema/0005_hardening_phase3_rls.sql`) — mechanical to add, not a new
design.

**Open question flagged, not resolved by this doc**: `predicate` as free
text vs. a constrained enum, same tension `entities.kind`'s existing
7-value enum already has. Given the entity-kind validation bug fixed
this session (models inventing kinds like `"conversation"`/`"family"`
outside a *7-value* enum), a predicate enum — which would need to cover
a much larger and genuinely open-ended space of relationship types — is
very likely a worse version of the same problem. Leaning toward free
text with light sanitization (non-empty, length-capped, lowercased) plus
the same "skip and log, don't fail the whole day" degrade this session
already added for invalid entity kinds — but this should be confirmed
against a few real consolidation runs before committing to it, not
assumed.

**Encryption boundary, decided by precedent**: `subject_id`/`object_id`/
`predicate` stay unencrypted, matching `entities.kind`/`entities.name`
today — they're structural, needed for indexing and joining. This is a
continuation of an existing security-model choice, not a new one being
made here.

## 4. Extraction (consolidation prompt + parsing)

A new `"relationships"` array in `ConsolidationOutput`, parallel to
`entities_touched`:

```json
{"subject": "kind:slug", "predicate": "...", "object": "kind:slug", "valid_from": "...", "valid_until": "..."}
```

This is a harder extraction task than flat attributes — the model has to
correctly identify *two* entities, canonicalize both IDs (reusing
`canonicalEntityID`), infer a sensible predicate, and reason about
temporal bounds, all in one pass. Given that the *simpler* flat-attribute
case just needed four distinct robustness fixes (lenient attribute-value
coercion, kind validation, grounding count mismatch, malformed-JSON
retry — all landed this session), this should budget for at least that
much hardening work from the start, not as a follow-up once problems
show up in production. Concretely: reuse the same lenient-parsing +
skip-invalid-and-log pattern for a bad relationship entry, rather than
letting one bad edge fail an entire day's consolidation the way one bad
entity used to.

## 5. The genuinely hard part: supersession, not insertion

Inserting a new edge is easy. The real design risk is recognizing that a
*new* fact conflicts with an *existing* one — "X now works at CompanyY"
should close out an open-ended "X works at CompanyX" edge
(`valid_until`), not just add a second, permanently-contradictory row
sitting next to it forever.

Two approaches, not yet chosen between:
- **Model-driven**: have the LLM explicitly cite what it's superseding,
  mirroring how `hupi-correct` already works for facts — more accurate,
  but adds yet another thing the extraction step has to get right.
- **Code-driven heuristic**: same `(subject, predicate)` pair, different
  `object` → close the old edge automatically. Simpler and doesn't
  depend on the model remembering to flag it, but heuristics here are
  exactly where subtle, hard-to-notice bugs live (e.g. a person can
  validly have *multiple* concurrent `friends_with` edges — the "same
  subject+predicate implies supersession" heuristic is wrong for
  one-to-many relationship types and right for one-to-one ones, so it
  can't be a single global rule).

This needs its own real-data testing pass before implementation, the
same way the consolidation robustness fixes this session were verified
against genuine LLM output rather than assumed correct from reading the
code.

## 6. Retrieval — a bounded graph walk

`internal/store/retrieve.go` gets a new step after existing entity
matching: walk 1–2 hops out along matched entities' relationship edges,
surfacing connected facts the query never named — this is what actually
enables a real multi-hop answer (LoCoMo's category 1) instead of relying
on the connecting fact happening to appear in the same retrieved
summary's prose.

Needs its own hop-limit and token budget, same concern as the existing
`maxVectorResults` cap — an ungated walk on a densely-connected scope
could blow up context size fast. The retrieval gate (`skipped`/`partial`/
`full`) needs a new signal for "a relationship-graph hit contributed,"
alongside the existing vector/keyword/entity ones.

## 7. Verification

- Real consolidation runs (same Docker-Postgres + real-Ollama pattern
  used throughout this session) against conversations with known,
  hand-checked relationships — not just unit tests against synthetic
  fixtures — to see actual extraction failure rates before assuming the
  design is sound.
- A dedicated multi-hop retrieval test: a scope where the answer to a
  question requires walking through at least one intermediate entity,
  confirming the graph walk actually surfaces it and that a *disabled*
  version of the same query (graph walk off) does not — proving the
  feature does something, not just that it doesn't crash.
- Re-run the LoCoMo benchmark harness's multi-hop (category 1) questions
  specifically, before and after, as the real before/after signal this
  whole plan exists to move.

## 8. Sequencing relative to the benchmark harness plan

This is intentionally being designed *before* Step 4 of the benchmark
harness plan (the real cloud-model LoCoMo/LongMemEval run) — the goal is
for that real, published number to reflect HUPI with this gap already
closed, not to publish a number now and re-run everything later once
this lands.
