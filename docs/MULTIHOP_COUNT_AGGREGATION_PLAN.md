# Plan: fix LoCoMo multi-hop counting/enumeration failures

## Context

[BENCHMARKS.md §8](BENCHMARKS.md) already logged a narrower version of
this finding (2026-10-04, `conv-30` calibration run): a multi-hop
question's real answer was correctly extracted and grounded across 5
different days, but the predicted answer only got one of those right
and filled in the rest from a topically-adjacent distractor thread —
flagged as "likely needs a real design pass... not attempted yet."

Digging into the full 10-conversation re-run's 45 zero-score multi-hop
(category 1) misses found this is far more pervasive than that one
example suggested: the dominant failure shape is **counting questions**
— "How many video game tournaments has Nate participated in?" (gold:
nine, predicted: "at least four"), "How many tournaments has Nate won?"
(gold: seven, predicted: "Six"), "How many letters has Joanna
received?" (gold: Two, predicted: "Not mentioned") — almost always an
undercount, across nearly every conversation in the dataset.

## Root cause (confirmed via direct code/data inspection, not guessed)

Two independent mechanisms are both involved, with different severity:

**1. `entities_touched.attributes` is structurally unsuited for
counting, and is demonstrably lossy.**

`entities.attributes` is a single encrypted blob holding a flat
`map[string]string` (`schema/0001_init.sql:138-146`,
`internal/consolidation/types.go:56`). The merge after each
consolidation pass is a dumb flat-map overlay —
`mergeAttributes` (`internal/consolidation/store.go:786-798`):

```go
func mergeAttributes(existing, incoming map[string]string) map[string]string {
	if existing == nil {
		return incoming
	}
	merged := make(map[string]string, len(existing)+len(incoming))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range incoming {
		merged[k] = v
	}
	return merged
}
```

New keys win, untouched keys survive. The LLM *does* see an entity's
existing attributes before deciding what to write this pass
(`findKnownEntities`, `internal/consolidation/supersession.go:43-85`,
interpolated into the prompt at `internal/consolidation/prompts.go:66-80`),
and can explicitly rename/supersede a key via `SupersedesKeys`
(`store.go:448-454`) — but that mechanism is documented and used only
for *renaming the same fact* (e.g. a corrected dollar figure), never
for "this is the Nth occurrence of a repeating event." Facing a new
tournament win with no existing key to supersede, the model invents a
fresh, uniquely-named scalar key (`fourth_video_game_tournament_win_date`,
`big_tournament_win_date`, `won_international_gaming_tournament_date`,
...) — and `mergeAttributes` just piles these on indefinitely. There is
no instruction anywhere in `summarySystemPrompt`
(`prompts.go:13-44`) telling the model how to name or accumulate these,
so naming is ad hoc and non-enumerable: you cannot reliably compute
"how many" from a bag of inconsistently-named scalar keys.

Confirmed empirically: a real export of `person:nate` (conv-42) shows
attributes implying **6** distinct tournament wins (gold says 7), and
one of them — `fourth_video_game_tournament_win_date: "2022-07-08"` —
has **no corresponding `summary_key_facts` entry anywhere in the scope**
(checked both a substring search across all 32 summaries' key_facts and
the summaries immediately surrounding that date). It exists only as an
attribute, uncorroborated by anything else stored for this scope —
either hallucinated, or extracted from raw dialogue that was never also
captured as a key_fact. Either way, nothing downstream can recover it
reliably.

**2. `summary_key_facts` is the mechanism that actually works for this
— the real gap is that nothing retrieves every instance for a counting
query.**

Unlike attributes, `summary_key_facts` already has the right
instruction (`prompts.go:34`): "When a source describes several
different dated milestones about one underlying story, case, or
project... extract each milestone as its own separate key_fact... do
not drop an earlier milestone in favor of a later one." And it's
already a genuinely accumulating, append-only structure — each
key_fact is its own row (`schema/0001_init.sql:78-121`), not a
flat-map key.

