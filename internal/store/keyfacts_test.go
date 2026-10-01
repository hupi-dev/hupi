package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// lexicalFacts builds []keyFact fixtures with no similarity (Valid ==
// false) for every fact — rankKeyFacts' documented fallback condition —
// so these tests exercise exactly the pre-embedding lexical path
// unchanged, regardless of any embedding-based behavior added since.
func lexicalFacts(texts ...string) []keyFact {
	facts := make([]keyFact, len(texts))
	for i, t := range texts {
		facts[i] = keyFact{text: t}
	}
	return facts
}

// semanticFacts builds []keyFact fixtures with real similarity scores —
// one float per text, same length and order — for tests exercising
// rankKeyFacts' semantic path.
func semanticFacts(texts []string, similarities []float64) []keyFact {
	if len(texts) != len(similarities) {
		panic("semanticFacts: texts and similarities must be the same length")
	}
	facts := make([]keyFact, len(texts))
	for i, t := range texts {
		facts[i] = keyFact{text: t, similarity: sql.NullFloat64{Float64: similarities[i], Valid: true}}
	}
	return facts
}

// TestMostRelevantFactIndexPicksClearWinner is the real hypothesis step 6
// of docs/BENCHMARK_IMPROVEMENT_PLAN.md exists to test: given several key
// facts under one summary, the one sharing the most query vocabulary
// should be identified so appendKeyFacts can promote it, instead of a
// question's answer sometimes being buried a few bullets down in
// insertion order for the model to find on its own.
func TestMostRelevantFactIndexPicksClearWinner(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
		"Melanie's daughter recently started kindergarten.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != 1 {
		t.Errorf("mostRelevantFactIndex() = %d, want 1 (the camping fact)", got)
	}
}

func TestMostRelevantFactIndexReturnsNegativeOneWhenNothingStandsOut(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie's daughter recently started kindergarten.",
	}
	queryTerms := []string{"what", "is", "the", "weather", "today"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no fact shares any real query vocabulary)", got)
	}
}

func TestMostRelevantFactIndexReturnsNegativeOneOnTie(t *testing.T) {
	facts := []string{
		"Melanie went camping at the beach.",
		"Melanie went camping in the mountains.",
	}
	queryTerms := []string{"melanie", "camping"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (both facts tie on shared vocabulary — no real winner to promote)", got)
	}
}

func TestMostRelevantFactIndexNoOpBelowTwoFacts(t *testing.T) {
	if got := mostRelevantFactIndex([]string{"only one fact"}, []string{"fact"}); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (nothing to reorder among a single fact)", got)
	}
	if got := mostRelevantFactIndex(nil, []string{"fact"}); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no facts at all)", got)
	}
}

func TestMostRelevantFactIndexNoOpWithoutQueryTerms(t *testing.T) {
	facts := []string{"Melanie went camping.", "Melanie enjoys painting."}
	if got := mostRelevantFactIndex(facts, nil); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no query terms to rank against)", got)
	}
}

// TestSummaryCitationSnippetIncludesMostRelevantFact confirms the
// citation snippet (docs/ANSWER_CITATIONS_PLAN.md) reuses the same
// mostRelevantFactIndex ranking appendKeyFacts already applies to the
// injected context — a citation should never disagree with what was
// actually promoted for the model to see.
func TestSummaryCitationSnippetIncludesMostRelevantFact(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := summaryCitationSnippet("Melanie's hobbies and travels.", lexicalFacts(facts...), queryTerms)
	want := "Melanie's hobbies and travels.\n  - (most relevant) Melanie has camped at the beach, in the mountains, and in the forest."
	if got != want {
		t.Errorf("summaryCitationSnippet() = %q, want %q", got, want)
	}
}

// TestSummaryCitationSnippetIsProseOnlyWithoutAClearWinner confirms no
// fabricated "(most relevant)" label when nothing actually stands out —
// mirrors appendKeyFacts' own no-reordering behavior in that case.
func TestSummaryCitationSnippetIsProseOnlyWithoutAClearWinner(t *testing.T) {
	got := summaryCitationSnippet("Melanie's hobbies and travels.", nil, []string{"melanie"})
	want := "Melanie's hobbies and travels."
	if got != want {
		t.Errorf("summaryCitationSnippet() = %q, want %q", got, want)
	}
}

