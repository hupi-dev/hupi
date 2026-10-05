package consolidation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// cleanupRelationshipScope deletes everything a relationships test seeds
// or produces, run inside a scoped transaction like every other write/
// read against RLS-protected tables in this package's tests.
func cleanupRelationshipScope(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from entity_relationships where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})
}

func seedEpisode(t *testing.T, ctx context.Context, db *sql.DB, runner *Runner, scope identity.Scope, id string, date time.Time) {
	t.Helper()
	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("seed input for " + id)
	outputCT, _ := enc.Encrypt("seed output for " + id)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ($1, $2, 'interaction', $3, $4, 'sha256:test', 0.5, $5, $6)
		`, id, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode %s: %v", id, err)
	}
}

// TestRunDaily_WritesRelationship exercises the real RunDaily ->
// storeSummary -> upsertRelationships path end to end: a relationship in
// the consolidation LLM's output should land in entity_relationships,
// referencing the same canonicalized entity ids entities_touched
// produces (see docs/ENTITY_RELATIONSHIPS_PLAN.md §§3-4).
func TestRunDaily_WritesRelationship(t *testing.T) {
	consolidationJSON := `{
		"summary": "Caroline started working at Acme Corp.",
		"key_facts": [{"fact": "Caroline works at Acme Corp", "source_episode_ids": ["ep_rel_1"]}],
		"entities_touched": [
			{"id": "person:caroline", "kind": "person", "name": "Caroline", "attributes": {}},
			{"id": "organization:acme-corp", "kind": "organization", "name": "Acme Corp", "attributes": {}}
		],
		"relationships": [
			{"subject_kind": "person", "subject_name": "Caroline", "predicate": "works_at", "object_kind": "organization", "object_name": "Acme Corp", "valid_from": "2026-01-15", "valid_until": ""}
		]
	}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-1"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	date := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	seedEpisode(t, ctx, db, runner, scope, "ep_rel_1", date)

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var predicate, subjectID, objectID string
	var validFrom sql.NullString
	var validUntil sql.NullString
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select predicate, subject_id, object_id, valid_from::text, valid_until::text
			from entity_relationships
			where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&predicate, &subjectID, &objectID, &validFrom, &validUntil)
	})
	if err != nil {
		t.Fatalf("expected a relationship row: %v", err)
	}
	if predicate != "works_at" {
		t.Errorf("predicate = %q, want works_at", predicate)
	}
	if subjectID != "person:caroline" {
		t.Errorf("subject_id = %q, want person:caroline (same canonicalization entities_touched gets)", subjectID)
	}
	if objectID != "organization:acme-corp" {
		t.Errorf("object_id = %q, want organization:acme-corp", objectID)
	}
	if !validFrom.Valid || validFrom.String != "2026-01-15" {
		t.Errorf("valid_from = %+v, want 2026-01-15", validFrom)
	}
	if validUntil.Valid {
		t.Errorf("valid_until = %+v, want null (still current)", validUntil)
	}
}

