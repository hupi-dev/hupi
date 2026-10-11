package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

func insertAttributeMemory(t *testing.T, s *Store, scope identity.Scope, id, entityID, key, value string) {
	t.Helper()
	ctx := context.Background()
	enc, keyVersion, err := s.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	ct, err := enc.Encrypt(value)
	if err != nil {
		t.Fatalf("encrypt test attribute: %v", err)
	}
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
			values ($1, $2, $3, $4, $5, $6, $7, true, true)
		`, id, scope.Kind, scope.Owner, entityID, key, ct, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("insert attribute memory %s: %v", id, err)
	}
}

func TestLookupEntity_ResolvesByIDNameAndSubstring(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-lookup-entity"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:dana", "person", "Dana Reyes")
	insertEntity(t, s, scope, "project:hupi", "project", "HUPI")
	insertEntity(t, s, scope, "project:hupi-code", "project", "HUPI Code")
	insertEntity(t, s, scope, "self_model:primary", "self_model", "Dana's profile")
	insertAttributeMemory(t, s, scope, "mem_test_lookup_role", "person:dana", "role", "staff engineer")
	insertRelationship(t, s, scope, "rel_test_lookup_1", "person:dana", "works_on", "project:hupi")

	byID, err := s.LookupEntity(ctx, scope, scope, "person:dana", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity by id: %v", err)
	}
	if byID.Entity == nil || byID.Entity.ID != "person:dana" {
		t.Fatalf("by id: entity = %+v, want person:dana", byID.Entity)
	}
	if len(byID.Attributes) != 1 || byID.Attributes[0].Key != "role" || byID.Attributes[0].Value != "staff engineer" {
		t.Errorf("attributes = %+v, want role=staff engineer", byID.Attributes)
	}
	if len(byID.Relationships) != 1 || byID.Relationships[0].ObjectName != "HUPI" || byID.Relationships[0].ValidUntil != nil {
		t.Errorf("relationships = %+v, want one current works_on HUPI", byID.Relationships)
	}

	// The incoming side of the same edge.
	hupi, err := s.LookupEntity(ctx, scope, scope, "hupi", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity by exact name: %v", err)
	}
	if hupi.Entity == nil || hupi.Entity.ID != "project:hupi" {
		t.Fatalf("exact name 'hupi' should resolve project:hupi before substring matching, got %+v / candidates %+v", hupi.Entity, hupi.Candidates)
	}
	if len(hupi.Relationships) != 1 || hupi.Relationships[0].SubjectID != "person:dana" {
		t.Errorf("incoming relationships = %+v, want dana works_on hupi", hupi.Relationships)
	}

	sub, err := s.LookupEntity(ctx, scope, scope, "hup", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity by substring: %v", err)
	}
	if sub.Entity != nil || len(sub.Candidates) != 2 {
		t.Errorf("ambiguous substring: entity = %+v, candidates = %+v, want 2 candidates", sub.Entity, sub.Candidates)
	}

	self, err := s.LookupEntity(ctx, scope, scope, "self_model:primary", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity self_model: %v", err)
	}
	if self.Entity != nil || len(self.Candidates) != 0 {
		t.Errorf("self_model must never be returned, got %+v / %+v", self.Entity, self.Candidates)
	}

	wildcard, err := s.LookupEntity(ctx, scope, scope, "%", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity wildcard: %v", err)
	}
	if wildcard.Entity != nil || len(wildcard.Candidates) != 0 {
		t.Errorf("a literal %% must not match everything, got %+v / %+v", wildcard.Entity, wildcard.Candidates)
	}
}

func TestLookupEntity_DoesNotSeeAnotherScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-lookup-iso-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-lookup-iso-b"}
	t.Cleanup(func() { cleanupScope(t, s, scopeA); cleanupScope(t, s, scopeB) })

	insertEntity(t, s, scopeA, "person:secret", "person", "Secret Person")

	got, err := s.LookupEntity(ctx, scopeB, scopeB, "person:secret", ReadAudit{})
	if err != nil {
		t.Fatalf("LookupEntity: %v", err)
	}
	if got.Entity != nil || len(got.Candidates) != 0 {
		t.Errorf("scope B saw scope A's entity: %+v / %+v", got.Entity, got.Candidates)
	}
}

func TestListEntities_FiltersAndReportsOverflow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-list-entities"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:dana", "person", "Dana")
	insertEntity(t, s, scope, "person:eli", "person", "Eli")
	insertEntity(t, s, scope, "project:hupi", "project", "HUPI")
	insertEntity(t, s, scope, "self_model:primary", "self_model", "profile")

	all, err := s.ListEntities(ctx, scope, scope, EntityFilter{Limit: 10}, ReadAudit{})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("got %d entities, want 3 (self_model excluded): %+v", len(all), all)
	}

	people, err := s.ListEntities(ctx, scope, scope, EntityFilter{Kind: "person", Limit: 10}, ReadAudit{})
	if err != nil {
		t.Fatalf("ListEntities kind: %v", err)
	}
	if len(people) != 2 {
		t.Errorf("kind=person: got %d, want 2", len(people))
	}

	named, err := s.ListEntities(ctx, scope, scope, EntityFilter{NameContains: "an", Limit: 10}, ReadAudit{})
	if err != nil {
		t.Fatalf("ListEntities name: %v", err)
	}
	if len(named) != 1 || named[0].ID != "person:dana" {
		t.Errorf("name contains 'an': got %+v, want only Dana", named)
	}

	limited, err := s.ListEntities(ctx, scope, scope, EntityFilter{Limit: 2}, ReadAudit{})
	if err != nil {
		t.Fatalf("ListEntities limit: %v", err)
	}
	if len(limited) != 3 {
		t.Errorf("limit 2 should return limit+1 = 3 rows so callers can detect overflow, got %d", len(limited))
	}
}

func TestRecentSummaries_ReturnsCurrentGroundedUnexpiredContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-recent-summaries"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	insertSummary(t, s, scope, "sum_test-recent_2026-10-08_daily_v1", "2026-10-08", "Old version of the 8th.", "")
	insertSummary(t, s, scope, "sum_test-recent_2026-10-08_daily_v2", "2026-10-08", "Corrected 8th: chose pgvector.", "sum_test-recent_2026-10-08_daily_v1")
	insertSummary(t, s, scope, "sum_test-recent_2026-10-10_daily_v1", "2026-10-10", "Planned the MCP server.", "")
	insertSummary(t, s, scope, "sum_test-recent_2026-09-01_daily_v1", "2026-09-01", "Too old for the window.", "")
	insertKeyFact(t, s, scope, "sum_test-recent_2026-10-10_daily_v1", "Decided to ship MCP over HTTP.", true)
	insertKeyFact(t, s, scope, "sum_test-recent_2026-10-10_daily_v1", "An ungrounded claim.", false)
	insertKeyFactWithExpiration(t, s, scope, "sum_test-recent_2026-10-10_daily_v1", "An expired reminder.", "2026-10-01")

	// One exchange after the latest summary's day, one on it.
	insertEpisodeWithTS(t, s, scope, "ep_test_recent_new", now)
	insertEpisodeWithTS(t, s, scope, "ep_test_recent_old", time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC))

	got, err := s.RecentSummaries(ctx, scope, scope, RecentQuery{Since: now.AddDate(0, 0, -7), Limit: 10}, now, ReadAudit{})
	if err != nil {
		t.Fatalf("RecentSummaries: %v", err)
	}
	if len(got.Summaries) != 2 {
		t.Fatalf("got %d summaries, want 2 (superseded and out-of-window excluded): %+v", len(got.Summaries), got.Summaries)
	}
	if got.Summaries[0].Period != "2026-10-10" || got.Summaries[1].ID != "sum_test-recent_2026-10-08_daily_v2" {
		t.Errorf("order/version wrong: %+v", got.Summaries)
	}
	if got.Summaries[1].Summary != "Corrected 8th: chose pgvector." {
		t.Errorf("summary prose not decrypted correctly: %q", got.Summaries[1].Summary)
	}
	facts := got.Summaries[0].KeyFacts
	if len(facts) != 1 || facts[0].Text != "Decided to ship MCP over HTTP." || facts[0].MemoryID == "" {
		t.Errorf("key facts = %+v, want only the grounded, unexpired one with a memory id", facts)
	}
	if got.UnconsolidatedExchanges != 1 {
		t.Errorf("UnconsolidatedExchanges = %d, want 1", got.UnconsolidatedExchanges)
	}
}

func insertEpisodeWithTS(t *testing.T, s *Store, scope identity.Scope, id string, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	enc, keyVersion, err := s.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	ct, err := enc.Encrypt("hello")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, scope_kind, scope_owner, key_version)
			values ($1, $2, 'interaction', $3, $3, $4, $5, $6, $7)
		`, id, ts, ct, hash(id), scope.Kind, scope.Owner, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("insert episode %s: %v", id, err)
	}
}