// TestGuaranteedFactPrefersMostRelevant is the real behavior
// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 1 exists for:
// whatever guaranteedFact returns is protected from summaryDepthCap's
// truncation, so it must be the single most useful thing to keep, not
// just the first fact in insertion order. Uses more facts than
// guaranteedFactMaxCount specifically so this exercises the "pick one"
// ranking path, not the "guarantee everything" small-set path below.
func TestGuaranteedFactPrefersMostRelevant(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
		"Melanie likes hiking on weekends.",
		"Melanie collects vintage postcards.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := guaranteedFact("Melanie's hobbies and travels.", lexicalFacts(facts...), queryTerms)
	want := "Melanie has camped at the beach, in the mountains, and in the forest."
	if got != want {
		t.Errorf("guaranteedFact() = %q, want the most relevant fact %q", got, want)
	}
}

// TestGuaranteedFactFallsBackToFirstFactWithoutAClearWinner mirrors
// mostRelevantFactIndex's own no-clear-winner behavior — still guarantee
// *a* fact over nothing, just not one fabricated as "most relevant."
// Uses more facts than guaranteedFactMaxCount for the same reason as
// TestGuaranteedFactPrefersMostRelevant above.
func TestGuaranteedFactFallsBackToFirstFactWithoutAClearWinner(t *testing.T) {
	facts := []string{
		"Melanie went camping at the beach.",
		"Melanie went camping in the mountains.",
		"Melanie went camping in the desert.",
		"Melanie went camping by the lake.",
	}
	got := guaranteedFact("Melanie's hobbies and travels.", lexicalFacts(facts...), []string{"melanie", "camping"})
	if got != facts[0] {
		t.Errorf("guaranteedFact() = %q, want the first fact %q when nothing stands out", got, facts[0])
	}
}

// TestGuaranteedFactTiedAmongRelevantFactsStillPicksARelevantOne is a
// real regression test (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a real
// 41-episode/112-fact busy day had several facts tie at the same
// nonzero score (each sharing exactly one query term, everything else
// scoring 0) — guaranteedFact used to call mostRelevantFactIndex
// directly, whose winner-take-all tie-break treats "several facts tied
// at the top" identically to "nothing relevant at all" (returns -1),
// falling back to facts[0] regardless of its own score. On the real
// busy day this was found against, facts[0] was an entirely unrelated
// tire-pressure fact, guaranteed into context ahead of the NFL-playoffs
// fact a "what order did I watch sports events" question actually
// needed — which was left stranded in the lower-priority depth section,
// where a tight context budget cut it away entirely. Unlike
// TestGuaranteedFactFallsBackToFirstFactWithoutAClearWinner (a genuine
// tie where every candidate is equally relevant), this fixture's tied
// facts are relevant while facts[0] itself scores 0 — rankFactsByRelevance
// must not discard that distinction.
func TestGuaranteedFactTiedAmongRelevantFactsStillPicksARelevantOne(t *testing.T) {
	facts := []string{
		"Measuring tire pressure is advised after hot laps.",
		"Melanie watched a documentary about volcanoes.",
		"Melanie watched the NFL playoffs over the weekend.",
		"Melanie watched a cooking show on Tuesday.",
	}
	got := guaranteedFact("prose", lexicalFacts(facts...), []string{"melanie", "watched"})
	if got == facts[0] {
		t.Errorf("guaranteedFact() = %q, want a fact that actually matches the query, not the unrelated facts[0]", got)
	}
	if !strings.Contains(got, "Melanie watched") {
		t.Errorf("guaranteedFact() = %q, want one of the tied-but-relevant facts", got)
	}
}

