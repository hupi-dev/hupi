package consolidation

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"hupi/internal/identity"
	"hupi/internal/provider"
)

// clusterEpisodeThreshold is how many source episodes one calendar day
// needs before clustering kicks in at all — below this, RunDaily's
// sources go through generateSummary in one call exactly as before
// (zero behavior change for the common case: most days have a handful
// of sessions, not dozens). docs/CONSOLIDATION_COMPLETENESS_PLAN.md Gap
// 1 / Phase B: every real failing example this exists to fix
// (gpt4_45189cb4, gpt4_e072b769, b46e15ed) had 10-50+ sessions on the
// day the needed fact went missing.
const clusterEpisodeThreshold = 8

// clusterSimilarityThreshold is the cosine-similarity bar for grouping
// two sources into the same topic cluster. Deliberately generous (a
// high bar): clustering's whole purpose is keeping genuinely distinct
// topics in separate generateSummary calls so neither crowds the other
// out. Two loosely-related sources staying in separate clusters costs
// nothing — each still gets its own dedicated consideration — while
// merging two genuinely distinct topics back into one cluster
// reproduces the exact dilution problem this exists to fix. Not yet
// measured against real embedding distances — a reasoned starting
// point, flagged for calibration once verified to help at all (same
// posture as internal/store/retrieve.go's own threshold comments).
const clusterSimilarityThreshold = 0.60

// maxClustersPerDay bounds real LLM-call cost on a pathological day
// with many genuinely distinct topics: each cluster is its own
// generateSummary call, so an unbounded cluster count could turn one
// busy day into dozens of consolidation calls. When grouping produces
// more clusters than this, the two most similar clusters (by centroid)
// are merged together until at or under the cap, rather than an
// arbitrary cluster being dropped or forced smaller.
const maxClustersPerDay = 6

// generateDailySummary is RunDaily's entry point into generateSummary,
// adding Gap 1's real fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase
// B): a busy day gets clustered into topic groups first, each with its
// own generateSummary call, instead of one call covering every session
// regardless of how many genuinely distinct topics that day held — the
// confirmed mechanism behind a busy day silently dropping minor-but-real
// facts (a prompt-only attempt at the same fix was tried and reverted;
// see that document's Status section for why a soft instruction wasn't
// enough to change this behavior on its own).
//
// Below clusterEpisodeThreshold, or if embedding/clustering fails or
// degenerates to one cluster, this falls back to exactly today's
// single-call behavior rather than erroring the whole day's
// consolidation over an optimization.
func (r *Runner) generateDailySummary(ctx context.Context, scope identity.Scope, period string, sources []textSource, establishedRecord string) (ConsolidationOutput, error) {
	if len(sources) <= clusterEpisodeThreshold {
		return r.generateSummary(ctx, scope, "daily", period, sources, establishedRecord)
	}

	texts := make([]string, len(sources))
	for i, s := range sources {
		texts[i] = s.text
	}
	embedResp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: texts})
	if err != nil {
		slog.Warn("consolidation: embed sources for clustering failed, falling back to single-pass summary", "period", period, "error", err)
		return r.generateSummary(ctx, scope, "daily", period, sources, establishedRecord)
	}
	if len(embedResp.Vectors) != len(sources) {
		slog.Warn("consolidation: embedder returned mismatched vector count, falling back to single-pass summary", "period", period, "want", len(sources), "got", len(embedResp.Vectors))
		return r.generateSummary(ctx, scope, "daily", period, sources, establishedRecord)
	}

	clusters := clusterSources(sources, embedResp.Vectors)
	if len(clusters) <= 1 {
		return r.generateSummary(ctx, scope, "daily", period, sources, establishedRecord)
	}

	slog.Info("consolidation: clustering busy day into topic groups", "period", period, "sources", len(sources), "clusters", len(clusters))

	outputs := make([]ConsolidationOutput, 0, len(clusters))
	for _, cluster := range clusters {
		out, err := r.generateSummary(ctx, scope, "daily", period, cluster, establishedRecord)
		if err != nil {
			return ConsolidationOutput{}, fmt.Errorf("generate cluster summary for %s: %w", period, err)
		}
		outputs = append(outputs, out)
	}
	return mergeConsolidationOutputs(outputs), nil
}