Checking this directly against the same `person:nate` scope: of the
entity attributes' implied wins, **4 are independently confirmed
present as grounded `summary_key_facts`** (the CS:GO win, the Street
Fighter win, the "big" tournament win, the Valorant final win — each
with its own dated, `grounded: true` key_fact, sometimes re-confirmed
across two consolidation passes after an initial `grounded: false`
draft). This is real, working raw material — the problem is that
nothing at answer time asks for *every* key_fact belonging to this
entity and this event type. Retrieval is tuned for top-K relevance
(fused vector+keyword ranking, then MMR diversity selection) for a
single broad query like "how many tournaments has Nate won" — not for
exhaustive enumeration — so the model only ever sees a bounded subset
and undercounts from a sample it has no way to know is incomplete.

**This is a different bottleneck than the one already ruled out for
LongMemEval's analogous category.** `docs/LONGMEMEVAL_ACCURACY_PLAN.md`
records that a generic "query-shape detection (ordering/counting) +
widened/less-diversity-penalized retrieval" was tried and reverted for
LongMemEval's temporal-reasoning category — real tracing proved the
bottleneck there was upstream, at consolidation completeness, not the
retrieval-selection stage that fix targeted. That finding doesn't
transfer automatically here: that case had facts genuinely *missing*
from storage; this case (per the direct check above) mostly has facts
*present* in `summary_key_facts` and just not all retrieved. The fix
needs to be targeted and verified the same rigorous way, not assumed
to work or assumed to fail by analogy.

## Proposed design — two parts, different effort/leverage

**Part A — attempted twice: the first failed and was reverted, the
second (the originally planned exhaustive per-entity fetch) is now
built, tested, and verified — see the status update after Attempt 2
below.**

*Attempt 1 (tried, reverted): reuse the existing ordering-query
retrieval-widening mechanism.* Added `looksLikeCountingRequest`
(mirroring `looksLikeOrderingRequest`'s own keyword-substring pattern)
and wired it into the same `orderingSummarySimilarityThreshold`/
`orderingSummaryMaxResults` widening already used for ordering
questions — zero new retrieval code, just a broader trigger. A full
10-conversation re-run (`locomo_full10_counting_fix_predictions.json`)
found this **regressed** the 43 real counting-shaped questions in the
dataset from 43.3% to 38.0% mean F1, not the hoped-for improvement.
Diffing individual predictions found a genuine, concrete harm, not just
noise: "How many people attended the gaming party hosted by Joanna in
June 2022?" is actually a category-5 adversarial question (Nate hosted
the party, not Joanna) that happens to contain the surface phrase "how
many" — the widened retrieval pulled in Nate's party details, and the
answer misattributed them to Joanna instead of correctly abstaining
(flipped from a correct "not mentioned" to a confident wrong answer).
A blunt, phrase-triggered widening doesn't distinguish "genuine
counting question" from "adversarial question that happens to share
the same surface words," and the cost of that confusion lands
specifically on the category (adversarial) most sensitive to it. This
approach was reverted, not shipped — see
`docs/BENCHMARKS.md` for the full numbers.