// TestGuaranteedFactGuaranteesAllWhenFewEnough is the real, measured fix
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md, Phase E adversarial case):
// picking just one fact from a small set is a real lottery when neither
// shares query vocabulary — a real consolidation run split "attended a
// robotics event" and "actuators and control systems were featured"
// into two separate facts, and guaranteeing only the first (content-free)
// one measurably broke the answer at temperature 0 (9/10 wrong vs. 10/10
// correct once both were guaranteed). Below guaranteedFactMaxCount, every
// fact is guaranteed together instead of picking one.
func TestGuaranteedFactGuaranteesAllWhenFewEnough(t *testing.T) {
	facts := []string{
		"On 2024-05-15, the user attended a downtown robotics event.",
		"At the event, actuators and control systems were featured.",
	}
	got := guaranteedFact("prose", lexicalFacts(facts...), []string{"what", "did", "i", "learn", "at", "the", "ai", "conference"})
	for _, f := range facts {
		if !strings.Contains(got, f) {
			t.Errorf("guaranteedFact() = %q, want it to include both facts, missing %q", got, f)
		}
	}
}

// TestGuaranteedFactPicksOneAtExactlyMaxCountPlusOne pins down the exact
// boundary: guaranteedFactMaxCount facts guarantee all of them,
// guaranteedFactMaxCount+1 falls back to picking one.
func TestGuaranteedFactPicksOneAtExactlyMaxCountPlusOne(t *testing.T) {
	atThreshold := make([]string, guaranteedFactMaxCount)
	for i := range atThreshold {
		atThreshold[i] = fmt.Sprintf("fact %d", i)
	}
	got := guaranteedFact("prose", lexicalFacts(atThreshold...), nil)
	for _, f := range atThreshold {
		if !strings.Contains(got, f) {
			t.Errorf("guaranteedFact() at exactly the threshold = %q, want all facts included, missing %q", got, f)
		}
	}

	overThreshold := append(atThreshold, "one fact too many")
	got = guaranteedFact("prose", lexicalFacts(overThreshold...), nil)
	if got != overThreshold[0] {
		t.Errorf("guaranteedFact() one over the threshold = %q, want just the first fact %q", got, overThreshold[0])
	}
}

// TestGuaranteedFactFallsBackToProseWithNoFactsAtAll confirms a summary
// with nothing grounded still guarantees *something* (a short prose
// snippet) rather than an empty guarantee.
func TestGuaranteedFactFallsBackToProseWithNoFactsAtAll(t *testing.T) {
	prose := "Melanie's hobbies and travels."
	got := guaranteedFact(prose, nil, []string{"melanie"})
	if got != prose {
		t.Errorf("guaranteedFact() = %q, want the prose itself %q (short enough not to need truncating)", got, prose)
	}
}

// TestGuaranteedFactTruncatesLongProseFallback confirms the no-facts
// fallback is itself bounded — a guarantee that's allowed to be
// unbounded would defeat summaryDepthCap's whole purpose.
func TestGuaranteedFactTruncatesLongProseFallback(t *testing.T) {
	longProse := strings.Repeat("x", guaranteedProseFallbackChars+500)
	got := guaranteedFact(longProse, nil, nil)
	if len(got) <= guaranteedProseFallbackChars || !strings.Contains(got, "truncated") {
		t.Errorf("guaranteedFact() with no facts and long prose wasn't truncated, len=%d", len(got))
	}
}

// TestDepthTextIncludesProseAndAllFacts confirms the bounded "extra
// depth" section still carries the full picture (prose + every fact,
// most-relevant marked) — summaryDepthCap bounds its *length*, not its
// content; depthText itself should still build the complete picture for
// the caller to truncate.
func TestDepthTextIncludesProseAndAllFacts(t *testing.T) {
	facts := []string{"fact one", "fact two"}
	got := depthText("the prose", lexicalFacts(facts...), nil)
	if !strings.Contains(got, "the prose") || !strings.Contains(got, "fact one") || !strings.Contains(got, "fact two") {
		t.Errorf("depthText() = %q, want it to contain the prose and every fact", got)
	}
}

