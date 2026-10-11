package hpmf

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestExportImport_SameTransactionVersionsKeepLineage is the real
// regression test for a TestExportImportRoundTrip_SameScope flake seen
// only under the full parallel ./master-test.sh run: seedScope writes v1
// and v2 in one transaction, so both get the same created_at (one
// now()), and exportSummaries' `order by level, period, created_at`
// left their relative order to whatever physical order Postgres read
// them in. Other packages' concurrent deletes/autovacuum on the shared
// summaries table occasionally put v2 first; importSummaries then saw
// v2 before its predecessor, imported it unlinked (a "supersedes
// reference not found in bundle" warning), and the round trip lost its
// correction chain.
//
// Forces that physical order deterministically instead of waiting for
// it: a non-HOT update to v1 (embedding is indexed, so this writes a new
// tuple and new index entries) after v2 is inserted puts v1's live tuple
// behind v2's in summaries_scope_idx, which is what the export reads.
func TestExportImport_SameTransactionVersionsKeepLineage(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-txorder-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-txorder-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "txorder")
	if err := dbscope.Run(ctx, db, src, src, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `update summaries set embedding = array_fill(0.1::real, array[1536])::vector where id = 'sum_txorder_v1'`)
		return err
	}); err != nil {
		t.Fatalf("rewrite v1's tuple: %v", err)
	}

	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "summaries", "daily", "2026-09-07.json"))
	if err != nil {
		t.Fatalf("read exported summaries file: %v", err)
	}
	var versions []SummaryRecord
	if err := json.Unmarshal(data, &versions); err != nil {
		t.Fatalf("parse exported summaries file: %v", err)
	}
	if len(versions) != 2 || versions[0].ID != "sum_txorder_v1" || versions[1].ID != "sum_txorder_v2" {
		ids := make([]string, len(versions))
		for i, v := range versions {
			ids[i] = v.ID
		}
		t.Fatalf("exported version order = %v, want [sum_txorder_v1 sum_txorder_v2] (predecessor first)", ids)
	}

	stats, err := ImportScope(ctx, db, keys, dir, dst, false, "test")
	if err != nil {
		t.Fatalf("ImportScope: %v", err)
	}
	if stats.SummariesWithDanglingSupersedes != 0 {
		t.Errorf("SummariesWithDanglingSupersedes = %d, want 0 — v1 was in the bundle", stats.SummariesWithDanglingSupersedes)
	}
	var linked int
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from summaries where scope_kind=$1 and scope_owner=$2 and supersedes is not null`, dst.Kind, dst.Owner).Scan(&linked)
	}); err != nil {
		t.Fatalf("count linked versions: %v", err)
	}
	if linked != 1 {
		t.Errorf("dst has %d linked (supersedes is not null) versions, want 1", linked)
	}
}

// TestOrderVersionChain covers what importSummaries relies on for a
// bundle exported before exportSummaries ordered its output: a file
// listing a successor before its predecessor still imports
// predecessor-first, and a dangling reference (TestImportScope_
// DanglingSupersedesImportedAsUnlinkedAndCounted's case) or a malformed
// cycle neither drops a version nor loops forever.
func TestOrderVersionChain(t *testing.T) {
	ids := func(rs []SummaryRecord) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.ID
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   []SummaryRecord
		want []string
	}{
		{"already ordered", []SummaryRecord{{ID: "v1"}, {ID: "v2", Supersedes: "v1"}, {ID: "v3", Supersedes: "v2"}}, []string{"v1", "v2", "v3"}},
		{"reversed", []SummaryRecord{{ID: "v3", Supersedes: "v2"}, {ID: "v2", Supersedes: "v1"}, {ID: "v1"}}, []string{"v1", "v2", "v3"}},
		{"dangling", []SummaryRecord{{ID: "v2", Supersedes: "missing"}}, []string{"v2"}},
		{"cycle", []SummaryRecord{{ID: "a", Supersedes: "b"}, {ID: "b", Supersedes: "a"}}, []string{"b", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(orderVersionChain(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
