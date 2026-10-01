package store

import (
	"fmt"
	"strings"
	"testing"
)

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
	got := summaryCitationSnippet("Melanie's hobbies and travels.", facts, queryTerms)
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
	got := guaranteedFact("Melanie's hobbies and travels.", facts, queryTerms)
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
	got := guaranteedFact("Melanie's hobbies and travels.", facts, []string{"melanie", "camping"})
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
	got := guaranteedFact("prose", facts, []string{"melanie", "watched"})
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
	got := guaranteedFact("prose", facts, []string{"what", "did", "i", "learn", "at", "the", "ai", "conference"})
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
	got := guaranteedFact("prose", atThreshold, nil)
	for _, f := range atThreshold {
		if !strings.Contains(got, f) {
			t.Errorf("guaranteedFact() at exactly the threshold = %q, want all facts included, missing %q", got, f)
		}
	}

	overThreshold := append(atThreshold, "one fact too many")
	got = guaranteedFact("prose", overThreshold, nil)
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
	got := depthText("the prose", facts, nil)
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
	writeKeyFacts(&sb, facts, queryTerms)
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