// TestRankFactsByRelevanceOrdersByScoreDescending is the core new
// behavior (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): unlike
// mostRelevantFactIndex (pick one winner or bail), every fact gets
// ordered, not just the top one.
func TestRankFactsByRelevanceOrdersByScoreDescending(t *testing.T) {
	facts := []string{
		"Her daughter recently started kindergarten.",                         // 0 shared terms
		"Melanie enjoys painting landscapes in her free time.",                // 1 shared term (melanie)
		"Melanie has camped at the beach, in the mountains, and in the forest.", // 2 shared terms (melanie, camped)
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := rankFactsByRelevance(facts, queryTerms)
	want := []int{2, 1, 0}
	if len(got) != len(want) {
		t.Fatalf("rankFactsByRelevance() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rankFactsByRelevance() = %v, want %v", got, want)
			break
		}
	}
}

// TestRankFactsByRelevanceStableOnTies confirms tied facts keep their
// original relative order rather than one winning arbitrarily — this is
// what distinguishes rankFactsByRelevance from mostRelevantFactIndex's
// own tie behavior (bail to -1, discard the signal entirely).
func TestRankFactsByRelevanceStableOnTies(t *testing.T) {
	facts := []string{"Melanie went camping at the beach.", "Melanie went camping in the mountains."}
	queryTerms := []string{"melanie", "camping"}
	got := rankFactsByRelevance(facts, queryTerms)
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("rankFactsByRelevance() = %v, want [0 1] (tied facts keep original order)", got)
	}
}

func TestRankFactsByRelevanceNoQueryTermsReturnsOriginalOrder(t *testing.T) {
	facts := []string{"fact a", "fact b", "fact c"}
	got := rankFactsByRelevance(facts, nil)
	for i, idx := range got {
		if idx != i {
			t.Errorf("rankFactsByRelevance(no query terms) = %v, want [0 1 2] (original order preserved)", got)
			break
		}
	}
}

// TestWriteKeyFactsOrdersEntireListNotJustTheWinner is the real,
// measured fix this investigation found necessary
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a real 112-fact busy day
// summary (41 episodes clustered together) tied out on
// mostRelevantFactIndex's own winner-take-all scoring, and the one fact
// that actually answered the question was left buried in raw
// cluster-merge order among the other ~110, indistinguishable from
// completely unrelated facts. This test reconstructs that shape at a
// manageable scale: several relevant facts plus many irrelevant ones,
// none of them a clean single "winner" (so mostRelevantFactIndex bails),
// confirming every relevant fact still lands ahead of every irrelevant
// one once writeKeyFacts is in play.
func TestWriteKeyFactsOrdersEntireListNotJustTheWinner(t *testing.T) {
	facts := []string{
		"The user collects vintage postcards.",
		"The user enjoys sports on television.",
		"The user has been working on a thesis for six months.",
		"The user attended several events last month.",
		"The user is interested in embroidery.",
		"The user traveled to Chicago in January for a conference.",
		"The user started a spreadsheet log.",
	}
	queryTerms := []string{"what", "is", "the", "order", "of", "sports", "events", "i", "watched", "in", "january"}

	// Confirm the real failure mode actually reproduces here first: three
	// separate facts each share exactly one query term (sports/events/
	// january), tying for the top score, so mostRelevantFactIndex bails
	// rather than picking a single winner — matching the real bug's
	// shape (no clean winner once multiple facts are each genuinely
	// relevant).
	if best := mostRelevantFactIndex(facts, queryTerms); best != -1 {
		t.Fatalf("test setup: mostRelevantFactIndex() = %d, want -1 (three facts should tie, matching the real bug's shape)", best)
	}

	var sb strings.Builder
	writeKeyFacts(&sb, lexicalFacts(facts...), queryTerms)
	got := sb.String()

	relevant := []string{"enjoys sports", "several events", "Chicago in January"}
	irrelevant := []string{"postcards", "thesis", "embroidery", "spreadsheet log"}
	lastRelevant := -1
	for _, f := range relevant {
		if idx := strings.Index(got, f); idx > lastRelevant {
			lastRelevant = idx
		} else if idx < 0 {
			t.Fatalf("writeKeyFacts() missing expected fact containing %q", f)
		}
	}
	for _, f := range irrelevant {
		if idx := strings.Index(got, f); idx >= 0 && idx < lastRelevant {
			t.Errorf("writeKeyFacts() placed irrelevant fact %q (at %d) ahead of the last relevant sports fact (at %d) — relevant facts should cluster toward the front even without a single clean winner", f, idx, lastRelevant)
		}
	}
}