*Attempt 2 (built and verified — see `docs/BENCHMARKS.md` §12): exhaustive
key_fact fetch for a resolved named entity.* The plan here: detect a
counting question, resolve the named entity via `stage1EntityMatches`'s
already-computed match, fetch every `grounded = true` `summary_key_facts`
row for that entity (decrypt-then-filter in memory, mirroring
`keywordSearchEpisodes`'s existing pattern for encrypted content), and
present the result as an explicit, deduplicated, numbered list rather
than prose. Narrower-gated than Attempt 1 as planned: requires both the
counting shape *and* a specific resolved entity, so it doesn't fire on
the Joanna/Nate adversarial case above (confirmed directly — that case
still correctly abstains with this feature on).

Implemented as `looksLikeCountingRequest` + `exhaustiveKeyFactsForEntity`
(`internal/store/retrieve.go`), with one addition the original plan
didn't anticipate: restricted to `level = 'daily'` summaries, because an
unrestricted scope-wide scan has its own double-counting bug — a
weekly/monthly/yearly rollup independently re-extracts its own key_facts
from its daily sources' text, so it would multi-count the same
occurrence once per rollup level it survives into. Five real-Postgres
integration tests cover this plus grounded-only, superseded-summary
exclusion, and exact-text dedup (`internal/store/counting_retrieve_test.go`).
Verified against the real, already-studied `conv-42`/`person:nate` scope
(the mechanism returns exactly what's grounded and stored, nothing
duplicated, nothing missed) and against a full from-scratch re-ingest
(real, question-by-question improvement on counting questions, no
adversarial regression). Merged via PR #109; a full 10-conversation
re-run confirming no regression at scale is in progress as of this
writing.

**Part B (smaller, consolidation-prompt-only, closes the
attribute/key_fact inconsistency gap): extend the existing milestone
instruction to explicitly cover repeating/countable events.**

Added a sentence to the existing `prompts.go:34` milestone paragraph
(already proven to work most of the time): "each occurrence of the
same type of repeating event for one entity (a tournament win, a trip,
a purchase) must get its own separate key_fact every time it comes up,
even if you also update a summary attribute about it" — closing the
specific gap found above where the attribute-extraction and
key-fact-extraction outputs of the *same* consolidation pass can
disagree (the `fourth_video_game_tournament_win_date` case, with zero
key_fact backing anywhere).

**Tried and reverted — real-infra check found a confound that makes
this un-shippable without much more investment than reasonable for one
backlog item.** Unlike Part A, this changes consolidation-time
behavior, which a `-answer-only` re-run (reusing already-consolidated
scopes) can't exercise at all — verifying it needs a real re-ingestion
from scratch. Did exactly that for `conv-42` (wiped its scope, rebuilt
with the prompt change, ran a full fresh replay + consolidation +
answer cycle, not `-answer-only`). Result was **worse**, not better:
"How many tournaments has Nate won?" (gold: seven) went from "6" (the
already-shipped baseline) to "2"; "...won by July 10, 2022?" (gold:
Four) went from a correct "Four" to "2".

Tracing why found a genuine confound, not a flaw in the prompt wording
itself: two of the events the *original* investigation treated as
confirmed wins — the international tournament (2022-08-21) and the
Valorant final (2022-11-05) — had **zero key_fact backing** even in
that original run (noted in Part A's root-cause section above as
"either hallucinated, or extracted from raw dialogue never also
key-facted"). This fresh, independent re-extraction consistently read
the *same* international-tournament event as a **loss**
("did not do well"), backed by real grounded key_facts this time, with
no mention of a Valorant win anywhere. That casts real doubt on whether
those were genuine wins the original run under-captured, or
attribute-extraction hallucinations that were never real — i.e. the
gold count of seven itself may not be reliably reconstructable from
what this specific conversation's text actually supports, independent
of any prompt fix. Confirming this one way or the other would mean
reading the raw LoCoMo source conversation directly rather than trusting
either run's extraction, and running several more independent
re-ingests to separate real signal from this demonstrated run-to-run
non-determinism — a real, open question, but a much bigger investment
than fits a single backlog item. Not shipped; the code change was
reverted. If this is revisited, start by reading the raw conversation
text for `conv-42` directly to establish real ground truth on these two
specific tournaments before trying another prompt change.

**Follow-up (this session): raw text read directly — not ambiguous, gold
of seven is correct. Root cause of the run-to-run disagreement found and
partly (not fully) fixed.**

Read `bench/data/locomo/data/locomo10.json`'s `conv-42` conversation
turns directly (not either prior run's extraction) around both disputed
dates. Neither is ambiguous:

- Session 19 (dated 2022-08-22): "Woah Joanna, I won an international
  tournament yesterday! It was wild." — Joanna: "Congrats, Nate! So proud
  of you for winning that tournament." Unambiguous win, dated 2022-08-21.
- Session 27 (dated 2022-11-07): "I was in the final of a big Valorant
  tournament last Saturday, and I won! It was the best feeling to see my
  name as the champion." — Joanna: "Congrats on winning the tournament."
  Unambiguous win, dated 2022-11-05.

Reading every "tournament" mention across all 29 sessions end to end
reconstructs exactly **seven** explicit, unambiguous wins (gold is
correct, fully reconstructable from the raw text, not an unreliable
annotation): (1) CS:GO, session 1, ~2022-01-14; (2) a local Street
Fighter tournament explicitly called his "second," session 10,
~2022-04-25; (3) "another regional" tournament, session 14, ~2022-05-27;
(4) explicitly his "fourth," session 17, 2022-07-08; (5) the
international tournament above, 2022-08-21; (6) a "really big" tournament
he made significant money from, session 22, ~2022-09-29; (7) the Valorant
final above, 2022-11-05. A separate, genuine non-win — "didn't make it to
the finals," session 28, 2022-11-09 — is textually distinct from the
Valorant win two days earlier and was never confused with it in any run
traced this session.

**Verdict: the prior run's "read the international tournament as a
loss" was a real extraction hallucination, not a defensible alternative
reading of ambiguous text.** This retracts this doc's own earlier
"the gold count of seven may not be reliably reconstructable" framing —
that framing was built on trusting two runs' own extractions against
each other, never the source itself; reading the source resolves it.

**A real, previously-unexamined mechanical contributor: no consolidation
LLM call anywhere set `Temperature`.** `generateSummary`
(`runner.go`), `groundingCheckOneAttempt` (`grounding.go`),
`extractPerEpisodeFacts` (`perepisode.go`), and
`checkOneRelatedSummary`'s contradiction check (`contradiction.go`) all
left `provider.ChatRequest.Temperature` nil, falling back to the
provider's own default (1.0 for both OpenAI's and Anthropic's chat
APIs) — tuned for creative variety, not for "did the source text state
X." This is consistent with, and likely a real contributor to, the
non-determinism this doc already documented: three different
investigation sessions' own from-scratch re-ingests of this identical,
unmodified `conv-42` scope landed on three different "tournaments won"
counts (2, this doc's Part B section's "6," and this follow-up's own
"4," below) with no prompt change between them.

**Fixed (shipped this round): pinned `consolidationTemperature = 0.0`
across all four call sites above.** Real-infra verification, same
discipline as Part A — a full from-scratch re-ingest of `conv-42` (not
`-answer-only`) with the fix applied, checked against the real raw-text
ledger above via `hupi-export-memory`:

- **4 of 7 real wins now land as grounded `summary_key_facts`** — CS:GO,
  the Street Fighter win (worded as "another regional... tournament,"
  correctly dated 2022-05-02), the "really big" tournament (correctly
  dated 2022-10-06), and the Valorant final (correctly dated and
  reconfirmed across three separate consolidation passes, 2022-10-21/
  11-04/11-07) — up from 2 of 7 in the established baseline mechanism
  check (`docs/BENCHMARKS.md` §12).
- **The specific prior failure mode — confidently asserting the wrong
  outcome — did not reproduce.** The international tournament is not
  asserted as a loss anywhere in this run; it's absent from key_facts
  entirely (see gap below) but entity attributes still correctly record
  `international_tournament_win_date: 2022-08-21`, and no key_fact
  anywhere in the scope claims Nate lost it.
- **3 of 7 real wins still don't land as grounded key_facts — a
  different, more specific gap than "the model doesn't know to extract
  every occurrence."** Two distinct, concrete bugs, not non-determinism:
  - *Misdating*: the "fourth" win (2022-07-08, session 17) gets drafted
    as a key_fact attached to the wrong day (2022-06-03, session 14's
    day, which never mentions it) — correctly rejected by the grounding
    check since that day's source doesn't support it — while session
    17's own day either omits the event or (once, in this run)
    hallucinates the opposite outcome ("experienced a setback... did not
    do well"), which the grounding check also correctly rejects. Net:
    the real win is never recorded as a key_fact, but it's also never
    wrongly asserted as a loss — the grounding safety net is working,
    the upstream drafting isn't.
  - *Selective omission*: the international tournament's own day
    (2022-08-22) never attempts a key_fact naming the event at all —
    its key_facts for that day include a vague paraphrase ("Nate is able
    to make a living from gaming and is passionate about it") that drops
    the concrete, checkable "won an international tournament yesterday"
    sentence it was paraphrasing. The June "another regional" win
    (session 14) is missing from both key_facts and entity attributes in
    this run — a clean miss, not a misattribution.

**Recommendation: do not re-attempt the reverted "each occurrence must
get its own key_fact" prompt sentence yet.** That wording targets "the
model doesn't realize a repeating event needs its own fact" — but the
gap this follow-up found is different and more specific: event-to-day
misattribution, and a paraphrase silently standing in for a specific
fact within the correct day. A future attempt should target those two
mechanisms directly (why does a drafted fact sometimes cite the wrong
day's sources, and why does `generateSummary` sometimes keep only the
vaguer of two candidate phrasings for the same sentence), and now that
`consolidationTemperature = 0.0` makes repeat trials of the same scope
actually comparable to each other, verify with at least 2-3 repeat
from-scratch re-ingests before shipping, not a single run on each side
of the change as Part B's first attempt did.

## Explicitly not recommended

**Building a structured list/counter-valued entity-attribute mechanism**
(a schema change to `entities.attributes`, plus new append-not-overwrite
merge logic) — my own first pass at this plan assumed this was
necessary before checking whether `summary_key_facts` already solves
the accumulation problem. It does, mostly. Building a second, parallel
accumulating-list mechanism on top of entity attributes would be
duplicative of a structure (`summary_key_facts`) that already exists,
already has the right extraction instruction, and the data shows
already works better. The only real schema-level precedent for
"recurring facts as accumulating rows" in this codebase is
`entity_relationships` (`schema/0015_entity_relationships.sql:33+`),
which is a different domain (relationships between entities, not
scalar counts) — not reused here, just noted as the one place this
pattern already exists.

## Verification plan

- Unit test for the new query-shape detector (mirroring
  `mmr_test.go`/`keyfacts_test.go`'s pattern): positive/negative cases
  for "how many times has X...", "how many X does Y have", and
  near-miss phrasings that shouldn't trigger it.
- Real-infra check against the already-consolidated `conv-42` scope
  (`person:nate`): confirm the exhaustive fetch actually returns all 4+
  grounded tournament-win key_facts for "how many tournaments has Nate
  won," not just the ones the bounded top-K search would have
  surfaced — this is the same `HUPI_DEBUG_FUSION`/`hupi-export-memory`
  tracing discipline used throughout this investigation.
- A small, targeted LoCoMo re-run restricted to category-1 (multi-hop)
  counting-shaped questions first (cheap, fast-iteration, matching this
  session's established "small sample before full re-run" discipline),
  before a full 10-conversation re-run.
- Confirm no regression on non-counting multi-hop questions and on the
  other four categories — counting detection is a narrow, specific
  trigger, but the exhaustive-fetch path is new code that needs the
  same full-suite/full-benchmark discipline as every other change this
  session.
- Record the outcome (confirmed fix, any regressions found/fixed, final
  category-1 accuracy) as a dated entry in `docs/BENCHMARKS.md`, same
  tracking convention as every other investigation this session.
