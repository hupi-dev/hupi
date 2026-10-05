package consolidation

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/identity"
	"hupi/internal/metrics"
)

// clusterIDs collects the ids from one cluster in source order, for
// assertions that don't care which cluster is "cluster 0" vs "cluster 1"
// (clusterSources' grouping order depends on map iteration order, which
// Go doesn't guarantee) but do care which sources ended up together.
func clusterIDs(cluster []textSource) map[string]bool {
	ids := make(map[string]bool, len(cluster))
	for _, s := range cluster {
		ids[s.id] = true
	}
	return ids
}

// findClusterContaining returns the cluster (as an id set) that contains
// id, or nil if no cluster does — a test helper, not production code.
func findClusterContaining(clusters [][]textSource, id string) map[string]bool {
	for _, c := range clusters {
		ids := clusterIDs(c)
		if ids[id] {
			return ids
		}
	}
	return nil
}

// TestClusterSourcesSeparatesDistinctTopics is the core real behavior
// docs/CONSOLIDATION_COMPLETENESS_PLAN.md's Phase B exists for: two
// genuinely unrelated topics (near-orthogonal embeddings) must land in
// separate clusters, each getting its own generateSummary call, rather
// than staying in one call where one topic can crowd the other out.
func TestClusterSourcesSeparatesDistinctTopics(t *testing.T) {
	sources := []textSource{
		{id: "a1", text: "topic A, message 1"},
		{id: "a2", text: "topic A, message 2"},
		{id: "b1", text: "topic B, message 1"},
	}
	vectors := [][]float32{
		{1, 0, 0},
		{0.95, 0.1, 0},
		{0, 1, 0},
	}

	clusters := clusterSources(sources, vectors)
	if len(clusters) != 2 {
		t.Fatalf("clusterSources() produced %d clusters, want 2 (a1+a2 together, b1 alone): %v", len(clusters), clusters)
	}

	aCluster := findClusterContaining(clusters, "a1")
	if aCluster == nil || !aCluster["a2"] {
		t.Errorf("a1 and a2 (near-identical topic A vectors) ended up in different clusters: %v", clusters)
	}
	if aCluster["b1"] {
		t.Errorf("b1 (topic B) ended up in the same cluster as topic A: %v", clusters)
	}
}

// TestClusterSourcesKeepsHighlySimilarSourcesTogether confirms the
// threshold doesn't over-fragment a day that's genuinely all one topic
// — clustering should be a no-op (one cluster) when nothing actually
// needs separating, matching today's behavior exactly for that case.
func TestClusterSourcesKeepsHighlySimilarSourcesTogether(t *testing.T) {
	sources := []textSource{
		{id: "s1", text: "same topic, message 1"},
		{id: "s2", text: "same topic, message 2"},
		{id: "s3", text: "same topic, message 3"},
	}
	vectors := [][]float32{
		{1, 0, 0},
		{0.99, 0.05, 0},
		{0.98, 0.02, 0.02},
	}

	clusters := clusterSources(sources, vectors)
	if len(clusters) != 1 {
		t.Fatalf("clusterSources() produced %d clusters, want 1 for near-identical vectors: %v", len(clusters), clusters)
	}
	if len(clusters[0]) != 3 {
		t.Errorf("clusterSources() single cluster has %d sources, want all 3", len(clusters[0]))
	}
}