// TestGuaranteeBudgetPerSummary_FewPicksStaysGenerous confirms the
// common case (1-3 picked summaries) is effectively unaffected by the
// fair-share cap — most real facts are well under it.
func TestGuaranteeBudgetPerSummary_FewPicksStaysGenerous(t *testing.T) {
	t.Setenv("HUPI_CONTEXT_CHAR_BUDGET", "2000")
	for _, n := range []int{1, 2, 3} {
		got := guaranteeBudgetPerSummary(n)
		if got < 300 {
			t.Errorf("guaranteeBudgetPerSummary(%d) = %d, want a generous share (>=300) when few summaries are picked", n, got)
		}
	}
}

// TestGuaranteeBudgetPerSummary_ManyPicksStillRespectsFloor is a real
// regression test (docs/CONSOLIDATION_COMPLETENESS_PLAN.md /
// docs/MEMORY_SCENARIOS.md scenario D): a real production case
// (gpt4_e072b769, "how many weeks ago did I start using Ibotta") had 9
// summaries picked for one generic question, and the correct summary —
// lowest fused score of the 9 — never got its guarantee line written at
// all under the default 2000-char budget, because nothing capped how
// much the other 8 picks' guarantee lines could cost first. Confirms
// the per-summary share shrinks as more summaries compete, but never
// below guaranteeMinPerSummary — enough room for a real short fact
// ("The user has just downloaded Ibotta, a cashback app." is 55 chars)
// regardless of how many summaries are picked.
func TestGuaranteeBudgetPerSummary_ManyPicksStillRespectsFloor(t *testing.T) {
	t.Setenv("HUPI_CONTEXT_CHAR_BUDGET", "2000")
	got := guaranteeBudgetPerSummary(9)
	if got != guaranteeMinPerSummary {
		t.Errorf("guaranteeBudgetPerSummary(9) = %d, want the floor %d — 9 picks against a 2000-char budget should hit the minimum, not divide down to near-zero", got, guaranteeMinPerSummary)
	}
	if got < 55 {
		t.Errorf("guaranteeBudgetPerSummary(9) = %d, want enough room for a real short fact (55 chars)", got)
	}
}

// TestGuaranteeBudgetPerSummary_ZeroPicksReturnsZero is a defensive
// boundary check — fusedSearchSummaries never calls this with an empty
// picks list in practice (it returns early), but the function shouldn't
// divide by zero if it ever did.
func TestGuaranteeBudgetPerSummary_ZeroPicksReturnsZero(t *testing.T) {
	if got := guaranteeBudgetPerSummary(0); got != 0 {
		t.Errorf("guaranteeBudgetPerSummary(0) = %d, want 0", got)
	}
}

// TestCenteredExcerptShortTextReturnedUnchanged confirms the common
// case — text already under the cap — is a no-op, same as hardTruncate.
func TestCenteredExcerptShortTextReturnedUnchanged(t *testing.T) {
	text := "short text"
	if got := centeredExcerpt(text, []string{"short"}, 100); got != text {
		t.Errorf("centeredExcerpt() = %q, want unchanged %q", got, text)
	}
}

// TestCenteredExcerptFallsBackToHeadTruncateWithoutAMatch confirms the
// no-query-term-found case degrades to the old hardTruncate behavior
// rather than returning nothing.
func TestCenteredExcerptFallsBackToHeadTruncateWithoutAMatch(t *testing.T) {
	text := strings.Repeat("x", 500)
	got := centeredExcerpt(text, []string{"nomatch"}, 50)
	want := hardTruncate(text, 50)
	if got != want {
		t.Errorf("centeredExcerpt() = %q, want the same as hardTruncate() = %q", got, want)
	}
}

