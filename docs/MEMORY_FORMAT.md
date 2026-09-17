# HUPI Portable Memory Format (HPMF v1)

HUPI = "Human-equivalent Personal Interaction [memory]". This document
defines the record schema for a portable, AI-agnostic memory: episodes,
summaries, entities, and the manifest that ties them together.

**Where this schema lives, concretely, depends on which side you're
looking at it from:**

- In the running system, these records are rows in **PostgreSQL** (with
  embeddings in a `pgvector` column) — see
  [ARCHITECTURE.md](../ARCHITECTURE.md) for how the live gateway reads and
  writes them.
- As a **portable interchange/bootstrap format**, the same records serialize
  to the directory tree described below (JSONL for logs, JSON for
  structured records) — produced by `hupi export`, consumed by `hupi
  import`. This is what you'd zip and carry on a flash drive / GDrive / any
  object store, or use to seed a fresh deployment, or hand to something
  that isn't HUPI at all.

The schema is the durable contract between those two forms; neither is
allowed to imply fields the other can't represent.

**Built** (`cmd/hupi-export`, `cmd/hupi-import`, `internal/hpmf`, see
[GAP_CLOSURE_PLAN.md §4.2](GAP_CLOSURE_PLAN.md)) — this document predates
Tier 3's multi-tenancy and originally assumed one deployment, one owner.
Two deltas between what's written below and what actually shipped, both
called out again at the relevant section:

