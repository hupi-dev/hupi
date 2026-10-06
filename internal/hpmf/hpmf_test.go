package hpmf

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// these against a real Postgres instance.

func testDB(t *testing.T) (*sql.DB, *crypto.KeyStore) {
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
	return db, crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
}

func cleanupScope(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
	db.Exec(`delete from audit_log where workspace_scope_kind = $1 and workspace_scope_owner = $2`, scope.Kind, scope.Owner)
}

// seedScope writes one episode, one two-version summary (with a key
// fact), and one entity into scope — enough to exercise every record
// kind ExportScope/ImportScope handle, including the supersedes chain.
func seedScope(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, tag string) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key for %s: %v", tag, err)
	}
	ts := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	inputCT, _ := enc.Encrypt("input " + tag)
	outputCT, _ := enc.Encrypt("output " + tag)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ($1, $2, 'interaction', $3, $4, $5, 0.6, $6, $7)
		`, "ep_"+tag, ts, inputCT, outputCT, "sha256:"+tag, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode for %s: %v", tag, err)
	}

	v1CT, _ := enc.Encrypt("summary v1 " + tag)
	v2CT, _ := enc.Encrypt("summary v2 " + tag)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, source_episode_ids, scope_kind, scope_owner)
			values ($1, '2026-09-07', 'daily', $2, $3, $4, $5)
		`, "sum_"+tag+"_v1", v1CT, "{ep_"+tag+"}", scope.Kind, scope.Owner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, scope_kind, scope_owner)
			values ($1, $2, $3, true, $4, $5)
		`, "sum_"+tag+"_v1", v1CT, "{ep_"+tag+"}", scope.Kind, scope.Owner); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, source_episode_ids, supersedes, correction_reason, scope_kind, scope_owner)
			values ($1, '2026-09-07', 'daily', $2, $3, $4, 'fixed a mistake', $5, $6)
		`, "sum_"+tag+"_v2", v2CT, "{ep_"+tag+"}", "sum_"+tag+"_v1", scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed summary for %s: %v", tag, err)
	}

	tagCT, _ := enc.Encrypt(tag)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, scope_kind, scope_owner)
			values ($1, 'project', $2, $3, $4)
		`, "project:"+tag, "Project "+tag, scope.Kind, scope.Owner); err != nil {
			return err
		}
		// A real, post-cutover attribute row (internal/consolidation/store.go's
		// upsertEntities shape) — entities.attributes is no longer where
		// live attribute data lives, see exportEntities' own doc comment.
		_, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, is_static, grounded)
			values ($1, $2, $3, $4, 'tag', $5, true, true)
		`, "mem_"+tag, scope.Kind, scope.Owner, "project:"+tag, tagCT)
		return err
	})
	if err != nil {
		t.Fatalf("seed entity for %s: %v", tag, err)
	}
}