// TestRankKeyFacts_SemanticSimilarityBreaksTheRealLexicalTie_gpt4_45189cb4
// is a real regression test for the live LongMemEval failure this schema/
// internal/store change exists to fix: a 123-grounded-fact busy-day
// summary where the correct fact ("watched the Chiefs defeat the
// Bills... NFL playoffs") and an unrelated climate-change fact both
// scored exactly one shared lexical term ("watched" vs. "events"
// respectively) — a tie that the old lexical-only rankFactsByRelevance
// broke in favor of whichever fact was extracted first (the climate one,
// at an earlier index), for "what is the order of the sports events I
// watched in January." Similarities below are the real values measured
// against the live OpenAI text-embedding-3-small API for this exact
// question against this scope's real facts (the NFL fact ranked #1 of
// 125). The fixture includes 10 facts, not 4 — a toy-sized fixture
// initially passed with a design (semantic similarity overriding lexical
// outright) that real end-to-end re-verification then showed regressed a
// *different* real fact in this same scope (see
// TestRankKeyFacts_RRFDoesNotLetOneNoisyEmbeddingOverrideAClearLexicalSignal_gpt4_45189cb4
// below) — a small, flat tie between exactly two candidates doesn't
// exercise RRF's actual rank-fusion behavior the way a realistically
// proportioned candidate pool does.
func TestRankKeyFacts_SemanticSimilarityBreaksTheRealLexicalTie_gpt4_45189cb4(t *testing.T) {
	// text, lexical-relevant?, real-measured cosine similarity to the
	// query "what is the order of the sports events I watched in January"
	type fixtureFact struct {
		text        string
		similarity  float64
		lexicalHint string // which query term, if any, this fact shares — for readability only
	}
	fixture := []fixtureFact{
		{"Rising temperatures and increased frequency of extreme weather events are affecting crop yields.", 0.08, "events"},
		{"The user recently participated in the 3-day 'Turbocharged' autocross event at the fairgrounds.", 0.15, ""},
		{"The user is planning to join a recreational volleyball league that starts in a few weeks.", 0.10, ""},
		{"The user has been practicing their tennis serve on their own at least once a week.", 0.12, ""},
		{"The user was considering planning a fantasy football draft with friends.", 0.11, ""},
		{"The user hopes to finish reading a novel by the end of the month.", 0.05, ""},
		{"The user needs to follow up on some leads from the Tech Expo last month.", 0.09, ""},
		{"The user is planning a sports-themed scavenger hunt around the Staples Center.", 0.19, "sports"},
		{"The user watched the Kansas City Chiefs defeat the Buffalo Bills in the Divisional Round of the NFL playoffs.", 0.267, "watched"},
		{"The user discussed a painting class with a friend.", 0.07, ""},
	}
	queryTerms := []string{"order", "sports", "events", "watched", "january"}

	texts := make([]string, len(fixture))
	sims := make([]float64, len(fixture))
	for i, f := range fixture {
		texts[i] = f.text
		sims[i] = f.similarity
	}
	const nflIdx, climateIdx, scavengerIdx = 8, 0, 7

	// Confirm the real failure mode reproduces first: facts 0, 7, and 8
	// all share exactly one query term each, tying for the top lexical
	// score, and the stable sort keeps the earliest (wrong) one first.
	lexOrder, lexBest := rankKeyFacts(lexicalFacts(texts...), queryTerms)
	if lexBest != -1 {
		t.Fatalf("test setup: lexical rankKeyFacts bestIdx = %d, want -1 (a genuine 3-way tie, matching the real bug's shape)", lexBest)
	}
	if lexOrder[0] != climateIdx {
		t.Fatalf("test setup: lexical rankKeyFacts order[0] = %d, want %d (the wrong, earliest-indexed fact winning the tie — this must reproduce the real bug before the fix is meaningful)", lexOrder[0], climateIdx)
	}

	semOrder, _ := rankKeyFacts(semanticFacts(texts, sims), queryTerms)
	if semOrder[0] != nflIdx {
		t.Errorf("RRF-fused rankKeyFacts order[0] = %d, want %d (the NFL fact — its dominant semantic rank (#1 of 10) should outweigh the climate fact's and the scavenger-hunt fact's merely-tied lexical rank)", semOrder[0], nflIdx)
	}

	// The real property that matters: the fact actually reaches the
	// depth block within summaryDepthCap, not just "ranks first in
	// isolation."
	depth := hardTruncate(depthText("prose", semanticFacts(texts, sims), queryTerms), summaryDepthCap)
	if !strings.Contains(depth, "Kansas City Chiefs") {
		t.Errorf("depthText() truncated to summaryDepthCap = %q, want it to contain the NFL fact", depth)
	}
}