// TestRunDaily_SkipsRelationshipReferencingUnextractedEntity reproduces a
// real failure found running cmd/hupi-bench at scale (Step 5 of the
// benchmark harness plan): the consolidation LLM's relationships[] list
// and its entities_touched[] list are two independent parts of the same
// JSON output, and nothing stops the model naming a relationship endpoint
// that entities_touched never declared (or declared under a different
// kind/name that canonicalizes to a different id) — subject_id/object_id
// are hard foreign keys into entities, so that used to fail the entire
// day's consolidation with a raw FK-violation error instead of just
// dropping the one malformed relationship, exactly like an invalid entity
// kind or an unresolvable name already does.
func TestRunDaily_SkipsRelationshipReferencingUnextractedEntity(t *testing.T) {
	consolidationJSON := `{
		"summary": "Caroline mentioned a charity race.",
		"key_facts": [{"fact": "Caroline mentioned a charity race", "source_episode_ids": ["ep_rel_2"]}],
		"entities_touched": [
			{"id": "person:caroline", "kind": "person", "name": "Caroline", "attributes": {}}
		],
		"relationships": [
			{"subject_kind": "person", "subject_name": "Caroline", "predicate": "took_part_in", "object_kind": "project", "object_name": "Charity Race", "valid_from": "", "valid_until": ""}
		]
	}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-unextracted"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	date := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	seedEpisode(t, ctx, db, runner, scope, "ep_rel_2", date)

	// The real bug: this used to return an error (FK violation), failing
	// the whole day. It must now succeed, having simply skipped the one
	// relationship whose object was never extracted as an entity.
	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v (relationship referencing an unextracted entity should be skipped, not fail the whole day)", err)
	}

	var count int
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if count != 0 {
		t.Errorf("relationship count = %d, want 0 (the only relationship references project:charity-race, which was never in entities_touched)", count)
	}

	// The summary itself must still have been written — one bad
	// relationship shouldn't take the rest of the day's consolidation
	// down with it.
	var summaryCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryCount)
	})
	if err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if summaryCount != 1 {
		t.Errorf("summary count = %d, want 1", summaryCount)
	}
}

// TestUpsertRelationships_SupersedesOpenEndedEdgeOnExplicitValidFrom
// covers the conservative supersession rule from
// docs/ENTITY_RELATIONSHIPS_PLAN.md §5: an existing open-ended edge for
// the same (subject, predicate) closes only when the *new* edge gives an
// explicit valid_from, not merely because a second object showed up.
func TestUpsertRelationships_SupersedesOpenEndedEdgeOnExplicitValidFrom(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-2"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	// Seed both entities directly (this test exercises upsertRelationships
	// in isolation, not the full RunDaily path) so the FK is satisfiable.
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:dana", "person", "Dana"},
			{"organization:old-co", "organization", "Old Co"},
			{"organization:new-co", "organization", "New Co"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	mustUpsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	// First: Dana works at Old Co, no stated end date (open-ended).
	mustUpsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Dana", Predicate: "works_at", ObjectKind: "organization", ObjectName: "Old Co", ValidFrom: "2020-01-01"},
	})

	// Second, a later consolidation: Dana now works at New Co, with an
	// explicit new valid_from — this SHOULD close the Old Co edge.
	mustUpsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Dana", Predicate: "works_at", ObjectKind: "organization", ObjectName: "New Co", ValidFrom: "2026-06-01"},
	})

	// entity_relationships has RLS enabled, same as episodes/summaries/
	// entities — an unscoped read (plain db.QueryContext) would silently
	// see zero rows rather than fail loudly, so this must run inside
	// dbscope.Run like every other read in this package's tests.
	got := map[string]sql.NullString{}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select object_id, valid_until::text from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:dana' and predicate = 'works_at'
			order by object_id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var objectID string
			var validUntil sql.NullString
			if err := rows.Scan(&objectID, &validUntil); err != nil {
				return err
			}
			got[objectID] = validUntil
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query relationships: %v", err)
	}

	if vu, ok := got["organization:old-co"]; !ok {
		t.Fatal("expected the old-co edge to still exist (superseded, not deleted)")
	} else if !vu.Valid || vu.String != "2026-06-01" {
		t.Errorf("old-co edge valid_until = %+v, want 2026-06-01 (closed at the new edge's valid_from)", vu)
	}
	if vu, ok := got["organization:new-co"]; !ok {
		t.Fatal("expected the new-co edge to exist")
	} else if vu.Valid {
		t.Errorf("new-co edge valid_until = %+v, want null (still current)", vu)
	}
}

// TestUpsertRelationships_DoesNotSupersedeWithoutExplicitValidFrom is the
// safety check for the "one-to-many predicate" concern
// docs/ENTITY_RELATIONSHIPS_PLAN.md §5 raises: a second edge for the same
// (subject, predicate) with NO stated valid_from must NOT close the
// first one — otherwise a genuinely concurrent, one-to-many relationship
// (e.g. friends_with) would silently lose one of its edges every time a
// new one is extracted.
func TestUpsertRelationships_DoesNotSupersedeWithoutExplicitValidFrom(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-3"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:eli", "person", "Eli"},
			{"person:frankie", "person", "Frankie"},
			{"person:gale", "person", "Gale"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	upsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	// Eli is friends with Frankie (no date — friendships rarely have a
	// stated start), then later, separately, Eli is friends with Gale
	// too. Both should coexist.
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Eli", Predicate: "friends_with", ObjectKind: "person", ObjectName: "Frankie"},
	})
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Eli", Predicate: "friends_with", ObjectKind: "person", ObjectName: "Gale"},
	})

	// Scoped, same reason as the supersession test above — RLS makes an
	// unscoped read silently see zero rows rather than fail loudly.
	var count int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:eli'
			  and predicate = 'friends_with' and valid_until is null
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if count != 2 {
		t.Errorf("open friends_with edges for Eli = %d, want 2 (both should still be open — no valid_from given, so neither should have been superseded)", count)
	}
}

// TestUpsertRelationships_SkipsBareEdgeWhenAnyRecordOfTheSameTripleExists
// is the regression test for a real production duplicate: the exact-match
// check only catches a repeat with the *same* (valid_from, valid_until),
// so a dateless mention of a relationship that was already recorded with
// specific dates (a realistic scenario when clustering splits one day's
// text into separate topic groups — see mergeConsolidationOutputs'
// concatenation-with-no-cross-cluster-dedup) used to insert a third,
// strictly-less-informative row for the exact same (subject, predicate,
// object) triple. This is deliberately not the same mechanism as
// DoesNotSupersedeWithoutExplicitValidFrom above — it never closes or
// alters an existing row (so one-to-many predicates like friends_with
// still work, see TestUpsertRelationships_SkipsBareEdgeOnlyForSameObject
// below), it only skips inserting a new one that adds no information.
func TestUpsertRelationships_SkipsBareEdgeWhenAnyRecordOfTheSameTripleExists(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-bare-dup"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:priya", "person", "Priya"},
			{"organization:acme", "organization", "Acme"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	upsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	// First: a dated stint is recorded. Then: the same (subject,
	// predicate, object) triple is extracted again with no dates at all
	// — a realistic "a different part of the same day's text mentioned
	// it without dates" case, not a literal re-consolidation repeat.
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Priya", Predicate: "worked_at", ObjectKind: "organization", ObjectName: "Acme",
			ValidFrom: "2019-01-01", ValidUntil: "2022-01-01"},
	})
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Priya", Predicate: "worked_at", ObjectKind: "organization", ObjectName: "Acme"},
	})

	var count int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:priya'
			  and predicate = 'worked_at' and object_id = 'organization:acme'
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for person:priya worked_at organization:acme = %d, want exactly 1 (the bare second mention should have been skipped as adding no new information)", count)
	}
}

// TestUpsertRelationships_SkipsBareEdgeOnlyForSameObject confirms the new
// bare-edge skip is scoped to the same object — it must not reintroduce
// the one-to-many edge-loss concern DoesNotSupersedeWithoutExplicitValidFrom
// above already protects against for a genuinely different object.
func TestUpsertRelationships_SkipsBareEdgeOnlyForSameObject(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-bare-diffobj"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:priya", "person", "Priya"},
			{"organization:acme", "organization", "Acme"},
			{"organization:globex", "organization", "Globex"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	upsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Priya", Predicate: "worked_at", ObjectKind: "organization", ObjectName: "Acme",
			ValidFrom: "2019-01-01", ValidUntil: "2022-01-01"},
	})
	// A bare edge to a *different* object must still be inserted — this
	// isn't a duplicate of anything.
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Priya", Predicate: "worked_at", ObjectKind: "organization", ObjectName: "Globex"},
	})

	var count int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:priya' and predicate = 'worked_at'
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if count != 2 {
		t.Errorf("worked_at edges for person:priya = %d, want 2 (bare edge to a different object must not be skipped)", count)
	}
}

// TestUpsertRelationships_CanonicalizesBasedInToLivesInForPersonSubjects
// confirms the based_in/lives_in synonym fix: a person's residence
// extracted as "based_in" on one day and "lives_in" on another must land
// as the same predicate, so they dedup instead of showing up as two
// separate-looking relationships for the same fact.
func TestUpsertRelationships_CanonicalizesBasedInToLivesInForPersonSubjects(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-based-in-alias"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:sujith", "person", "Sujith"},
			{"place:espoo", "place", "Espoo"},
			{"organization:acme", "organization", "Acme"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	upsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Sujith", Predicate: "based_in", ObjectKind: "place", ObjectName: "Espoo"},
	})
	upsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Sujith", Predicate: "lives_in", ObjectKind: "place", ObjectName: "Espoo"},
	})
	// An organization's based_in is a different, legitimate predicate
	// (a registered location, not a residence) and must NOT be rewritten.
	upsert([]RelationshipUpdate{
		{SubjectKind: "organization", SubjectName: "Acme", Predicate: "based_in", ObjectKind: "place", ObjectName: "Espoo"},
	})

	var basedInCount, livesInCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:sujith' and predicate = 'based_in'
		`, scope.Kind, scope.Owner).Scan(&basedInCount); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:sujith' and predicate = 'lives_in'
		`, scope.Kind, scope.Owner).Scan(&livesInCount)
	})
	if err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	if basedInCount != 0 {
		t.Errorf("person:sujith based_in edges = %d, want 0 (should have been canonicalized to lives_in)", basedInCount)
	}
	if livesInCount != 1 {
		t.Errorf("person:sujith lives_in edges = %d, want exactly 1 (both mentions should have merged into one)", livesInCount)
	}

	var orgBasedInCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'organization:acme' and predicate = 'based_in'
		`, scope.Kind, scope.Owner).Scan(&orgBasedInCount)
	})
	if err != nil {
		t.Fatalf("count org relationships: %v", err)
	}
	if orgBasedInCount != 1 {
		t.Errorf("organization:acme based_in edges = %d, want 1 (organization's based_in must not be rewritten to lives_in)", orgBasedInCount)
	}
}

