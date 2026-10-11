package store

import (
	"context"
	"time"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// SearchOptions configures Search.
type SearchOptions struct {
	CharBudget int // 0 = HUPI_CONTEXT_CHAR_BUDGET
	ReadAudit
}

// SearchResult is a RetrievalResult plus where the fixed anchor ends:
// Citations[:AnchorCitations] are self_model / latest-summary context, not
// search hits.
type SearchResult struct {
	gateway.RetrievalResult
	AnchorCitations int
}

// Search is Retrieve for an explicit lookup (an MCP tool, a search box):
// the query is already a deliberate request, so stage 1's "nothing worth
// searching" shortcut is bypassed — a plain keyword query like "postgres
// migration plan" would otherwise be skipped as small talk.
func (s *Store) Search(ctx context.Context, actingUser, workspace identity.Scope, query string, now time.Time, opts SearchOptions) (SearchResult, error) {
	messages := []provider.Message{{Role: provider.RoleUser, Content: query}}
	result, anchors, err := s.retrieve(ctx, actingUser, workspace, messages, now, retrieveOptions{forceSearch: true, charBudget: opts.CharBudget})
	if err != nil {
		return SearchResult{}, err
	}
	s.auditRead(ctx, actingUser, workspace, opts.ReadAudit.detail(map[string]any{
		"gate":      string(result.Gate),
		"ref_count": len(result.Refs),
		"forced":    true,
	}))
	return SearchResult{RetrievalResult: result, AnchorCitations: anchors}, nil
}