// TestClusterSourcesRespectsMaxClustersPerDay confirms the real-cost
// bound: even when every source is genuinely distinct from every other
// (worst case for cluster count), the result never exceeds
// maxClustersPerDay, since each cluster is its own real LLM call.
func TestClusterSourcesRespectsMaxClustersPerDay(t *testing.T) {
	const n = 10 // more than maxClustersPerDay (6), all mutually orthogonal
	sources := make([]textSource, n)
	vectors := make([][]float32, n)
	for i := 0; i < n; i++ {
		sources[i] = textSource{id: string(rune('a' + i))}
		v := make([]float32, n)
		v[i] = 1
		vectors[i] = v
	}

	before := testutil.ToFloat64(metrics.ConsolidationClusterMergesTotal)
	clusters := clusterSources(sources, vectors)
	if len(clusters) > maxClustersPerDay() {
		t.Fatalf("clusterSources() produced %d clusters, want at most %d (maxClustersPerDay)", len(clusters), maxClustersPerDay())
	}

	total := 0
	for _, c := range clusters {
		total += len(c)
	}
	if total != n {
		t.Errorf("clusterSources() merged clusters lost sources: got %d total, want %d", total, n)
	}

	// docs/CONSOLIDATION_ARCHITECTURE_REVIEW_PLAN.md finding 4: 10
	// orthogonal sources into a cap of 6 forces exactly 4 merge events
	// (10 clusters -> 6), each one real dilution risk worth counting.
	wantMerges := n - maxClustersPerDay()
	if got := testutil.ToFloat64(metrics.ConsolidationClusterMergesTotal) - before; got != float64(wantMerges) {
		t.Errorf("ConsolidationClusterMergesTotal increased by %v, want %d", got, wantMerges)
	}
}

// TestClusterSourcesFallsBackOnMismatchedVectors is the defensive path:
// a caller bug (vector count not matching source count) must not panic
// or silently misassign vectors to the wrong source — it should fall
// back to one cluster, identical to not clustering at all.
func TestClusterSourcesFallsBackOnMismatchedVectors(t *testing.T) {
	sources := []textSource{{id: "a"}, {id: "b"}}
	vectors := [][]float32{{1, 0}}

	clusters := clusterSources(sources, vectors)
	if len(clusters) != 1 || len(clusters[0]) != 2 {
		t.Fatalf("clusterSources() with mismatched lengths = %v, want a single cluster containing both sources", clusters)
	}
}

func TestClusterSourcesEmptyInput(t *testing.T) {
	if got := clusterSources(nil, nil); got != nil {
		t.Errorf("clusterSources(nil, nil) = %v, want nil", got)
	}
}

// TestMergeConsolidationOutputsConcatenatesEverything is Phase B's other
// real requirement: combining clusters must not re-summarize (that
// would reintroduce the exact dilution risk clustering exists to
// avoid) — every key_fact, entity, and relationship from every cluster
// must survive into the merged output untouched.
func TestMergeConsolidationOutputsConcatenatesEverything(t *testing.T) {
	a := ConsolidationOutput{
		Summary:         "Topic A summary.",
		KeyFacts:        []KeyFactOutput{{Fact: "fact A", SourceEpisodeIDs: []string{"a1"}}},
		EntitiesTouched: []EntityUpdate{{ID: "person:a", Kind: "person", Name: "A"}},
		Relationships:   []RelationshipUpdate{{SubjectName: "A", Predicate: "knows", ObjectName: "B"}},
	}
	b := ConsolidationOutput{
		Summary:  "Topic B summary.",
		KeyFacts: []KeyFactOutput{{Fact: "fact B", SourceEpisodeIDs: []string{"b1"}}},
	}

	merged := mergeConsolidationOutputs([]ConsolidationOutput{a, b})

	if merged.Summary != "Topic A summary.\n\nTopic B summary." {
		t.Errorf("merged.Summary = %q, want both paragraphs joined with a blank line", merged.Summary)
	}
	if len(merged.KeyFacts) != 2 {
		t.Errorf("merged.KeyFacts has %d entries, want 2 (one per cluster)", len(merged.KeyFacts))
	}
	if len(merged.EntitiesTouched) != 1 {
		t.Errorf("merged.EntitiesTouched has %d entries, want 1", len(merged.EntitiesTouched))
	}
	if len(merged.Relationships) != 1 {
		t.Errorf("merged.Relationships has %d entries, want 1", len(merged.Relationships))
	}
}

// TestMergeConsolidationOutputsSkipsEmptySummaries confirms a cluster
// that produced no prose summary (edge case, but KeyFactOutput-only
// output is structurally valid) doesn't leave a stray blank paragraph
// in the merged text.
func TestMergeConsolidationOutputsSkipsEmptySummaries(t *testing.T) {
	a := ConsolidationOutput{Summary: "Real content."}
	b := ConsolidationOutput{Summary: "   "}

	merged := mergeConsolidationOutputs([]ConsolidationOutput{a, b})
	if merged.Summary != "Real content." {
		t.Errorf("merged.Summary = %q, want the blank/whitespace-only summary excluded", merged.Summary)
	}
}