1. **Scope-aware.** Every export is per-scope (`-scope-kind`/`-scope-owner`)
   or whole-deployment (`-all`), not single-owner. `manifest.json` lists
   one or more scopes, each with its own directory; see
   [Directory layout](#directory-layout).
2. **Entities are one file, not one per kind.** `entities.jsonl` with
   `kind` inline, not `people.jsonl`/`projects.jsonl`/etc. — simpler, and
   no file's existence depends on which kind values happen to be in use.

## Design principles

1. **Raw text is the source of truth. Vectors are a cache.**
   Embeddings are tied to whichever embedding model produced them. A bundle
   must remain fully useful even if every vector index is deleted — it can
   always be rebuilt by re-embedding the raw/summary text with whatever
   embedding model is available in the new environment.
2. **Human-readable, diffable, append-only where possible.**
   JSONL for logs (git/rsync-friendly, streamable), JSON for structured
   records. No proprietary binary formats in the core layer.
3. **Content-addressed and hash-verifiable**, so sync/merge across devices
   (laptop, phone, cloud) is conflict-safe and dedup is trivial.
4. **Hierarchical, not flat.** Raw episodes -> daily -> weekly -> monthly ->
   yearly summaries, so retrieval never has to linearly scan years of logs.
5. **Model-agnostic metadata.** Every record tags which model/provider
   produced it, but nothing in the schema assumes a specific vendor.
6. **Correction over mutation.** Nothing that represents history is ever
   silently edited or deleted. If a summary is later found to be wrong, a
   new record supersedes it; the old one and the reasoning for the change
   stay in the bundle. This is what makes it possible to catch and recover
   from a hallucinated "memory" instead of it quietly becoming accepted
   fact — see [Grounding & correction](#grounding--correction).
7. **Encrypted at rest, always — never a plaintext default.** The live
   store (Postgres) is protected by deployment-level disk encryption plus
   application-level field encryption; an exported snapshot of this format
   is itself encrypted, never a bare plaintext directory, because that's
   precisely the artifact most likely to end up on a flash drive or synced
   to the wrong cloud folder. See [Storage & encryption model](#storage--encryption-model)
   for the export-time scheme, and
   [ARCHITECTURE.md § Storage security](../ARCHITECTURE.md) for the live
   store's two-layer model.

## Storage & encryption model

This section covers encryption of an **HPMF export** — the live store's
encryption (deployment-encrypted volume + application-level field
encryption in Postgres) is specified in
[ARCHITECTURE.md § Storage security](../ARCHITECTURE.md), not here, since
it's a property of the running system rather than of this file format.

An export produced by `hupi export` is a point-in-time, infrequently
generated artifact (unlike the live store, which is written continuously),
so a simpler scheme fits it better than trying to keep a directory tree
transparently encrypted under continuous append-only writes:

- The directory tree in [Directory layout](#directory-layout) below is
  written to a temp location and then encrypted as a whole with
  [`age`](https://github.com/FiloSottile/age) to a single `.age` file —
  `hupi-export-2026-09-09.tar.age`. That file, not the raw directory, is
  the thing you'd copy to a flash drive / GDrive / any object store.
- `hupi import` decrypts it to a temp location, loads it into Postgres, and
  deletes the plaintext temp copy — the decrypted form is never the
  long-lived artifact.
- Key management: the `age` recipient/identity used for export is
  independent of the live store's KEK (see ARCHITECTURE.md) — losing it
  makes that specific export unreadable, but the live store (and future
  exports, once you generate a new key) are unaffected. Back it up
  separately from the exports it protects, same rule as any encryption key.
- This means the format's own on-disk shape (JSONL/JSON, human-readable) is
  only ever exposed as plaintext transiently, during an explicit
  export/import, not as a resting state.

## Directory layout

The following is the shape of a **decrypted HPMF export** (see above) — the
temp-location output of `hupi export`, before it's packed into a single
`.age` file, and the input `hupi import` expects after decrypting one. It
is not a directory that exists on disk long-term.

A **per-scope export** (`hupi-export -scope-kind private -scope-owner
user:alice`) is flat, at the bundle root:

```
manifest.json                       # one scope entry — see below
episodes/                           # raw interaction log
└── 2026/
    └── 09/
        └── 2026-09-07.jsonl        # one file per day, one JSON object per line
summaries/                          # hierarchical rollups (all rebuildable)
├── daily/2026-09-07.json           # array of every version ever written for
├── weekly/2026-W37.json            # that (level, period) — a correction's old
├── monthly/2026-09.json            # version and its correction_reason don't
└── yearly/2026.json                # get dropped just because it's exported
entities.jsonl                      # one line per entity, `kind` inline — see below
```

A **whole-deployment export** (`hupi-export -all`) nests one of the above
per scope, keyed by the manifest, plus embeddings are never included
either way (see below):

```
manifest.json                       # one entry per scope, each naming its own dir
scopes/
├── private-user_alice/
│   ├── episodes/...
│   ├── summaries/...
│   └── entities.jsonl
└── shared-team_acme-eng/
    ├── episodes/...
    ├── summaries/...
    └── entities.jsonl
```

Everything here is the durable, portable core, matching the live Postgres
tables 1:1 (`episodes` row per JSONL line, etc.) — minus embeddings,
which are never exported in either shape: they're cheap to regenerate and
tied to whichever embedding model produced them, so exporting them buys
nothing. `hupi-import` always rebuilds the `pgvector` index locally from
`episodes/` + `summaries/` + `entities.jsonl` using whatever embedding
model is configured in the target deployment.

## manifest.json

This is the actual shape `internal/hpmf.Manifest` writes — one `scopes`
entry per exported scope, `dir` naming where that scope's files live
inside the bundle (`.` for a per-scope export, `scopes/<kind>-<owner>`
for whole-deployment, see [Directory layout](#directory-layout)):

```json
{
  "hpmf_version": "1.0",
  "created_at": "2026-09-09T03:00:00Z",
  "created_by": "sujith.samuel",
  "scopes": [
    {
      "scope_kind": "private",
      "scope_owner": "user:alice",
      "dir": ".",
      "episode_count": 48213,
      "summary_count": 1904,
      "entity_count": 62,
      "earliest_episode_at": "2024-01-15T09:03:00Z",
      "latest_episode_at": "2026-09-09T21:41:00Z"
    }
  ]
}
```

Note what's **not** here, versus this document's original single-owner
design: `export_encryption` (the age recipient is never recorded inside
the bundle it protects — that would defeat the point) and
`consolidation_provider_history` (a nice-to-have for "why do summaries
from this period sound different," not built).

## Episode record (episodes/YYYY/MM/YYYY-MM-DD.jsonl)

One line per interaction turn or logical exchange. Append-only; never
mutated in place (corrections are new records referencing the old `id`).

```json
{
  "id": "01J8Z3F9K2Q7T8V2X6C4B1M0N5",
  "ts": "2026-09-09T14:32:10Z",
  "type": "interaction",
  "provider": {
    "vendor": "anthropic",
    "model": "claude-sonnet-5",
    "endpoint": "https://api.anthropic.com/v1/messages"
  },
  "context_tags": ["work", "hupi-project", "architecture"],
  "input_text": "raw user message (or pointer, see 'externalized' below)",
  "output_text": "raw assistant response",
  "importance": 0.6,
  "hash": "sha256:9f3c...",
  "supersedes": null,
  "externalized": false,
  "truncated": false,
  "memory_gate": "full",
  "retrieved_summary_ids": ["sum_2026-09-08_daily_v1"],
  "retrieved_entity_ids": ["project:hupi", "self_model:primary"],
  "retrieved_episode_ids": []
}
```

- `importance` (0-1): a **cheap local heuristic only** (length, presence of
  decision/preference keywords, question density, entity mentions) —
  deliberately *not* an LLM call. Capture must never depend on a network
  round-trip: the record is written synchronously, right after the
  response is finalized and before the turn is considered complete, so a
  crash can never silently drop a completed exchange. Any deeper,
  LLM-based salience judgment happens later during consolidation and is
  attached to the *summary* that references this episode, not by rewriting
  this record. This is what makes capture both durable and fast — see
  [ARCHITECTURE.md § Capture](../ARCHITECTURE.md#capture-durable-and-synchronous).
- `truncated: true` marks a streamed response that was cut off (client
  disconnected, gateway restarted mid-stream) — the partial text is still
  captured rather than lost, just flagged as incomplete.
- `externalized: true` means `input_text`/`output_text` are omitted here and
  stored instead as a content-addressed blob under `episodes/blobs/<hash>`
  (used for large pastes, code dumps, documents) — keeps the JSONL light.
- `hash` lets any two copies of the bundle be merged/deduped by simple set
  union on `id`+`hash`.
- `memory_gate` (`skipped`/`partial`/`full`) plus `retrieved_summary_ids`/
  `retrieved_entity_ids`/`retrieved_episode_ids` record what the retrieval
  engine actually decided and injected for this turn — written at capture
  time, cheap, and the basis for auditing retrieval quality instead of
  guessing at it (see
  [ARCHITECTURE.md § Retrieval observability](../ARCHITECTURE.md)).
  `retrieved_episode_ids` is populated only when vector search matches
  another episode directly (rather than via a summary that folded it in) —
  see [DESIGN_VS_BUILT.md #3](DESIGN_VS_BUILT.md).
- A second `type: "feedback"` record can reference an episode after the
  fact — `{"type": "feedback", "refers_to": "<episode_id>", "rating":
  "memory_wrong", "note": "..."}` — appended like any other line, never
  mutating the original. This is the cheapest possible signal channel for
  "the memory system got this turn wrong," and doesn't require a UI to
  start collecting.
- Raw episodes are **kept indefinitely by default**. Earlier drafts of this
  format allowed pruning low-importance episodes once folded into a
  summary — that's removed: pruning destroys the one thing that lets you
  verify or correct a summary later, which matters more than the storage
  it saves (plain text compresses well; a year of daily interaction is
  realistically low tens of MB compressed). Treat pruning as a manual,
  opt-in operation on episodes older than a chosen horizon, never automatic.

## Summary record (summaries/{daily,weekly,monthly,yearly}/*.json)

```json
{
  "id": "sum_2026-09-09_daily_v1",
  "period": "2026-09-09",
  "level": "daily",
  "status": "draft",
  "generated_by": {"vendor": "anthropic", "model": "claude-sonnet-5"},
  "source_episode_ids": ["01J8Z3F9K2...", "01J8Z3FA..."],
  "summary": "Worked on HUPI architecture: defined portable memory format...",
  "key_facts": [
    {
      "fact": "Decided to use PostgreSQL + pgvector as the vector index",
      "source_episode_ids": ["01J8Z3FA..."],
      "grounded": true
    },
    {
      "fact": "Working directory for the project is ~/repos/hupi",
      "source_episode_ids": ["01J8Z3F9K2..."],
      "grounded": true
    }
  ],
  "entities_touched": ["project:hupi"],
  "grounding_checked": true,
  "supersedes": null,
  "correction_reason": null
}
```

Weekly/monthly/yearly summaries are generated *from lower-level summaries*,
not from raw episodes — this is what keeps retrieval over years of history
bounded instead of linear. Their `source_episode_ids` field is omitted in
favor of `source_summary_periods`; a fact can always be traced back to raw
text by walking that chain down to daily summaries and from there to
episode ids.

### Grounding & correction

Summaries are LLM output, and LLM output can fabricate a plausible-sounding
"fact" that has no basis in the source text — especially several hops up
the daily→yearly chain, where each level is summarizing a summary rather
than the original record. Left unchecked, a fabrication at the monthly
level looks identical to a true fact, gets folded into the yearly rollup,
and from then on is retrieved and presented with full confidence. Two
mechanisms address this:

1. **Per-fact citations instead of a citation-free paragraph.** Every
   entry in `key_facts` must carry its own `source_episode_ids` (or, above
   the daily level, a path resolvable to them). The free-text `summary`
   field is allowed to be more loosely written prose — it's for human
   skimming — but anything meant to be *retrieved and treated as fact*
   lives in `key_facts` and must be citable.
2. **A second-pass grounding check**, run immediately after a summary is
   generated: a separate LLM call (can be a cheaper/smaller model) is given
   only the source text and the generated `key_facts`, and asked to flag
   any fact not actually supported by the source. Facts that fail are
   marked `"grounded": false` and excluded from retrieval until reviewed —
   they aren't deleted, since a false-negative on the grounding check
   shouldn't destroy real information, but they don't get treated as
   trusted memory either. `grounding_checked: true` records that this pass
   ran at all.

If a problem is found later anyway (you notice the AI "remembers" something
wrong), the fix is never to edit the summary file in place — consistent
with the append-only principle. A new summary record is written for the
same `period`/`level` with an incremented id, `supersedes` pointing at the
old record's `id`, and `correction_reason` filled in. Retrieval always
resolves to the latest non-superseded version for a given period, but the
wrong version and *why it was wrong* stay in the bundle rather than
disappearing — which is itself useful signal for tuning the grounding check
later.

**Operator caveat: re-consolidating a day after correcting it.** A daily
summary can be regenerated more than once — new episodes keep arriving
through the day, and `hupi-consolidate` re-running for that date
regenerates from *all* of that day's episodes and supersedes whatever
draft was current (rather than leaving two rows both current for the same
day). Because that regeneration prompt is told the existing draft is an
"already-established record" and to trust it unless a raw episode
explicitly overrides it, a **correct** correction survives being
re-consolidated on top of. But the same trust means an already-**wrong**
record does not self-heal: if a draft is corrupted (a bad correction, a
grounding-check gap, or — as happened during development, before this
established-record handling existed — a re-consolidation that misread its
own earlier, correct answer as an unverified hallucination and quietly
walked it back), every subsequent re-consolidation will carry that error
forward as settled, not catch or fix it. `hupi-consolidate` is not a
self-healing mechanism for its own past mistakes. If you notice a wrong
fact, correct it with `hupi-correct` (`-dump-template` first, to see
today's actual current content before you edit it) — don't assume the
next scheduled consolidation run will notice and fix it on its own.

Optionally, `status` can be used as a lightweight human-review hook:
summaries are written as `"draft"`, and a periodic digest (weekly, say)
surfaces newly generated daily/weekly summaries for a quick skim; accepting
one flips it to `"reviewed"`. This is off by default (retrieval treats
`draft` and `reviewed` the same) but gives you a place to plug in review
without redesigning the format later, and is most worth turning on for
monthly/yearly rollups, where compounding drift risk is highest.

## Entity record (entities/*.jsonl)

Long-lived facts that don't belong to a single point in time (a knowledge
graph, kept separate from the episodic timeline so it isn't diluted by
consolidation).

```json
{
  "id": "project:hupi",
  "kind": "project",
  "name": "HUPI",
  "first_seen": "2026-09-08",
  "last_updated": "2026-09-09",
  "attributes": {
    "description": "Personal portable AI memory system",
    "repo": "~/repos/hupi"
  }
}
```

### The `self_model` entity (special case)

`entities/self_model.json` is a singleton, `kind: "self_model"` entity that
is **hand-written and hand-edited by you, not generated by consolidation**.
It holds the stable stuff that shouldn't be reconstructed differently every
time an LLM re-summarizes your history — voice/communication preferences,
values, standing instructions ("terse responses," "always ask before X").
It's small by design and is always included in retrieval's fixed-size
anchor (see [ARCHITECTURE.md § Retrieval Engine](../ARCHITECTURE.md)),
regardless of gating, so presentation stays consistent across whichever
chat model or consolidation model happens to be active.

```json
{
  "id": "self_model:primary",
  "kind": "self_model",
  "name": "sujith",
  "last_updated": "2026-09-09",
  "attributes": {
    "communication_style": "terse, direct, skip trailing summaries",
    "standing_preferences": [
      "confirm before destructive git operations",
      "prefer editing existing files over new ones"
    ]
  }
}
```

## index/build.json (rebuild provenance, not portable data)

```json
{
  "embedding_model": "text-embedding-3-large",
  "dims": 3072,
  "built_at": "2026-09-09T03:05:00Z",
  "record_count": 48213,
  "chunking": "one vector per summary record + one per high-importance episode"
}
```

## Portability workflow

The live system's day-to-day backup path is a Postgres-native one
(`pg_dump`), not this format — see
[ARCHITECTURE.md § Portability workflow](../ARCHITECTURE.md). This format's
own workflow is for the less frequent case: moving to a new deployment,
handing data to something that isn't HUPI, or deliberately carrying a
snapshot outside the database.

1. `hupi-export -scope-kind private -scope-owner user:alice -recipient age1...`
   reads Postgres, writes the [Directory layout](#directory-layout) above
   to a temp location, packs it, and encrypts it to the given `-out` path
   (see [Storage & encryption model](#storage--encryption-model)) —
   deleting the plaintext temp copy once encryption succeeds. This file is
   what you'd copy to a flash drive, GDrive, S3, whatever. `-all` instead
   of `-scope-kind`/`-scope-owner` exports every scope in the deployment
   into one bundle.
2. On a new deployment: `hupi-import -in alice.age -scope-kind private
   -scope-owner user:alice -identity alice-key.txt`, which decrypts to a
   temp location and loads `episodes/`/`summaries/`/`entities.jsonl` into
   Postgres — `-all` restores every scope a whole-deployment bundle
   contains, provisioning each user/team first if it doesn't already
   exist. `-merge` allows importing into a scope that already has data
   (dedup by hash/id/period — see `internal/hpmf`'s doc comments for the
   exact rule); without it, the target scope must be empty.
3. Point HUPI at *any* OpenAI-compatible endpoint (see
   [ARCHITECTURE.md](../ARCHITECTURE.md)). `hupi-consolidate` only ever
   embeds a summary the moment it's freshly written, or an episode
   captured on the specific day it processes, neither of which applies to
   rows that just arrived via `hupi-import` — they land with `embedding`
   still null. Retrieval still works immediately for anything the
   two-stage gate matches by keyword/entity-name (ARCHITECTURE.md §
   Retrieval Engine), but imported content won't surface via vector
   search until it's been embedded: run `hupi-reembed -scope-kind
   <kind> -scope-owner <id>` after an import to backfill it (see
   `internal/reembed`'s doc comment) — the same tool that handles a
   changed `active_embedding_provider` also covers a never-embedded row,
   since both cases are "this row's embedding isn't current." Whatever
   embedding model you point HUPI at needs to end up producing exactly
   1536 dimensions — natively, by supporting an explicit request for that
   size, or (most local/offline models) by being shorter and getting
   zero-padded up automatically — HUPI checks this at startup and refuses
   to run rather than fail partway through a backfill; see ARCHITECTURE.md § Provider
   abstraction and docs/INSTALL.md's Troubleshooting section.

Treat both the `age` identity for exports and the live store's field
encryption KEK (see ARCHITECTURE.md) like a password manager vault: either
one is the only thing protecting years of personal history in its
respective form, and unlike a forgotten website password, there's no
"reset" path if you lose one.
