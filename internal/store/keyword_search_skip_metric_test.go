package store

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// TestRetrieve_PunctuationOnlyQueryIncrementsKeywordSearchSkippedMetric
// is the real regression test for review finding B13: a near-empty or
// punctuation-only query (here, "???") passes stage 1 via
// stage1QuestionSignal's trailing-"?" rule alone, reaches vector search
// (paying a real embedding call), but tokenizes to zero real terms —
// silently skipping keyword search with nothing observable distinguishing
// that from "keyword search ran and found nothing." Confirms the new
// metric actually increments for this exact scenario.
func TestRetrieve_PunctuationOnlyQueryIncrementsKeywordSearchSkippedMetric(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-keyword-skip-metric"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	before := testutil.ToFloat64(metrics.KeywordSearchSkippedNoTermsTotal)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "???"}}
	if _, err := s.Retrieve(ctx, scope, scope, messages, time.Now()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	after := testutil.ToFloat64(metrics.KeywordSearchSkippedNoTermsTotal)
	if after != before+1 {
		t.Errorf("hupi_keyword_search_skipped_no_terms_total went from %v to %v, want exactly +1", before, after)
	}
}

// TestRetrieve_RealQueryDoesNotIncrementKeywordSearchSkippedMetric
// confirms the new metric is scoped to the real "zero tokens" cause,
// not firing on every ordinary turn.
func TestRetrieve_RealQueryDoesNotIncrementKeywordSearchSkippedMetric(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-keyword-skip-metric-control"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	before := testutil.ToFloat64(metrics.KeywordSearchSkippedNoTermsTotal)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "What is my favorite programming language?"}}
	if _, err := s.Retrieve(ctx, scope, scope, messages, time.Now()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	after := testutil.ToFloat64(metrics.KeywordSearchSkippedNoTermsTotal)
	if after != before {
		t.Errorf("hupi_keyword_search_skipped_no_terms_total went from %v to %v, want unchanged for a query with real search terms", before, after)
	}
}
