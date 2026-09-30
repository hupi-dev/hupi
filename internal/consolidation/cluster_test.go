package consolidation

import "testing"

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

	clusters := clusterSources(sources, vectors)
	if len(clusters) > maxClustersPerDay {
		t.Fatalf("clusterSources() produced %d clusters, want at most %d (maxClustersPerDay)", len(clusters), maxClustersPerDay)
	}

	total := 0
	for _, c := range clusters {
		total += len(c)
	}
	if total != n {
		t.Errorf("clusterSources() merged clusters lost sources: got %d total, want %d", total, n)
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