// mergeConsolidationOutputs combines several clusters' independently-
// generated outputs into the one ConsolidationOutput storeSummary's
// single-row-per-period shape expects. Mechanical concatenation, not a
// further LLM summarization pass — re-compressing the clusters' own
// outputs together would just reintroduce the exact dilution risk
// clustering exists to avoid. Duplicate entities_touched across clusters
// (e.g. the same person mentioned in two different clusters) are safe
// to pass through unmodified: upsertEntities already processes its
// updates slice one at a time within a single transaction, so the same
// entity ID appearing twice merges correctly in order, the same as any
// other repeated EntityUpdate.
func mergeConsolidationOutputs(outputs []ConsolidationOutput) ConsolidationOutput {
	var merged ConsolidationOutput
	summaries := make([]string, 0, len(outputs))
	for _, out := range outputs {
		if strings.TrimSpace(out.Summary) != "" {
			summaries = append(summaries, out.Summary)
		}
		merged.KeyFacts = append(merged.KeyFacts, out.KeyFacts...)
		merged.EntitiesTouched = append(merged.EntitiesTouched, out.EntitiesTouched...)
		merged.Relationships = append(merged.Relationships, out.Relationships...)
	}
	merged.Summary = strings.Join(summaries, "\n\n")
	return merged
}

// clusterSources groups sources into topic clusters using each source's
// embedding vector (same order as sources) — see generateDailySummary's
// doc comment for why. Pure function, no I/O, fully unit-testable
// without a real LLM or database.
//
// Union-find on pairwise cosine similarity, not a "proper" clustering
// algorithm (k-means, DBSCAN): the input size here is small (tens of
// sources, not thousands) and the goal is specifically "don't let two
// unrelated topics share a summarization call," not an optimal
// partition — greedy transitive grouping is enough, and simple enough
// to reason about and test exhaustively.
func clusterSources(sources []textSource, vectors [][]float32) [][]textSource {
	n := len(sources)
	if n == 0 {
		return nil
	}
	if len(vectors) != n {
		// Caller error, not a data problem: fall back to one cluster
		// (today's exact behavior) rather than silently mis-cluster
		// against a mismatched index.
		return [][]textSource{sources}
	}

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cosineSimilarity(vectors[i], vectors[j]) >= clusterSimilarityThreshold {
				union(i, j)
			}
		}
	}

	rootOrder := make([]int, 0, n)
	groups := make(map[int][]int) // root -> source indices, insertion order preserved via rootOrder
	for i := 0; i < n; i++ {
		root := find(i)
		if _, ok := groups[root]; !ok {
			rootOrder = append(rootOrder, root)
		}
		groups[root] = append(groups[root], i)
	}

	clusters := make([][]textSource, 0, len(rootOrder))
	centroids := make([][]float32, 0, len(rootOrder))
	for _, root := range rootOrder {
		idxs := groups[root]
		cluster := make([]textSource, len(idxs))
		for k, idx := range idxs {
			cluster[k] = sources[idx]
		}
		clusters = append(clusters, cluster)
		centroids = append(centroids, centroidOf(vectors, idxs))
	}

	for len(clusters) > maxClustersPerDay {
		bi, bj, best := 0, 1, -2.0
		for i := 0; i < len(centroids); i++ {
			for j := i + 1; j < len(centroids); j++ {
				sim := cosineSimilarity(centroids[i], centroids[j])
				if sim > best {
					best = sim
					bi, bj = i, j
				}
			}
		}
		clusters[bi] = append(clusters[bi], clusters[bj]...)
		centroids[bi] = centroidOfVectors([][]float32{centroids[bi], centroids[bj]})
		clusters = append(clusters[:bj], clusters[bj+1:]...)
		centroids = append(centroids[:bj], centroids[bj+1:]...)
	}

	return clusters
}

func centroidOf(vectors [][]float32, idxs []int) []float32 {
	vs := make([][]float32, len(idxs))
	for i, idx := range idxs {
		vs[i] = vectors[idx]
	}
	return centroidOfVectors(vs)
}

func centroidOfVectors(vs [][]float32) []float32 {
	if len(vs) == 0 {
		return nil
	}
	dim := len(vs[0])
	sum := make([]float32, dim)
	for _, v := range vs {
		for i, x := range v {
			if i < dim {
				sum[i] += x
			}
		}
	}
	for i := range sum {
		sum[i] /= float32(len(vs))
	}
	return sum
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
