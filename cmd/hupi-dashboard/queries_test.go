package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// testDB opens a connection to a real Postgres instance for integration
// testing — skips (not fails) if HUPI_TEST_DATABASE_URL isn't set, same
// convention internal/store/scope_isolation_test.go's testStore uses.
// None of these tests need a KeyStore/Encryptor: every panel query in
// queries.go reads only plaintext columns.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// cleanupScope deliberately never touches audit_log: hupi_app has no
// DELETE grant on it at all (schema/0007_audit_log.sql — append-only by
// design, a real security property of an audit trail, not an oversight).
// A delete attempt there would abort the whole transaction and silently
// roll back every other delete in it too — found for real the hard way,
// by seeing episodes/entities/summaries deletes that should have
// committed leave rows behind across test runs. Tests whose audit_log
// rows would otherwise accumulate across reruns (securityPosture) assert
// on presence/absence of specific events, not exact counts, for exactly
// this reason.
func cleanupScope(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from entity_relationships where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from scope_corpus_size where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from key_rotations where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
}

func hash(tag string) string {
	return "sha256:" + strings.Repeat("a", 58) + tag
}

func insertEpisode(t *testing.T, db *sql.DB, scope identity.Scope, id string, ts time.Time, importance float64) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into episodes (id, ts, type, hash, importance, scope_kind, scope_owner)
			values ($1, $2, 'interaction', $3, $4, $5, $6)
		`, id, ts, hash(id), importance, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode %s: %v", id, err)
	}
}

func insertEntityRow(t *testing.T, db *sql.DB, scope identity.Scope, id, kind, name string) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5)
		`, id, kind, name, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity %s: %v", id, err)
	}
}

func insertEntityRowWithLastUpdated(t *testing.T, db *sql.DB, scope identity.Scope, id, kind, name, lastUpdated string) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, last_updated, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5, $6)
		`, id, kind, name, lastUpdated, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity %s: %v", id, err)
	}
}

func insertSummaryRow(t *testing.T, db *sql.DB, scope identity.Scope, id, period string, entitiesTouched []string, correctionReason string) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (id, period, level, status, grounding_checked, entities_touched, correction_reason, scope_kind, scope_owner)
			values ($1, $2, 'daily', 'draft', true, $3::text[], $4, $5, $6)
		`, id, period, pgfmt.TextArray(entitiesTouched), pgfmt.Nullable(correctionReason), scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

func insertEntityRelationship(t *testing.T, db *sql.DB, scope identity.Scope, id, subject, predicate, object string) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entity_relationships (id, scope_kind, scope_owner, subject_id, predicate, object_id)
			values ($1, $2, $3, $4, $5, $6)
		`, id, scope.Kind, scope.Owner, subject, predicate, object)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity relationship %s: %v", id, err)
	}
}

func insertScopeCorpusSize(t *testing.T, db *sql.DB, scope identity.Scope, episodeCount, summaryCount int) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into scope_corpus_size (scope_kind, scope_owner, episode_count, summary_count)
			values ($1, $2, $3, $4)
			on conflict (scope_kind, scope_owner) do update
				set episode_count = excluded.episode_count, summary_count = excluded.summary_count
		`, scope.Kind, scope.Owner, episodeCount, summaryCount)
		return err
	})
	if err != nil {
		t.Fatalf("insert test scope_corpus_size: %v", err)
	}
}