// TestUpsertRelationships_DoesNotCorruptEdgeWhenAnEarlierStartingOneArrivesLater
// is the regression test for a real, confirmed bug found during a full
// 48-instance LongMemEval run (round 5, six scopes/predicates hit it
// independently — see docs/LONGMEMEVAL_ACCURACY_PLAN.md): consolidation
// doesn't process periods in strict chronological order (cross-period
// contradiction correction revisits earlier periods after later ones are
// already stored), so an open-ended edge's own valid_from can be *later*
// than a new edge's valid_from for the same (subject, predicate). The
// old "close superseded edge" UPDATE blindly set valid_until to the new
// edge's (earlier) valid_from regardless, producing valid_from >
// valid_until and crashing the whole day's consolidation on
// entity_relationships_valid_date_order_check
// (schema/0019_entity_relationships_valid_date_order.sql) — not just a
// skipped relationship, the real production failure mode this
// reproduces.
func TestUpsertRelationships_DoesNotCorruptEdgeWhenAnEarlierStartingOneArrivesLater(t *testing.T) {
	runner, db := testRunner(t, "", "")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-relationships-out-of-order"}
	ctx := context.Background()
	cleanupRelationshipScope(t, db, scope)

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, e := range []struct{ id, kind, name string }{
			{"person:dana", "person", "Dana"},
			{"organization:old-co", "organization", "Old Co"},
			{"organization:new-co", "organization", "New Co"},
		} {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, $2, $3, $4, $5)
			`, e.id, e.kind, e.name, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	mustUpsert := func(updates []RelationshipUpdate) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return runner.upsertRelationships(ctx, tx, scope, updates, "")
		})
		if err != nil {
			t.Fatalf("upsertRelationships: %v", err)
		}
	}

	// First (processed first, chronologically later): Dana works at New
	// Co starting 2026-06-01, open-ended.
	mustUpsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Dana", Predicate: "works_at", ObjectKind: "organization", ObjectName: "New Co", ValidFrom: "2026-06-01"},
	})

	// Second (processed second, but chronologically *earlier* — the
	// out-of-order case): Dana worked at Old Co starting 2020-01-01. Must
	// not crash, and must not set New Co's valid_until before its own
	// valid_from.
	mustUpsert([]RelationshipUpdate{
		{SubjectKind: "person", SubjectName: "Dana", Predicate: "works_at", ObjectKind: "organization", ObjectName: "Old Co", ValidFrom: "2020-01-01"},
	})

	got := map[string]sql.NullString{}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select object_id, valid_until::text from entity_relationships
			where scope_kind = $1 and scope_owner = $2 and subject_id = 'person:dana' and predicate = 'works_at'
			order by object_id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var objectID string
			var validUntil sql.NullString
			if err := rows.Scan(&objectID, &validUntil); err != nil {
				return err
			}
			got[objectID] = validUntil
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query relationships: %v", err)
	}

	if vu, ok := got["organization:new-co"]; !ok {
		t.Fatal("expected the new-co edge to still exist")
	} else if vu.Valid {
		t.Errorf("new-co edge valid_until = %+v, want null (its own valid_from is later than old-co's, so it must not be closed by old-co's earlier valid_from)", vu)
	}
	if vu, ok := got["organization:old-co"]; !ok {
		t.Fatal("expected the old-co edge to exist")
	} else if vu.Valid {
		t.Errorf("old-co edge valid_until = %+v, want null (nothing should have closed it)", vu)
	}
}
