package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// TestFusedSearchSummaries_ExcludesOutOfTimeframeCandidateEntirely is the
// answer-time reasoning follow-up to Phase E's own temporal-relevance
// boost (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): real-verified against
// the adversarial "AI conference" case that a ranking boost alone wasn't
// enough — even after it correctly raised the right candidate's fused
// score, and even after mmrSelect's ordering fix correctly put it first
// in context, the answering model still picked the temporally-wrong
// summary because it sat right there in context with a closer lexical
// match. This test mirrors that exact scenario and confirms the
// temporally-wrong one is now excluded from context entirely, not merely
// deprioritized.
func TestFusedSearchSummaries_ExcludesOutOfTimeframeCandidateEntirely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-timeframe-filter"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummary(t, s, scope, "sum_test-timeframe-filter_2024-01-15_daily_v1", "2024-01-15",
		"The user attended an AI conference on 2024-01-15. Neural networks and deep learning were covered in depth.", "")
	insertSummary(t, s, scope, "sum_test-timeframe-filter_2024-05-15_daily_v1", "2024-05-15",
		"The user attended a robotics-focused event downtown on 2024-05-15. Actuators and control systems were highlighted.", "")

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "Neural networks") {
		t.Errorf("context includes the January (out-of-timeframe) summary's content — it should have been excluded entirely, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Actuators and control systems") {
		t.Errorf("context missing the May (in-timeframe, correct) summary's content, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "dated 2024-05-15") || !strings.Contains(result.ContextMessage, "3 weeks before now") {
		t.Errorf("context missing the computed relative-date label for the May summary, got: %q", result.ContextMessage)
	}
}

// TestFusedSearchSummaries_TimeframeFilterBacksOffWhenNothingSurvives
// confirms the safe-degrade path: if a confidently-resolved timeframe
// would exclude every candidate (e.g. consolidation never ran for the
// implied period), the filter must not return an empty context — it
// backs off to the unfiltered, boost-only behavior instead. Same
// direction as groundingCheck's own count-mismatch handling elsewhere in
// this codebase: never destroy information on an unclear signal.
func TestFusedSearchSummaries_TimeframeFilterBacksOffWhenNothingSurvives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-timeframe-filter-backoff"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// Neither summary's period overlaps "last month" relative to `now`
	// below (June) — January and May are both out of scope.
	insertSummary(t, s, scope, "sum_test-timeframe-filter-backoff_2024-01-15_daily_v1", "2024-01-15",
		"The user attended an AI conference on 2024-01-15. Neural networks and deep learning were covered in depth.", "")
	insertSummary(t, s, scope, "sum_test-timeframe-filter-backoff_2024-05-15_daily_v1", "2024-05-15",
		"The user attended a robotics-focused event downtown on 2024-05-15. Actuators and control systems were highlighted.", "")

	now := time.Date(2024, 7, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to June — neither summary overlaps
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q — the filter backing off should still surface something (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Neural networks") && !strings.Contains(result.ContextMessage, "Actuators and control systems") {
		t.Errorf("context has neither summary's content — the timeframe filter should have backed off rather than excluding everything, got: %q", result.ContextMessage)
	}
}
