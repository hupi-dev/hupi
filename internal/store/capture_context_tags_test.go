package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

func TestCapture_PersistsContextTagsAndAuditsThem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-capture-tags"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	now := time.Now()
	ep := gateway.NewInteractionEpisode(gateway.NewEpisodeID(now), now, "mcp", "remember", "Prefers pnpm over npm.", "")
	ep.ContextTags = []string{"mcp", "remember"}
	if err := s.Capture(ctx, scope, ep); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plain := gateway.NewInteractionEpisode(gateway.NewEpisodeID(now.Add(time.Millisecond)), now, "openai", "gpt", "hi", "hello")
	if err := s.Capture(ctx, scope, plain); err != nil {
		t.Fatalf("Capture untagged: %v", err)
	}

	var tagsLit, plainTagsLit, detail string
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select context_tags from episodes where id = $1`, ep.ID).Scan(&tagsLit); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `select context_tags from episodes where id = $1`, plain.ID).Scan(&plainTagsLit); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			select detail::text from audit_log
			where event_type = 'capture' and target_ref->>'id' = $1
		`, ep.ID).Scan(&detail)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := pgfmt.ParseTextArray(tagsLit); len(got) != 2 || got[0] != "mcp" || got[1] != "remember" {
		t.Errorf("context_tags = %v, want [mcp remember]", got)
	}
	if got := pgfmt.ParseTextArray(plainTagsLit); len(got) != 0 {
		t.Errorf("untagged episode context_tags = %v, want empty", got)
	}
	if !strings.Contains(detail, `"context_tags": ["mcp", "remember"]`) {
		t.Errorf("capture audit detail %s missing context_tags", detail)
	}
}