func decryptedSummaryText(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string) string {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	var ct []byte
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select summary from summaries where id = $1`, id).Scan(&ct)
	})
	if err != nil {
		t.Fatalf("load summary %s: %v", id, err)
	}
	text, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt summary %s: %v", id, err)
	}
	return text
}

func TestExportImportRoundTrip_SameScope(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "roundtrip")

	dir := t.TempDir()
	sm, err := ExportScope(ctx, db, keys, src, dir, "test")
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	if sm.EpisodeCount != 1 || sm.SummaryCount != 2 || sm.EntityCount != 1 {
		t.Fatalf("manifest = %+v, want 1 episode, 2 summaries (v1+v2), 1 entity", sm)
	}

	// Regression check: attributes must serialize as a nested JSON object
	// in entities.jsonl, not a string holding escaped JSON — the whole
	// point of a "human-readable, diffable" export (MEMORY_FORMAT.md's
	// own design principle #2).
	entitiesRaw, err := os.ReadFile(filepath.Join(dir, "entities.jsonl"))
	if err != nil {
		t.Fatalf("read entities.jsonl: %v", err)
	}
	if want := `"attributes":{"tag":"roundtrip"}`; !strings.Contains(string(entitiesRaw), want) {
		t.Errorf("entities.jsonl = %s, want it to contain %s (attributes as a nested object)", entitiesRaw, want)
	}

	stats, err := ImportScope(ctx, db, keys, dir, dst, false, "test")
	if err != nil {
		t.Fatalf("ImportScope: %v", err)
	}
	if stats.EpisodesImported != 1 || stats.SummariesImported != 2 || stats.EntitiesImported != 1 {
		t.Fatalf("stats = %+v, want 1/2/1 imported", stats)
	}

	// Content round-trips, decrypted, through the target scope's own key —
	// not just "a row exists."
	var episodeCount int
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from episodes where scope_kind=$1 and scope_owner=$2`, dst.Kind, dst.Owner).Scan(&episodeCount)
	}); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 1 {
		t.Errorf("dst has %d episodes, want 1", episodeCount)
	}

	var dstV1ID, dstV2ID, supersedes string
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where scope_kind=$1 and scope_owner=$2 and supersedes is null`, dst.Kind, dst.Owner).Scan(&dstV1ID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id, supersedes from summaries where scope_kind=$1 and scope_owner=$2 and supersedes is not null`, dst.Kind, dst.Owner).Scan(&dstV2ID, &supersedes)
	}); err != nil {
		t.Fatalf("load imported summary ids: %v", err)
	}
	if supersedes != dstV1ID {
		t.Errorf("imported v2's supersedes = %q, want it remapped to the imported v1's own new id %q (not the source scope's id)", supersedes, dstV1ID)
	}
	if got := decryptedSummaryText(t, ctx, db, keys, dst, dstV2ID); got != "summary v2 roundtrip" {
		t.Errorf("decrypted summary text = %q, want %q", got, "summary v2 roundtrip")
	}
}