// TestMaxClustersPerDayDefaultAndOverride is the real lever
// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 3 exists for —
// the default of 6 is a real, measured limitation on especially
// topic-diverse days (the Ibotta LongMemEval case), so this needs to be
// tunable without a rebuild, same pattern as
// internal/store/retrieve.go's contextCharBudget/maxVectorResults.
func TestMaxClustersPerDayDefaultAndOverride(t *testing.T) {
	if got := maxClustersPerDay(); got != defaultMaxClustersPerDay {
		t.Errorf("maxClustersPerDay() = %d, want the default %d when HUPI_MAX_CLUSTERS_PER_DAY is unset", got, defaultMaxClustersPerDay)
	}
	t.Setenv("HUPI_MAX_CLUSTERS_PER_DAY", "10")
	if got := maxClustersPerDay(); got != 10 {
		t.Errorf("maxClustersPerDay() = %d, want 10 from HUPI_MAX_CLUSTERS_PER_DAY", got)
	}
	t.Setenv("HUPI_MAX_CLUSTERS_PER_DAY", "not-a-number")
	if got := maxClustersPerDay(); got != defaultMaxClustersPerDay {
		t.Errorf("maxClustersPerDay() = %d, want the default %d for an invalid override", got, defaultMaxClustersPerDay)
	}
}

// TestGenerateDailySummary_LightDayAlsoRunsPerEpisodeInsurancePass is a
// real regression test for two independent live LongMemEval failures
// (89527b6b, 852ce960 — see generateDailySummary's own doc comment):
// a day at or below clusterEpisodeThreshold used to skip the per-episode
// insurance pass entirely, on the reasoning that only a busy, crowded
// day needed it. Both real failures were genuinely light days (4 and 1
// sessions respectively) where a single message buried a real, specific
// detail as a trailing aside after its own primary topic — the
// whole-day summary call extracted the primary topic and silently
// dropped the aside. This fixture reproduces that exact shape: one
// source whose primary topic (cable/TV providers) is what the summary
// call "notices," with a trailing Wells Fargo pre-approval mention the
// per-episode pass is specifically positioned to catch.
func TestGenerateDailySummary_LightDayAlsoRunsPerEpisodeInsurancePass(t *testing.T) {
	fake := &fakeSequentialProvider{responses: []string{
		// The whole-day summary call "notices" only the primary topic.
		`{"summary": "The user asked for cable and TV provider recommendations.", "key_facts": [{"fact": "The user asked for cable and TV provider recommendations."}], "entities_touched": []}`,
		// The per-episode insurance pass catches the trailing aside.
		`{"facts": ["The user was pre-approved for $400,000 from Wells Fargo."]}`,
	}}
	runner := New(nil, nil, fake, nil, nil)

	sources := []textSource{
		{id: "ep_wells_fargo", text: "I need to set up cable and TV services. Can you recommend some providers? By the way, remember when I got pre-approved for $400,000 from Wells Fargo?"},
	}
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-light-day-insurance"}

	out, err := runner.generateDailySummary(context.Background(), scope, "2023-11-30", sources, "", nil)
	if err != nil {
		t.Fatalf("generateDailySummary: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("got %d consolidation LLM calls, want 2 (1 summary + 1 per-episode) — the per-episode pass must run on this light (1-source) day, not just busy ones", fake.calls)
	}

	foundWellsFargo := false
	for _, f := range out.KeyFacts {
		if f.Fact == "The user was pre-approved for $400,000 from Wells Fargo." {
			foundWellsFargo = true
		}
	}
	if !foundWellsFargo {
		t.Errorf("generateDailySummary().KeyFacts = %+v, want it to include the per-episode pass's fact, not just the summary call's own fact", out.KeyFacts)
	}
}