// TestRankKeyFacts_RRFDoesNotLetOneNoisyEmbeddingOverrideAClearLexicalSignal_gpt4_45189cb4
// is a real regression test caught by live end-to-end re-verification of
// the fix above, on the *same* real question against a *different* picked
// summary in the same scope (5 facts — nowhere near busy-day scale). Here
// factScores unambiguously ranks the College Football National
// Championship fact first: it's the only one of the five sharing "watched"
// with the query, no tie at all. A first version of rankKeyFacts (semantic
// similarity overriding lexical outright, falling back to lexical only on
// a near-exact-tie) got this case wrong: real measurement against the
// live OpenAI text-embedding-3-small API showed the embedding model itself
// scores an unrelated fact (a TV-show-watching plan) *higher* than the
// correct one (0.293 vs. 0.218) — a genuine embedding-model false
// positive, not a bug in this ranking code. That version let the single
// noisy similarity comparison override a clean, unambiguous lexical
// signal, truncating the correct fact out of the real assembled context.
// RRF fusion (reciprocalRank(lexRank) + reciprocalRank(semRank)) is the
// fix: a fact with a clean top lexical rank keeps real weight even when
// one embedding comparison disagrees, rather than being unilaterally
// overruled by it.
func TestRankKeyFacts_RRFDoesNotLetOneNoisyEmbeddingOverrideAClearLexicalSignal_gpt4_45189cb4(t *testing.T) {
	texts := []string{
		"On 2023-01-13, Georgia defeated Alabama 33-18 in the College Football National Championship game.",
		"The user and their dad watched the College Football National Championship game at home on 2023-01-13.", // the correct fact — the only one sharing "watched"
		"On 2023-01-14, the user had not played The Witcher video games or read the books before starting the TV show.",
		"On 2023-01-14, the user planned to check out The Witcher and The Mandalorian TV shows after finishing another series.", // the real embedding false positive — no shared query vocabulary at all
		"On 2023-01-14, the user was considering planning a fantasy football draft with friends.",
	}
	// Real cosine similarities measured against the live OpenAI API for
	// "what is the order of the sports events I watched in January".
	sims := []float64{0.1986, 0.2177, 0.1858, 0.2926, 0.2530}
	queryTerms := []string{"order", "sports", "events", "watched", "january"}
	const championshipIdx, falsePositiveIdx = 1, 3

	// Confirm the lexical signal really is clean and unambiguous first —
	// no tie, nothing for RRF to need to rescue on the lexical side.
	lexOrder, lexBest := rankKeyFacts(lexicalFacts(texts...), queryTerms)
	if lexBest != championshipIdx || lexOrder[0] != championshipIdx {
		t.Fatalf("test setup: lexical rankKeyFacts = (order[0]=%d, best=%d), want a clean, unambiguous winner at %d", lexOrder[0], lexBest, championshipIdx)
	}

	// Confirm the embedding model itself really does score the false
	// positive higher — this is what makes the regression real, not a
	// fixture artifact.
	if sims[falsePositiveIdx] <= sims[championshipIdx] {
		t.Fatalf("test setup: similarity[%d]=%.4f should exceed similarity[%d]=%.4f for this to be a real false positive", falsePositiveIdx, sims[falsePositiveIdx], championshipIdx, sims[championshipIdx])
	}

	order, _ := rankKeyFacts(semanticFacts(texts, sims), queryTerms)
	if order[0] != championshipIdx {
		t.Errorf("RRF-fused rankKeyFacts order[0] = %d, want %d (the championship fact) — its clean top lexical rank should outweigh one noisy embedding comparison", order[0], championshipIdx)
	}

	depth := hardTruncate(depthText("prose", semanticFacts(texts, sims), queryTerms), summaryDepthCap)
	if !strings.Contains(depth, "College Football National Championship") {
		t.Errorf("depthText() truncated to summaryDepthCap = %q, want it to contain the championship fact", depth)
	}
}