// TestExportImportAuditEveryPhaseSeparately is a real regression test
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A10): ExportScope/
// ImportScope used to write one combined audit_log entry only after all
// phases (episodes/summaries/entities, later joined by memory_relations
// once docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's fields gained a
// portable representation) finished — a crash partway through left
// real, already-written/already-committed data with zero audit trail.
// Each now logs one entry per phase, immediately after that phase
// completes. Confirms 4 export entries and 4 import entries exist, not
// 1 combined one of each — using a high-water-mark query rather than
// relying on cleanup, since audit_log is genuinely append-only (the
// hupi_app role has no DELETE grant on it at all).
func TestExportImportAuditEveryPhaseSeparately(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-audit-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-audit-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "audit-phase")

	var beforeID int64
	if err := db.QueryRowContext(ctx, `select coalesce(max(id), 0) from audit_log`).Scan(&beforeID); err != nil {
		t.Fatalf("capture audit_log high-water mark: %v", err)
	}

	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	if _, err := ImportScope(ctx, db, keys, dir, dst, false, "test"); err != nil {
		t.Fatalf("ImportScope: %v", err)
	}

	countPhases := func(scope identity.Scope, eventType string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			select count(*) from audit_log
			where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = $3 and id > $4
		`, scope.Kind, scope.Owner, eventType, beforeID).Scan(&n); err != nil {
			t.Fatalf("count %s audit entries: %v", eventType, err)
		}
		return n
	}

	if got := countPhases(src, "export"); got != 4 {
		t.Errorf("got %d export audit_log entries, want 4 (one per phase: episodes, summaries, entities, memory_relations) — not 1 combined entry", got)
	}
	if got := countPhases(dst, "import"); got != 4 {
		t.Errorf("got %d import audit_log entries, want 4 (one per phase: entities, summaries, episodes, memory_relations) — not 1 combined entry", got)
	}
}

// TestExportImportRoundTrip_PreservesKeyFactFields is the real-infra
// regression for docs/DESIGN_VS_BUILT.md #8: source_count/expires_at/
// expire_reason/is_inference had no portable representation at all
// before KeyFactRecord gained these fields — an export/import round
// trip silently dropped them.
func TestExportImportRoundTrip_PreservesKeyFactFields(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-keyfact-fields-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-keyfact-fields-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	enc, keyVersion, err := keys.GetOrCreate(ctx, src)
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	summaryCT, _ := enc.Encrypt("Joanna went hiking and discussed allergies.")
	plainFactCT, _ := enc.Encrypt("Joanna went on a hike.")
	inferredFactCT, _ := enc.Encrypt("Joanna likely has asthma.")

	summaryID := "sum_keyfact-fields_v1"
	if err := dbscope.Run(ctx, db, src, src, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, key_version)
			values ($1, '2026-09-07', 'daily', $2, $3, $4, $5)
		`, summaryID, summaryCT, src.Kind, src.Owner, keyVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into summary_key_facts (summary_id, fact, grounded, scope_kind, scope_owner, key_version, source_count, expires_at, expire_reason, is_inference)
			values ($1, $2, true, $3, $4, $5, 1, null, null, false)
		`, summaryID, plainFactCT, src.Kind, src.Owner, keyVersion); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			insert into summary_key_facts (summary_id, fact, grounded, scope_kind, scope_owner, key_version, source_count, expires_at, expire_reason, is_inference)
			values ($1, $2, true, $3, $4, $5, 3, '2026-12-31', 'inferred from allergy attributes', true)
		`, summaryID, inferredFactCT, src.Kind, src.Owner, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("seed summary + key facts: %v", err)
	}

	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	if _, err := ImportScope(ctx, db, keys, dir, dst, false, "test"); err != nil {
		t.Fatalf("ImportScope: %v", err)
	}

	dstEnc, _, err := keys.GetOrCreate(ctx, dst)
	if err != nil {
		t.Fatalf("resolve dst key: %v", err)
	}
	var foundPlain, foundInferred bool
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select fact, source_count, expires_at::text, expire_reason, is_inference
			from summary_key_facts where scope_kind = $1 and scope_owner = $2
		`, dst.Kind, dst.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var factCT []byte
			var sourceCount int
			var expiresAt, expireReason sql.NullString
			var isInference bool
			if err := rows.Scan(&factCT, &sourceCount, &expiresAt, &expireReason, &isInference); err != nil {
				return err
			}
			fact, err := dstEnc.Decrypt(factCT)
			if err != nil {
				return err
			}
			switch fact {
			case "Joanna went on a hike.":
				if sourceCount == 1 && !expiresAt.Valid && !isInference {
					foundPlain = true
				}
			case "Joanna likely has asthma.":
				if sourceCount == 3 && expiresAt.String == "2026-12-31" && expireReason.String == "inferred from allergy attributes" && isInference {
					foundInferred = true
				}
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load imported key facts: %v", err)
	}
	if !foundPlain {
		t.Error("missing the plain fact with source_count=1, no expiration, is_inference=false")
	}
	if !foundInferred {
		t.Error("missing the inferred fact with source_count=3, expires_at, expire_reason, is_inference=true all preserved")
	}

	// The memories mirror (needed for RefKindMemory citations and, as of
	// this fix, memory_relations resolution) must exist too — imported
	// key facts never got one before this fix.
	var memoriesCount int
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from memories where scope_kind = $1 and scope_owner = $2 and is_static = false
		`, dst.Kind, dst.Owner).Scan(&memoriesCount)
	}); err != nil {
		t.Fatalf("count memories mirror rows: %v", err)
	}
	if memoriesCount != 2 {
		t.Errorf("memories mirror rows for imported key facts = %d, want 2", memoriesCount)
	}
}

