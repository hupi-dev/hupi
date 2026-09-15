// Package selfcheck implements the scheduled memory-probe self-check from
// ARCHITECTURE.md § Retrieval observability: a small, fixed set of
// questions you write once, re-run periodically, that catch retrieval
// regressing — a probe whose retrieved context stops containing the
// expected fact is flagged. This tests the retrieval pipeline, not the
// underlying LLM, so it runs directly against a gateway.Retriever with no
// provider call of its own beyond whatever Retrieve does internally
// (embedding the query).
package selfcheck

import (
	"context"
	"fmt"
	"strings"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// Probe is a single "does memory still know this" check, meant to be
// hand-written once against things you know are durably true (per
// ARCHITECTURE.md's examples: "what's my current project's working
// directory", "what did I decide about the vector index last month") —
// not generated, since the whole point is an independent check on
// retrieval, not another thing retrieval itself could get wrong.
type Probe struct {
	ID              string `yaml:"id"`
	Query           string `yaml:"query"`
	ExpectSubstring string `yaml:"expect_substring"` // case-insensitive; empty skips this check
	ExpectMinGate   string `yaml:"expect_min_gate"`  // "partial" or "full"; empty skips this check
	// Scope is which scope to probe — a team scope once Tier 3 teams
	// exist. Zero value defaults to identity.DefaultScope, so existing
	// probes.yaml files with no scope field keep working unchanged.
	Scope identity.Scope `yaml:"scope"`
}

type Result struct {
	ProbeID string
	Passed  bool
	Gate    gateway.MemoryGate
	Detail  string
}

var gateRank = map[gateway.MemoryGate]int{
	gateway.GateSkipped: 0,
	gateway.GatePartial: 1,
	gateway.GateFull:    2,
}

// Run executes every probe against retriever and returns one Result each.
// It stops and returns an error only on an infrastructure failure (the
// retriever itself erroring) — a probe simply not finding what it expects
// is a failed Result, not a Go error, since that's the exact condition
// this exists to detect and report on, not to crash over.
func Run(ctx context.Context, retriever gateway.Retriever, probes []Probe) ([]Result, error) {
	results := make([]Result, 0, len(probes))
	for _, p := range probes {
		res, err := runOne(ctx, retriever, p)
		if err != nil {
			return nil, fmt.Errorf("selfcheck: probe %s: %w", p.ID, err)
		}
		results = append(results, res)
	}
	return results, nil
}

func runOne(ctx context.Context, retriever gateway.Retriever, p Probe) (Result, error) {
	scope := p.Scope
	if scope == (identity.Scope{}) {
		scope = identity.DefaultScope
	}

	// Probes have no notion of "acting user" distinct from "workspace" —
	// they're a low-level check on retrieval, not tied to a specific
	// person — so the same scope serves as both (see gateway.Retriever's
	// doc comment on why the two are normally separate).
	result, err := retriever.Retrieve(ctx, scope, scope, []provider.Message{{Role: provider.RoleUser, Content: p.Query}})
	if err != nil {
		return Result{}, err
	}

	var reasons []string

	if p.ExpectMinGate != "" {
		want, ok := gateRank[gateway.MemoryGate(p.ExpectMinGate)]
		if !ok {
			return Result{}, fmt.Errorf("probe %s: invalid expect_min_gate %q", p.ID, p.ExpectMinGate)
		}
		if gateRank[result.Gate] < want {
			reasons = append(reasons, fmt.Sprintf("gate was %q, wanted at least %q", result.Gate, p.ExpectMinGate))
		}
	}

	if p.ExpectSubstring != "" && !strings.Contains(strings.ToLower(result.ContextMessage), strings.ToLower(p.ExpectSubstring)) {
		reasons = append(reasons, fmt.Sprintf("expected retrieved context to contain %q, it didn't", p.ExpectSubstring))
	}

	if len(reasons) == 0 {
		return Result{ProbeID: p.ID, Passed: true, Gate: result.Gate, Detail: "ok"}, nil
	}
	return Result{ProbeID: p.ID, Passed: false, Gate: result.Gate, Detail: strings.Join(reasons, "; ")}, nil
}