func insertKeyRotation(t *testing.T, db *sql.DB, scope identity.Scope, fromVersion, toVersion int, status string) {
	t.Helper()
	err := dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into key_rotations (scope_kind, scope_owner, from_version, to_version, status)
			values ($1, $2, $3, $4, $5)
		`, scope.Kind, scope.Owner, fromVersion, toVersion, status)
		return err
	})
	if err != nil {
		t.Fatalf("insert test key_rotations row: %v", err)
	}
}

func TestConversationVolume_CountsInteractionEpisodesByDay(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-volume"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	today := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	insertEpisode(t, db, scope, "ep_vol_1", today, 0.5)
	insertEpisode(t, db, scope, "ep_vol_2", today, 0.5)
	insertEpisode(t, db, scope, "ep_vol_3", today.Add(-24*time.Hour), 0.5)

	points, err := conversationVolume(context.Background(), db, scope, 90)
	if err != nil {
		t.Fatalf("conversationVolume: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d days, want 2 — points: %+v", len(points), points)
	}
	byDay := map[string]int{}
	for _, p := range points {
		byDay[p.Day] = p.Count
	}
	todayStr := today.Format("2006-01-02")
	yesterdayStr := today.Add(-24 * time.Hour).Format("2006-01-02")
	if byDay[todayStr] != 2 {
		t.Errorf("today's count = %d, want 2", byDay[todayStr])
	}
	if byDay[yesterdayStr] != 1 {
		t.Errorf("yesterday's count = %d, want 1", byDay[yesterdayStr])
	}
}

func TestThemeWordCloud_CountsEntityMentionsAcrossCurrentSummariesOnly(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-themes"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	insertEntityRow(t, db, scope, "project:hupi", "project", "HUPI")
	insertEntityRow(t, db, scope, "person:alex", "person", "Alex")
	insertSummaryRow(t, db, scope, "sum_themes_1", "2026-01-01", []string{"project:hupi"}, "")
	insertSummaryRow(t, db, scope, "sum_themes_2", "2026-01-02", []string{"project:hupi", "person:alex"}, "")

	entries, err := themeWordCloud(context.Background(), db, scope, 50)
	if err != nil {
		t.Fatalf("themeWordCloud: %v", err)
	}
	byID := map[string]ThemeWordCloudEntry{}
	for _, e := range entries {
		byID[e.EntityID] = e
	}
	if byID["project:hupi"].Mentions != 2 {
		t.Errorf("project:hupi mentions = %d, want 2", byID["project:hupi"].Mentions)
	}
	if byID["person:alex"].Mentions != 1 {
		t.Errorf("person:alex mentions = %d, want 1", byID["person:alex"].Mentions)
	}
	if byID["project:hupi"].Name != "HUPI" || byID["project:hupi"].Kind != "project" {
		t.Errorf("project:hupi name/kind = %q/%q, want HUPI/project", byID["project:hupi"].Name, byID["project:hupi"].Kind)
	}
}

func TestEntityRelationshipGraph_ReturnsScopedRelationships(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-relationships"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	insertEntityRow(t, db, scope, "person:alex", "person", "Alex")
	insertEntityRow(t, db, scope, "project:hupi", "project", "HUPI")
	insertEntityRelationship(t, db, scope, "rel_1", "person:alex", "works_on", "project:hupi")

	rels, err := entityRelationshipGraph(context.Background(), db, scope, 200)
	if err != nil {
		t.Fatalf("entityRelationshipGraph: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d relationships, want 1", len(rels))
	}
	if rels[0].SubjectID != "person:alex" || rels[0].Predicate != "works_on" || rels[0].ObjectID != "project:hupi" {
		t.Errorf("relationship = %+v, want person:alex works_on project:hupi", rels[0])
	}
}

func TestMemoryHealth_ReflectsCorpusSizeAndCorrections(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-health"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	insertScopeCorpusSize(t, db, scope, 10, 3)
	insertSummaryRow(t, db, scope, "sum_health_1", "2026-01-01", nil, "")
	insertSummaryRow(t, db, scope, "sum_health_2", "2026-01-02", nil, "cross-period contradiction corrected")

	health, err := memoryHealth(context.Background(), db, scope)
	if err != nil {
		t.Fatalf("memoryHealth: %v", err)
	}
	if health.EpisodeCount != 10 || health.SummaryCount != 3 {
		t.Errorf("counts = %d/%d, want 10/3", health.EpisodeCount, health.SummaryCount)
	}
	if health.CorrectionsCount != 1 {
		t.Errorf("corrections = %d, want 1", health.CorrectionsCount)
	}
	if health.LastSummaryAt == nil {
		t.Error("expected LastSummaryAt to be set")
	}
}

func TestMemoryHealth_NoCorpusSizeRowYetIsNotAnError(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-health-empty"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	health, err := memoryHealth(context.Background(), db, scope)
	if err != nil {
		t.Fatalf("memoryHealth: %v", err)
	}
	if health.EpisodeCount != 0 || health.SummaryCount != 0 {
		t.Errorf("counts = %d/%d, want 0/0 for a brand-new scope", health.EpisodeCount, health.SummaryCount)
	}
}

func TestForgottenButImportant_FindsOnlyStaleHighImportanceEpisodesAndEntities(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-forgotten"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	now := time.Now().UTC()
	insertEpisode(t, db, scope, "ep_forgotten_stale_important", now.Add(-60*24*time.Hour), 0.9)   // stale + important: should surface
	insertEpisode(t, db, scope, "ep_forgotten_recent_important", now.Add(-1*24*time.Hour), 0.9)   // important but recent: should not
	insertEpisode(t, db, scope, "ep_forgotten_stale_unimportant", now.Add(-60*24*time.Hour), 0.2) // stale but unimportant: should not

	insertEntityRowWithLastUpdated(t, db, scope, "project:stale", "project", "Stale Project", now.Add(-60*24*time.Hour).Format("2006-01-02"))
	insertEntityRowWithLastUpdated(t, db, scope, "project:fresh", "project", "Fresh Project", now.Format("2006-01-02"))

	result, err := forgottenButImportant(context.Background(), db, scope, 0.7, 30, 20)
	if err != nil {
		t.Fatalf("forgottenButImportant: %v", err)
	}

	if len(result.Episodes) != 1 || result.Episodes[0].ID != "ep_forgotten_stale_important" {
		t.Errorf("episodes = %+v, want exactly ep_forgotten_stale_important", result.Episodes)
	}
	if len(result.Entities) != 1 || result.Entities[0].ID != "project:stale" {
		t.Errorf("entities = %+v, want exactly project:stale", result.Entities)
	}
}

func TestKeywordSearchGovernance_MatchesThresholdBoundaries(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-governance"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	g, err := keywordSearchGovernance(context.Background(), db, scope)
	if err != nil {
		t.Fatalf("keywordSearchGovernance: %v", err)
	}
	if g.Tier != "full" {
		t.Errorf("tier = %q, want full for a brand-new scope", g.Tier)
	}

	insertScopeCorpusSize(t, db, scope, 2000, 2000)
	g, err = keywordSearchGovernance(context.Background(), db, scope)
	if err != nil {
		t.Fatalf("keywordSearchGovernance: %v", err)
	}
	if g.Tier != "disabled" {
		t.Errorf("tier = %q, want disabled at 4000 total (default disable threshold 3000)", g.Tier)
	}
	if g.TotalCorpusSize != 4000 {
		t.Errorf("total corpus size = %d, want 4000", g.TotalCorpusSize)
	}
}

func TestSecurityPosture_ReturnsKeyRotationAndFiltersToSecurityRelevantEvents(t *testing.T) {
	db := testDB(t)
	// A unique owner per run, not a fixed one: audit_log rows can never
	// be deleted (cleanupScope's own doc comment) and this test asserts
	// an exact RecentEvents count, which a fixed scope owner would break
	// on the second and every later run.
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: fmt.Sprintf("user:test-dashboard-security-%d", time.Now().UnixNano())}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	insertKeyRotation(t, db, scope, 1, 2, "completed")

	if err := audit.LogStandalone(context.Background(), db, audit.Entry{
		EventType: audit.EventExport, Actor: scope.Owner, ActingScope: scope, WorkspaceScope: scope,
	}); err != nil {
		t.Fatalf("seed export audit event: %v", err)
	}
	if err := audit.LogStandalone(context.Background(), db, audit.Entry{
		EventType: audit.EventRetrieve, Actor: scope.Owner, ActingScope: scope, WorkspaceScope: scope,
	}); err != nil {
		t.Fatalf("seed retrieve audit event: %v", err)
	}

	posture, err := securityPosture(context.Background(), db, scope, 50)
	if err != nil {
		t.Fatalf("securityPosture: %v", err)
	}
	if posture.KeyRotation == nil || posture.KeyRotation.Status != "completed" {
		t.Fatalf("key rotation = %+v, want a completed rotation", posture.KeyRotation)
	}
	if len(posture.RecentEvents) != 1 || posture.RecentEvents[0].EventType != "export" {
		t.Errorf("recent events = %+v, want exactly one export event (retrieve must be filtered out)", posture.RecentEvents)
	}
}