// TestExportImportRoundTrip_PreservesMemoryRelations is the real-infra
// regression for the other half of docs/DESIGN_VS_BUILT.md #8:
// memory_relations had no portable representation at all. Seeds both
// locator shapes a MemoryLocator can take — a key-fact-to-key-fact
// `updates` edge across a supersedes chain (exercising summaryIDMap's
// cross-version remapping), and a key-fact-to-attribute `derives` edge
// — then confirms both resolve correctly against the freshly-imported
// data in a different target scope.
func TestExportImportRoundTrip_PreservesMemoryRelations(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-relations-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-relations-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	enc, keyVersion, err := keys.GetOrCreate(ctx, src)
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}

	insertKeyFactWithMirror := func(tx *sql.Tx, summaryID, fact string, isInference bool) string {
		t.Helper()
		factCT, err := enc.Encrypt(fact)
		if err != nil {
			t.Fatalf("encrypt %q: %v", fact, err)
		}
		var factRowID int64
		if err := tx.QueryRowContext(ctx, `
			insert into summary_key_facts (summary_id, fact, grounded, scope_kind, scope_owner, key_version, is_inference)
			values ($1, $2, true, $3, $4, $5, $6)
			returning id
		`, summaryID, factCT, src.Kind, src.Owner, keyVersion, isInference).Scan(&factRowID); err != nil {
			t.Fatalf("insert key fact %q: %v", fact, err)
		}
		memID := memoryIDForImportedKeyFact(factRowID)
		if _, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, summary_id, content, key_version, is_static, grounded, is_inference)
			values ($1, $2, $3, $4, $5, $6, false, true, $7)
		`, memID, src.Kind, src.Owner, summaryID, factCT, keyVersion, isInference); err != nil {
			t.Fatalf("insert memories mirror for %q: %v", fact, err)
		}
		return memID
	}

	var oldFactMemID, newFactMemID, inferredFactMemID, attrMemID string
	if err := dbscope.Run(ctx, db, src, src, func(tx *sql.Tx) error {
		oldCT, _ := enc.Encrypt("summary v1")
		newCT, _ := enc.Encrypt("summary v2")
		if _, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, key_version)
			values ('sum_relations_v1', '2026-09-07', 'daily', $1, $2, $3, $4)
		`, oldCT, src.Kind, src.Owner, keyVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, supersedes, correction_reason, scope_kind, scope_owner, key_version)
			values ('sum_relations_v2', '2026-09-07', 'daily', $1, 'sum_relations_v1', 'pre-approval amount updated', $2, $3, $4)
		`, newCT, src.Kind, src.Owner, keyVersion); err != nil {
			return err
		}
		oldFactMemID = insertKeyFactWithMirror(tx, "sum_relations_v1", "Wells Fargo pre-approval amount is $250,000", false)
		newFactMemID = insertKeyFactWithMirror(tx, "sum_relations_v2", "Wells Fargo pre-approval amount is $300,000", false)
		inferredFactMemID = insertKeyFactWithMirror(tx, "sum_relations_v2", "Joanna likely has asthma.", true)

		if _, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, scope_kind, scope_owner)
			values ('person:joanna', 'person', 'Joanna', $1, $2)
		`, src.Kind, src.Owner); err != nil {
			return err
		}
		allergyCT, _ := enc.Encrypt("most reptiles and animals with fur")
		attrMemID = "mem_joanna_allergic_to"
		if _, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
			values ($1, $2, $3, 'person:joanna', 'allergic_to', $4, $5, true, true)
		`, attrMemID, src.Kind, src.Owner, allergyCT, keyVersion); err != nil {
			return err
		}

		for _, rel := range []struct{ relType, from, to string }{
			{"updates", newFactMemID, oldFactMemID},
			{"derives", inferredFactMemID, attrMemID},
		} {
			relID, err := newImportRelationID()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				insert into memory_relations (id, scope_kind, scope_owner, from_memory_id, to_memory_id, relation_type)
				values ($1, $2, $3, $4, $5, $6)
			`, relID, src.Kind, src.Owner, rel.from, rel.to, rel.relType); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed relations scope: %v", err)
	}

	dir := t.TempDir()
	sm, err := ExportScope(ctx, db, keys, src, dir, "test")
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	if sm.MemoryRelationCount != 2 {
		t.Fatalf("manifest MemoryRelationCount = %d, want 2", sm.MemoryRelationCount)
	}

	relationsRaw, err := os.ReadFile(filepath.Join(dir, "memory_relations.jsonl"))
	if err != nil {
		t.Fatalf("read memory_relations.jsonl: %v", err)
	}
	if !strings.Contains(string(relationsRaw), `"relation_type":"updates"`) || !strings.Contains(string(relationsRaw), `"relation_type":"derives"`) {
		t.Errorf("memory_relations.jsonl = %s, want both an updates and a derives record", relationsRaw)
	}

	stats, err := ImportScope(ctx, db, keys, dir, dst, false, "test")
	if err != nil {
		t.Fatalf("ImportScope: %v", err)
	}
	if stats.MemoryRelationsImported != 2 {
		t.Fatalf("stats.MemoryRelationsImported = %d, want 2 (stats: %+v)", stats.MemoryRelationsImported, stats)
	}
	if stats.MemoryRelationsSkipped != 0 {
		t.Errorf("stats.MemoryRelationsSkipped = %d, want 0", stats.MemoryRelationsSkipped)
	}

	dstEnc, _, err := keys.GetOrCreate(ctx, dst)
	if err != nil {
		t.Fatalf("resolve dst key: %v", err)
	}
	factTextByMemID := func(tx *sql.Tx, memID string) (fact string, isStatic bool, entityID, attrKey string) {
		t.Helper()
		var contentCT []byte
		var eid, akey sql.NullString
		if err := tx.QueryRowContext(ctx, `
			select content, is_static, entity_id, attribute_key from memories where id = $1 and scope_kind = $2 and scope_owner = $3
		`, memID, dst.Kind, dst.Owner).Scan(&contentCT, &isStatic, &eid, &akey); err != nil {
			t.Fatalf("load memories row %s: %v", memID, err)
		}
		if isStatic {
			return "", true, eid.String, akey.String
		}
		text, err := dstEnc.Decrypt(contentCT)
		if err != nil {
			t.Fatalf("decrypt memories row %s: %v", memID, err)
		}
		return text, false, "", ""
	}

	var foundUpdates, foundDerives bool
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select relation_type, from_memory_id, to_memory_id from memory_relations
			where scope_kind = $1 and scope_owner = $2
		`, dst.Kind, dst.Owner)
		if err != nil {
			return err
		}
		type relRow struct{ relType, fromID, toID string }
		var relRows []relRow
		for rows.Next() {
			var r relRow
			if err := rows.Scan(&r.relType, &r.fromID, &r.toID); err != nil {
				rows.Close()
				return err
			}
			relRows = append(relRows, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, r := range relRows {
			switch r.relType {
			case "updates":
				fromFact, _, _, _ := factTextByMemID(tx, r.fromID)
				toFact, _, _, _ := factTextByMemID(tx, r.toID)
				if fromFact == "Wells Fargo pre-approval amount is $300,000" && toFact == "Wells Fargo pre-approval amount is $250,000" {
					foundUpdates = true
				}
			case "derives":
				fromFact, _, _, _ := factTextByMemID(tx, r.fromID)
				_, isStatic, entityID, attrKey := factTextByMemID(tx, r.toID)
				if fromFact == "Joanna likely has asthma." && isStatic && entityID == "person:joanna" && attrKey == "allergic_to" {
					foundDerives = true
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("load imported memory_relations: %v", err)
	}
	if !foundUpdates {
		t.Error("missing the updates edge resolved correctly across the supersedes chain (summaryIDMap remapping)")
	}
	if !foundDerives {
		t.Error("missing the derives edge resolved correctly to the attribute's memories row")
	}
}

func TestImportScope_FreshRejectsNonEmpty(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-nonempty"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	seedScope(t, ctx, db, keys, scope, "existing")
	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, scope, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	if _, err := ImportScope(ctx, db, keys, dir, scope, false, "test"); err == nil {
		t.Fatal("expected fresh (non-merge) import into a non-empty scope to fail")
	}
}

func TestImportScope_MergeDedupsOnSecondImport(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-mergesrc"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-mergedst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "merge")
	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	first, err := ImportScope(ctx, db, keys, dir, dst, true, "test")
	if err != nil {
		t.Fatalf("first ImportScope: %v", err)
	}
	if first.EpisodesImported != 1 || first.SummariesImported != 2 || first.EntitiesImported != 1 {
		t.Fatalf("first import stats = %+v, want everything imported", first)
	}

	second, err := ImportScope(ctx, db, keys, dir, dst, true, "test")
	if err != nil {
		t.Fatalf("second ImportScope: %v", err)
	}
	if second.EpisodesImported != 0 || second.EpisodesSkipped != 1 {
		t.Errorf("second import episodes = %+v, want everything skipped", second)
	}
	if second.EntitiesImported != 0 || second.EntitiesSkipped != 1 {
		t.Errorf("second import entities = %+v, want everything skipped", second)
	}
	if second.SummariesImported != 0 || second.SummariesSkipped != 2 {
		t.Errorf("second import summaries = %+v, want everything skipped", second)
	}
}

func TestExportBundle_WholeDeployment_ScopeIsolation(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-bundle-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-hpmf-bundle-b"}
	t.Cleanup(func() { cleanupScope(t, db, scopeA); cleanupScope(t, db, scopeB) })

	seedScope(t, ctx, db, keys, scopeA, "bundlea")
	seedScope(t, ctx, db, keys, scopeB, "bundleb")

	tempDir, manifest, err := ExportBundle(ctx, db, keys, []identity.Scope{scopeA, scopeB}, "test")
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tempDir) })

	if len(manifest.Scopes) != 2 {
		t.Fatalf("manifest has %d scopes, want 2", len(manifest.Scopes))
	}
	for _, sm := range manifest.Scopes {
		if sm.Dir == "." {
			t.Errorf("whole-deployment bundle scope %s:%s has flat Dir %q, want nested scopes/... path", sm.ScopeKind, sm.ScopeOwner, sm.Dir)
		}
	}

	// Import each into a fresh target and confirm B's data never lands in
	// A's target scope or vice versa.
	targetA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-bundle-a-restored"}
	targetB := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-hpmf-bundle-b-restored"}
	t.Cleanup(func() { cleanupScope(t, db, targetA); cleanupScope(t, db, targetB) })

	for _, sm := range manifest.Scopes {
		target := targetA
		if sm.ScopeKind == scopeB.Kind && sm.ScopeOwner == scopeB.Owner {
			target = targetB
		}
		if _, err := ImportScope(ctx, db, keys, filepath.Join(tempDir, sm.Dir), target, false, "test"); err != nil {
			t.Fatalf("ImportScope %s into %s:%s: %v", sm.Dir, target.Kind, target.Owner, err)
		}
	}

	if got := decryptedEntityName(t, ctx, db, keys, targetA, "project:bundlea"); got != "Project bundlea" {
		t.Errorf("targetA entity name = %q, want %q", got, "Project bundlea")
	}
	if got := decryptedEntityName(t, ctx, db, keys, targetB, "project:bundleb"); got != "Project bundleb" {
		t.Errorf("targetB entity name = %q, want %q", got, "Project bundleb")
	}
	if hasEntity(t, ctx, db, targetA, "project:bundleb") {
		t.Error("scope B's entity leaked into scope A's restored target")
	}
	if hasEntity(t, ctx, db, targetB, "project:bundlea") {
		t.Error("scope A's entity leaked into scope B's restored target")
	}
}

func decryptedEntityName(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string) string {
	t.Helper()
	var name string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select name from entities where scope_kind=$1 and scope_owner=$2 and id=$3`, scope.Kind, scope.Owner, id).Scan(&name)
	}); err != nil {
		t.Fatalf("load entity %s: %v", id, err)
	}
	return name
}

