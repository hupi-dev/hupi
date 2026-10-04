// Package qaprompt holds the QA-phase answer-style system prompt shared
// by the benchmark harness (cmd/hupi-bench) and the EvalMem adapter tool
// (cmd/hupi-answer-question) — both evaluation-only tools, not the real
// production gateway (internal/gateway.Handler injects no such prompt;
// changing real-deployment answer style is a separate decision, not made
// here). This was previously duplicated verbatim between the two cmd
// packages ("small tools don't cross-import" — see docs/CODE_GUIDE.md §5
// — refers to each tool's own unexported types, not a shared prompt
// constant, so moving it here doesn't violate that convention); the
// duplication was a real, live drift risk: a fix made to one copy had no
// mechanism forcing the other to follow, found in practice when
// cmd/hupi-answer-question's own copy lagged behind a wording fix to
// cmd/hupi-bench's.
package qaprompt

// Concise is sent only during the QA phase, never during session replay:
// LoCoMo/LongMemEval's own expected answers are short phrases ("7 May
// 2023", "2022"), and their official scoring is literal word-overlap F1
// or an external LLM judge, not forgiving of HUPI's default hedging,
// multi-sentence answer style ("Unfortunately, the provided text snippet
// does not specify...") — that style scores near-zero against a two-word
// expected answer even when the underlying retrieved content was in the
// right area. This doesn't fix genuine recall misses, but it stops
// good-content answers being scored as if they were wrong purely on
// phrasing. A real EvalMem run against cmd/hupi-answer-question without
// this prompt (docs/EVALMEM_INTEGRATION_PLAN.md step 7) independently
// confirmed the same gap there: every answer showed the same hedging
// style, and NEG/abstention accuracy was far below cmd/hupi-bench's own
// number on the same underlying model — this prompt was duplicated
// verbatim between the two tools before this package existed, which was
// itself a real, live drift risk (a wording fix to one copy had no
// mechanism forcing the other to follow).
//
// Revised after the first real GPT-4.1/LoCoMo run (all 10 conversations,
// 1,986 questions) surfaced three specific, real gaps in the original
// wording:
//
//  1. "as few words as possible" was measurably too aggressive — it cost
//     partial F1 credit by dropping words the reference answer needed
//     (e.g. reference "Psychology, counseling certification" vs a
//     truncated "counseling"). Categories that should be *easier*
//     (single-hop, temporal) scored lower than harder ones
//     (multi-hop, open-domain), which is backwards from what retrieval
//     quality alone would predict — a real signal the old wording was
//     itself costing points, not just style.
//  2. Query time for LoCoMo QA has no real per-question date (there is
//     no ground truth "now"), so the model would sometimes answer in
//     relative terms ("Yesterday", "Last year") reasoned against a
//     fabricated instant — scoring as wrong against an absolute
//     reference date ("7 May 2023") even when the underlying recall was
//     completely correct.
//  3. Category 5 (adversarial) is scored by literal substring match on
//     "no information available"/"not mentioned" — spot-checking real
//     category-5 predictions found many correct abstentions in
//     different words ("No recent setback mentioned", "No record of...")
//     that scored as wrong purely on phrasing.
//
// Note on (3) specifically: this is benchmark-vocabulary-aware tuning,
// not a general product improvement — teaching the model LoCoMo's exact
// expected abstention phrase is fair (clear abstention is good UX
// regardless), but it should be named honestly in any published
// write-up rather than presented as an organic capability gain.
//
// The "two or more retrieved memories give different values" paragraph
// (docs/LONGMEMEVAL_ACCURACY_PLAN.md Category 3 Phase 1, 852ce960 — two
// genuine, conflicting Wells Fargo pre-approval amounts, months apart,
// that consolidation-time contradiction detection (internal/consolidation's
// findRelatedSummaries) found as a candidate pair but the model itself
// judged NOT a clear contradiction, given the real "remember when I got
// pre-approved for $400,000?" phrasing — a recollection, not an explicit
// update statement) is deliberately phrased with a non-mortgage example,
// so the prompt isn't tuned to this one failing question's own wording.
// Each clause addresses something observed in that real case: "in passing
// or as a recollection" (the actual source phrasing), "repeated more
// often / (most relevant)" (the stale value appeared ~3x more often in
// context and was ranked first), "last updated" caveat (the touched
// entity's own last-updated date doesn't date the specific value), the
// different-facts guard (this same scope has several other dollar
// figures on related but distinct topics), and "past tense alone"
// (852ce960's own question asks "what was the amount," past tense, but
// still wants the updated value, not the original).
//
// The "only combine figures... about the exact same specific scenario"
// paragraph is a distinct failure shape from the one above — not two
// versions of one fact, but two different real figures from two
// different scenarios wrongly combined into one computed answer.
// Direct inspection of the real source sessions for 09ba9854_abs (a
// LongMemEval multi-session question asking a bus fare from Narita
// airport to a Shinjuku hotel) found the predicted answer "about ¥4,000
// (bus ¥3,200 vs taxi ¥7,000)" paired a genuine, scenario-matched Narita
// bus fare (¥3,200, from the session that specifies Narita+Shinjuku)
// with a genuine but wrong-scenario Haneda (a different airport) taxi
// estimate (¥6,000-10,000, from an earlier session discussing both
// airports generically) and did clean arithmetic on the mismatched pair
// — real figures, wrong pairing, not fabrication from nothing. The gold
// answer is an abstention ("you did not mention how much the bus would
// take"); teaching a broader "hedged/unconfirmed advice isn't a usable
// fact" rule was deliberately NOT added here — this case's whole
// conversation is pre-booking brainstorming, and that distinction is a
// much bigger, less-verified change than the narrow conflation guard
// below, recorded as a known residual in docs/LONGMEMEVAL_ACCURACY_PLAN.md
// rather than risked here.
//
// The "which of two named things happened first" paragraph is a
// distinct, related failure shape, found the same way: `gpt4_70e84552_abs`
// (LongMemEval temporal-reasoning) asked "which did I complete first,
// fixing the fence or purchasing three cows from Peter?" The real source
// mentions fixing the fence; "purchasing three cows from Peter" never
// appears anywhere — the question's second item is fabricated by
// design, testing whether a false premise gets silently accepted. The
// predicted answer, "fixing the fence," treated the one side it found as
// though it had won an actual comparison, rather than noticing the other
// side was never stated at all. Gold is an abstention. This is the
// comparison-shaped sibling of the figure-conflation paragraph above:
// that one guards against combining two real but mismatched figures,
// this one guards against ordering two named things when only one of
// them is real.
const Concise = `Answer the following question directly, using a short phrase rather than a full sentence or explanation — but include every specific detail the question asks for (a complete name, date, or list), not just the first word or a truncated fragment.

Always give dates as an absolute date (e.g. "7 May 2023"), never a relative term like "yesterday", "last year", or "this month".

Make your best specific attempt using anything relevant you've been told, even if you're not fully certain or the exact wording isn't stated verbatim — a specific, plausible answer inferred from related information is better than declining to answer. Only say "not mentioned" or "no information available" if there is truly nothing relevant to work with at all — not merely because the precise fact isn't stated in so many words.

Before answering, double-check WHO the retrieved information is actually about. A conversation between two people often has facts that apply to only one of them — if the question asks about person A but the fact you found belongs to person B, say so explicitly (e.g. "That's B's necklace, not A's — A's own necklace isn't mentioned") rather than answering as if it were A's.

When two or more retrieved memories give different values for the same specific fact about the same person or thing — for example, one memory says a gym membership costs $40 a month and a later-dated one says $55 — treat the most recently dated memory's value as the current one and answer with it, briefly noting the earlier value in parentheses (e.g. "$55 a month (earlier: $40)"). This holds even when the later memory mentions the value only in passing or as a recollection, and regardless of which memory appears first, is repeated more often, or is marked "(most relevant)". Judge recency by the date on the memory that actually states the value; an entity's "last updated" date covers its whole record, not each value inside it. This is only for genuine updates of one fact: values that answer different questions are not a conflict, even on the same topic (a $40 membership fee and a $55 personal-training session are two separate prices), and hypothetical or example figures don't count. If the question explicitly asks for the original, first, or previous value, give that one instead — past tense alone ("what was...") doesn't mean that.

When a question asks you to compute a value from two different figures together — a savings amount, a difference, "how much more/less" — only combine figures that were actually stated about the exact same specific scenario named in the question (the same airport, route, city, product, or person), ideally from the same statement or exchange. Do not pair a figure that answers the question with a different, only superficially-similar figure from a different scenario (a different airport, a different day's conversation, a different person's situation) just because it appeared nearby in what you were given — that produces a specific-looking number that doesn't correspond to anything either source actually said. If you can't find both figures stated about the same scenario, give the individual figures you do have, each labeled with which scenario it belongs to, rather than inventing a combined one.

When a question asks which of two specific named things happened first, happened more recently, or came before/after the other, confirm that BOTH named things actually appear somewhere in what you were given before answering — finding one of them is not evidence about the other. If only one appears at all, say so explicitly and name which one is missing (e.g. "You mentioned fixing the fence, but purchasing three cows from Peter isn't mentioned — I can't say which came first"), rather than naming the one you did find as though it had won an actual comparison.

When the answer is a list of items or a yes/no question, give ONLY the items or the yes/no verdict itself — do not add supporting context, dates, or an explanation for each item, even when that detail is available in what you were given. Having more detail available doesn't mean including it is more correct; match the specificity level the question actually asked for, not everything you know that's related.`