// TestRankKeyFacts_MixedEmbeddingStateFallsBackToLexical confirms a
// summary with some embedded and some unembedded facts (a partial
// reembed, or a fact written just before an embedding-provider switch)
// doesn't mix two incomparable scales — it falls back to the existing
// lexical ranking entirely, rather than ranking embedded facts by
// similarity and unembedded ones arbitrarily.
func TestRankKeyFacts_MixedEmbeddingStateFallsBackToLexical(t *testing.T) {
	facts := []keyFact{
		{text: "Melanie enjoys painting landscapes in her free time."},
		{text: "Melanie has camped at the beach, in the mountains, and in the forest.", similarity: sql.NullFloat64{Float64: 0.9, Valid: true}},
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	order, best := rankKeyFacts(facts, queryTerms)
	wantOrder, wantBest := rankFactsByRelevance(keyFactTexts(facts), queryTerms), mostRelevantFactIndex(keyFactTexts(facts), queryTerms)
	if order[0] != wantOrder[0] || best != wantBest {
		t.Errorf("rankKeyFacts() with a mixed embedding state = (order=%v, best=%d), want the pure lexical result (order=%v, best=%d)", order, best, wantOrder, wantBest)
	}
}

// TestRankKeyFacts_KillSwitchForcesLexical confirms
// HUPI_ENABLE_SEMANTIC_FACT_RANKING=false reproduces the pre-embedding
// behavior exactly, even when every fact has a real similarity score —
// the A/B and emergency-disable lever this switch exists for.
func TestRankKeyFacts_KillSwitchForcesLexical(t *testing.T) {
	t.Setenv("HUPI_ENABLE_SEMANTIC_FACT_RANKING", "false")
	texts := []string{"climate events fact", "NFL playoffs watched fact"}
	queryTerms := []string{"events", "watched"}
	order, _ := rankKeyFacts(semanticFacts(texts, []float64{0.9, 0.1}), queryTerms)
	wantOrder := rankFactsByRelevance(texts, queryTerms)
	if order[0] != wantOrder[0] {
		t.Errorf("rankKeyFacts() with the kill switch off = order %v, want the pure lexical order %v even though similarities favor a different fact", order, wantOrder)
	}
}

// TestCenteredExcerptPrefersDenserClusterOverFirstOccurrence is a real
// regression test (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): the real
// `60bf93ed` case had one query term ("backpack") appear early in a
// passing, less relevant remark, while the passage that actually
// answered the question sat near a denser cluster of several query
// terms together, later in the text. An earlier version of this
// function centered on the *first* occurrence of any term and missed
// the detail entirely — real-verified that scoring by cluster density
// instead fixes it.
func TestCenteredExcerptPrefersDenserClusterOverFirstOccurrence(t *testing.T) {
	text := "the backpack is nice. " +
		strings.Repeat("filler words with no query terms at all. ", 10) +
		"the laptop backpack was bought in january for the trip."
	queryTerms := []string{"laptop", "backpack", "bought"}
	got := centeredExcerpt(text, queryTerms, 60)
	if !strings.Contains(got, "bought") {
		t.Errorf("centeredExcerpt() = %q, want it centered on the denser cluster containing %q", got, "bought")
	}
}