func hasEntity(t *testing.T, ctx context.Context, db *sql.DB, scope identity.Scope, id string) bool {
	t.Helper()
	var count int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from entities where scope_kind=$1 and scope_owner=$2 and id=$3`, scope.Kind, scope.Owner, id).Scan(&count)
	}); err != nil {
		t.Fatalf("check entity %s: %v", id, err)
	}
	return count > 0
}

func TestPackAndEncrypt_RoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "episodes", "2026", "09"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "hello from a test fixture\n"
	if err := os.WriteFile(filepath.Join(srcDir, "episodes", "2026", "09", "2026-09-07.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	ageIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "bundle.age")
	if err := PackAndEncrypt(srcDir, outPath, []age.Recipient{ageIdentity.Recipient()}); err != nil {
		t.Fatalf("PackAndEncrypt: %v", err)
	}

	destDir := t.TempDir()
	if err := DecryptAndUnpack(outPath, destDir, []age.Identity{ageIdentity}); err != nil {
		t.Fatalf("DecryptAndUnpack: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "episodes", "2026", "09", "2026-09-07.jsonl"))
	if err != nil {
		t.Fatalf("read round-tripped file: %v", err)
	}
	if string(got) != content {
		t.Errorf("round-tripped content = %q, want %q", got, content)
	}
}

func TestDecryptAndUnpack_WrongIdentityFails(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	correctIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	wrongIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "bundle.age")
	if err := PackAndEncrypt(srcDir, outPath, []age.Recipient{correctIdentity.Recipient()}); err != nil {
		t.Fatalf("PackAndEncrypt: %v", err)
	}
	if err := DecryptAndUnpack(outPath, t.TempDir(), []age.Identity{wrongIdentity}); err == nil {
		t.Error("expected decryption with the wrong identity to fail")
	}
}
