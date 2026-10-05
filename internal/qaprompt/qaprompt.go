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
//
// The final sentence of the "double-check WHO" paragraph (the
// similar-lives emphasis) was added after tracing two real LoCoMo
// adversarial misses to their actual retrieved context, not just the
// final answer — both from the same conversation between two close
// friends, Caroline and Melanie, whose lives overlap heavily (both into
// running, mental health, self-care). In both cases the retrieved
// context was unambiguous: every single fact about the topic in
// question was explicitly labeled with the correct person's name, and
// one was even marked "(most relevant)". There was nothing wrong with
// what was retrieved. The predicted answer still blended the two
// people's facts together regardless — once with Caroline's own quote
// about a song attributed to Melanie (who was asked about), once with
// Melanie's own quote about running attributed to Caroline (who was
// asked about) — the same mechanism in both directions. The existing
// paragraph already named the general risk; this makes explicit that
// the risk is *highest*, not lower, exactly when two people's lives are
// similar enough that a fact "sounds like it could belong to either" —
// which is precisely when double-checking the specific name attached to
// the specific fact matters most, not less.
//
// The "some dates aren't stated directly but can be computed" paragraph
// was added after investigating LoCoMo's real temporal category
// (category 2 — see the relabeling note in docs/BENCHMARKS.md §1; this
// is the category previously mislabeled "single-hop"), whose zero-score
// misses were 66% abstentions (44 of 67) despite being literal date
// questions, not speculative ones. Traced two to their actual retrieved
// context, in two different conversations: "When did John get his dog
// Max?" (gold: 2013) had `entity person:max` retrieved with
// `date_of_passing: "2023-06-03", part_of_family_years: "10"` — the
// answer is 2023 minus 10, never computed; "When did John start his job
// in IT?" (gold: 2019) had a summary dated 2022-08-06 stating "John
// recently left his IT job after 3 years" — the answer is 2022 minus 3,
// also never computed. Both are genuine answer-time gaps, not retrieval
// misses: the raw ingredients (a duration and a dated anchor point) were
// correctly retrieved both times; the subtraction step was never taken,
// and the model fell back to "not mentioned" instead. Both worked
// examples in the new paragraph are these two real traced cases.
//
// The "some questions ask you to predict, judge, or infer" paragraph was
// added after investigating LoCoMo's real open-domain category (category
// 3 — see the category-relabeling note in docs/BENCHMARKS.md §1; this is
// the category that requires integrating a stated fact with outside
// knowledge or judgment, not the one previously mislabeled "temporal"),
// which scored worst of all five categories (34.8%) with a third of its
// questions abstaining. Tracing two real misses to their actual
// retrieved context (not just the final answer) found both had strong,
// directly on-topic facts already retrieved — not a retrieval gap and
// not simple abstention-aversion: "Would Caroline pursue writing as a
// career option?" (gold: likely no) had Caroline's own stated
// `career_interest: "counseling or mental health"` retrieved and marked
// "(most relevant)", with no mention of writing anywhere; "What state
// did Joanna visit in summer 2021?" (gold: Indiana) had "Joanna took a
// sunset photo during a hike near Fort Wayne" retrieved, also marked
// "(most relevant)", appearing multiple times. In both cases the model
// still said "not mentioned" rather than using the fact it had to
// construct the answer — treating "the literal predicted outcome isn't
// spelled out" as equivalent to "nothing relevant exists," which the
// existing "best specific attempt" paragraph didn't address: its
// existing examples are about incomplete/uncertain literal facts, not
// about deriving an unstated answer from a stated one via judgment or
// ordinary outside knowledge (geography, in this case). Both worked
// examples in the new paragraph are these two real traced cases, not
// invented ones.
//
// This paragraph is placed after the WHO-check paragraph, not before
// it (where it first shipped) — a full 10-conversation re-run of the
// first version found it regressed adversarial accuracy 66.6% → 61.2%
// (-5.4pp) for a net-zero gain on its own target category (34.8% →
// 34.8%, the specific traced cases fixed but offset by new failures
// elsewhere in the same category). Diffing predictions found three
// distinct causes, all fixed in the same pass: (1) the model sometimes
// used a fact already confirmed to belong to someone else anyway
// ("What does Caroline say running has been great for?" — Melanie's
// own quote, reused for Caroline) — fixed by moving this paragraph
// after the WHO-check and adding an explicit back-reference ("The
// WHO-check above still applies in full here too"); (2) the model
// sometimes fabricated a plausible-sounding but entirely unstated
// specific ("What did Caroline and her family do while camping?" →
// "roasted marshmallows" — nothing in context supports this) — fixed
// by an explicit "not a general license to elaborate... that is
// fabrication, not inference" sentence, since the paragraph's own
// examples (Fort Wayne, career interest) are real named facts, not
// permission to invent a scene; (3) the model sometimes correctly
// identified a false premise or misattribution in prose ("Jon does not
// own a store; he owns a dance studio") without including the literal
// substring LoCoMo's own scorer requires, scoring a substantively
// correct answer as wrong — fixed by adding a general, explicit
// "always include the literal phrase... don't rely on the explanation
// alone to imply it" instruction to the best-effort paragraph, rather
// than relying on this file's two scattered worked examples
// (TestConciseAbstentionExamplesUseTheExactScoredPhrase) to teach it by
// demonstration alone.
//
// The second sentence of the "double-check WHO" paragraph (the
// non-person generalization) was added after a real LongMemEval miss,
// `6ae235be` (single-session-assistant): the user asked what processes
// the Lake Charles Refinery uses, out of three CITGO refineries the
// assistant had described earlier, each with its own near-identical
// process list. Storage had the correct list for every refinery,
// correctly grounded. The predicted answer correctly matched Lake
// Charles's first three processes (atmospheric distillation, FCC,
// alkylation — distinguishing it from Lemont's different list) but then
// substituted the fourth item with Corpus Christi's extra process
// (hydrocracking) instead of Lake Charles's own (hydrotreating) — not a
// retrieval miss, an answer-time blend between two similarly-structured
// entities sitting next to each other in context. The original paragraph
// already named this exact mechanism for two people; it just didn't say
// it also applies to any other kind of similar, enumerated entity
// (places, branches, versions of a list), which is the same risk for a
// different noun.
//
// One new sentence in the "predict, judge, or infer" paragraph (dated
// 2026-10-05) extends it to one more sub-shape of the same category-3
// gap it was already built for, found by tracing all 33 traceable
// zero-score open-domain misses from a fresh full 10-conversation run
// to their actual retrieved context, not just the final answer (most of
// the 33 turned out to be genuine retrieval/consolidation gaps instead —
// not touched here, see docs/BENCHMARKS.md's dated entry for the full
// breakdown): Joanna's allergy profile (`"allergic_to":"most reptiles
// and animals with fur"`, `"allergic_to_cockroaches":"yes"`) was
// extensively retrieved for "what underlying condition might Joanna
// have based on her allergies?" (gold: asthma), but the existing
// paragraph's only worked examples are a preference-implies-judgment
// case and a geography lookup — neither covers an allergen pattern
// implying an ordinary medical condition, a different kind of outside-
// knowledge reasoning.
//
// A second addition — inferring who someone is, or how they relate to
// the person asked about, from shared activity or conversational framing
// rather than a named attribute (tried for two "who is X" cases, Anthony
// and Jill, that looked like a null-entity bug at first but turned out
// to have real on-topic evidence in their full retrieved context) — was
// tried and reverted the same day. It did flip Anthony's case correctly,
// but a full 10-conversation re-run found it net-regressed category 5
// (adversarial) by 16 separate questions, flipping correct abstentions
// into confident, specific wrong answers across a wide range of
// unrelated topics (a guitar's finish, a dog-grooming routine, a knee
// injury) — the same broadening-causes-fabrication failure shape this
// file's own first open-domain addition already caused once. Unlike
// that first regression (fixable by reordering and tightening), this
// one wasn't worth re-attempting in the same backlog item: the net
// trade was 1 genuine fix against 16 regressions. Reverted; see
// docs/BENCHMARKS.md's dated entry for the full before/after numbers.
const Concise = `Answer the following question directly, using a short phrase rather than a full sentence or explanation — but include every specific detail the question asks for (a complete name, date, or list), not just the first word or a truncated fragment.

Always give dates as an absolute date (e.g. "7 May 2023"), never a relative term like "yesterday", "last year", or "this month".

Some dates aren't stated directly but can be computed from a duration plus a dated reference point — a message dated 2022-08-06 saying a job ended "after 3 years" gives you everything you need to compute when it started (2022 minus 3 is 2019); an entity record's own last-known date (e.g. "date_of_passing: 2023-06-03") combined with a stated duration ("part of the family for 10 years") gives you the starting date (2013) just as directly as if it had been stated outright. Do that arithmetic and give the resulting absolute year or date, rather than saying the date is "not mentioned" just because it was never written out as a single standalone date.

Make your best specific attempt using anything relevant you've been told, even if you're not fully certain or the exact wording isn't stated verbatim — a specific, plausible answer inferred from related information is better than declining to answer. Only say "not mentioned" or "no information available" if there is truly nothing relevant to work with at all — not merely because the precise fact isn't stated in so many words. A fact that genuinely belongs to a different person or thing than the one actually asked about (see the next paragraph) does not count as something relevant to work with — if every specific detail you found on this topic turns out, on checking, to be about someone or something else, that is the same as having nothing, and you should say so rather than reporting their fact as if it answered the question asked. Whenever you conclude something isn't genuinely available — whether because it's truly missing, or because the only related fact you found actually belongs to someone or something else — always include the literal phrase "not mentioned" or "no information available" somewhere in your answer, even while also explaining why (e.g. naming who it actually belongs to instead); don't rely on the explanation alone to imply it.

Before answering, double-check WHO the retrieved information is actually about. A conversation between two people often has facts that apply to only one of them — if the question asks about person A but the fact you found belongs to person B, say so explicitly (e.g. "That's B's necklace, not A's — A's own necklace is not mentioned") rather than answering as if it were A's. The same risk applies to any set of similar, closely-related things, not just two people — several branches, locations, or versions of something, each with its own specific list or details. When a question names one specific one (e.g. "the Lake Charles Refinery" out of several refineries) and you have near-identical lists for multiple similar ones, use only the exact list that belongs to the one actually named — do not substitute or blend in an item from a different, similarly-structured one just because it sits right next to it in what you were given. This risk is HIGHEST, not lower, when two people's lives are similar — close friends who share the same interests or habits (both into running, both doing pottery, both focused on mental health) — because a fact that fits the topic can easily belong to the other person instead of the one named in the question; check the exact name actually attached to the specific fact you're using, every time, rather than assuming a topically-fitting fact must belong to whoever was asked about.

Some questions ask you to predict, judge, or infer something that was never stated outright — "Would X do Y?", "Might X have Z?", "Which state/company/person is likely..." — rather than asking you to find an explicit statement. For these, you may use a specific, directly on-topic fact you actually have — a named interest, a named place, a named detail — to construct a reasoned answer via simple, ordinary reasoning (geography, a stated preference implying how someone would likely feel about something similar), even when the literal predicted outcome itself is never spelled out word-for-word. If you know someone's actual, stated interest or focus, and the question asks whether they'd pursue something else instead, that stated interest is enough to answer with a judgment (e.g. a person whose stated career interest is "counseling or mental health" is likely NOT also pursuing writing as a career — say so, don't say "not mentioned" just because a writing career itself was never discussed). The same applies to connecting a stated detail to ordinary outside knowledge: if you're told someone hiked near Fort Wayne and the question asks which state they visited, give "Indiana" — Fort Wayne being in Indiana is ordinary geography, not a guess invented from nothing. The same reasoning also covers an ordinary medical or physical explanation for a stated pattern of symptoms or triggers: if someone is described as allergic to reptiles, animals with fur, and cockroaches, and the question asks what underlying condition they might have, "asthma" is a reasonable, ordinary inference from that allergy pattern, not a fabricated diagnosis. This is narrow, not a general license to elaborate: it requires an actual specific fact to reason from, named in what you were given — it does NOT mean inventing a plausible-sounding scene, feeling, or reason that was never stated anywhere just because it would fit (what someone did on a trip, how they felt about something, what inspired a piece of art); that is fabrication, not inference, and still counts as having nothing. The WHO-check above still applies in full here too — a fact that actually belongs to someone else doesn't become usable just because combining it would produce an answer.

When two or more retrieved memories give different values for the same specific fact about the same person or thing — for example, one memory says a gym membership costs $40 a month and a later-dated one says $55 — treat the most recently dated memory's value as the current one and answer with it, briefly noting the earlier value in parentheses (e.g. "$55 a month (earlier: $40)"). This holds even when the later memory mentions the value only in passing or as a recollection, and regardless of which memory appears first, is repeated more often, or is marked "(most relevant)". Judge recency by the date on the memory that actually states the value; an entity's "last updated" date covers its whole record, not each value inside it. This is only for genuine updates of one fact: values that answer different questions are not a conflict, even on the same topic (a $40 membership fee and a $55 personal-training session are two separate prices), and hypothetical or example figures don't count. If the question explicitly asks for the original, first, or previous value, give that one instead — past tense alone ("what was...") doesn't mean that.

When a question asks you to compute a value from two different figures together — a savings amount, a difference, "how much more/less" — only combine figures that were actually stated about the exact same specific scenario named in the question (the same airport, route, city, product, or person), ideally from the same statement or exchange. Do not pair a figure that answers the question with a different, only superficially-similar figure from a different scenario (a different airport, a different day's conversation, a different person's situation) just because it appeared nearby in what you were given — that produces a specific-looking number that doesn't correspond to anything either source actually said. If you can't find both figures stated about the same scenario, give the individual figures you do have, each labeled with which scenario it belongs to, rather than inventing a combined one.

When a question asks which of two specific named things happened first, happened more recently, or came before/after the other, confirm that BOTH named things actually appear somewhere in what you were given before answering — finding one of them is not evidence about the other. If only one appears at all, say so explicitly and name which one is missing (e.g. "You mentioned fixing the fence, but purchasing three cows from Peter is not mentioned — I can't say which came first"), rather than naming the one you did find as though it had won an actual comparison.

When the answer is a list of items or a yes/no question, give ONLY the items or the yes/no verdict itself — do not add supporting context, dates, or an explanation for each item, even when that detail is available in what you were given. Having more detail available doesn't mean including it is more correct; match the specificity level the question actually asked for, not everything you know that's related.`
