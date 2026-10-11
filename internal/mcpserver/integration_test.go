package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
	"hupi/internal/store"
)

// These run /mcp end to end against a real Postgres (row-level security
// on, the hupi_app role) and skip when HUPI_TEST_DATABASE_URL is unset,
// like internal/store's integration tests.

type fakeEmbedder struct{}

var errNotImplemented = errors.New("fakeEmbedder: not implemented")

func (fakeEmbedder) Name() string   { return "fake" }
func (fakeEmbedder) Vendor() string { return "fake" }
func (fakeEmbedder) Model() string  { return "fake-embed" }
func (fakeEmbedder) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errNotImplemented
}
func (fakeEmbedder) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errNotImplemented
}
func (fakeEmbedder) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	vecs := make([][]float32, len(req.Input))
	for i := range vecs {
		vecs[i] = make([]float32, 1536)
		vecs[i][0] = 1
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

type pgFixture struct {
	db   *sql.DB
	keys *crypto.KeyStore
	st   *store.Store
}

func testPG(t *testing.T) pgFixture {
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
	keys := crypto.NewKeyStore(db, make([]byte, 32))
	return pgFixture{db: db, keys: keys, st: store.New(db, keys, fakeEmbedder{})}
}

func (f pgFixture) cleanup(t *testing.T, scopes ...identity.Scope) {
	t.Helper()
	for _, sc := range scopes {
		_ = dbscope.Run(context.Background(), f.db, sc, sc, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, sc.Kind, sc.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, sc.Kind, sc.Owner)
			return nil
		})
	}
}

func (f pgFixture) capture(t *testing.T, sc identity.Scope, input, output string) string {
	t.Helper()
	now := time.Now()
	ep := gateway.NewInteractionEpisode(gateway.NewEpisodeID(now), now, "test", "test-model", input, output)
	if err := f.st.Capture(context.Background(), sc, ep); err != nil {
		t.Fatalf("capture: %v", err)
	}
	return ep.ID
}

func (f pgFixture) entity(t *testing.T, sc identity.Scope, id, kind, name string) {
	t.Helper()
	enc, _, err := f.keys.GetOrCreate(context.Background(), sc)
	if err != nil {
		t.Fatalf("encryption key: %v", err)
	}
	attrs, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := dbscope.Run(context.Background(), f.db, sc, sc, func(tx *sql.Tx) error {
		_, err := tx.Exec(`insert into entities (id, kind, name, attributes, scope_kind, scope_owner) values ($1, $2, $3, $4, $5, $6)`,
			id, kind, name, attrs, sc.Kind, sc.Owner)
		return err
	}); err != nil {
		t.Fatalf("insert entity: %v", err)
	}
}

func TestIntegration_SearchMemoryIsScopedPerCaller(t *testing.T) {
	pg := testPG(t)
	alice := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-mcp-alice"}
	bob := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-mcp-bob"}
	pg.cleanup(t, alice, bob)
	t.Cleanup(func() { pg.cleanup(t, alice, bob) })

	// A plain keyword phrase: the chat route's stage-1 gate would skip it.
	epID := pg.capture(t, alice, "quokka ingestion pipeline uses a nightly batch", "Noted, nightly batch it is.")
	pg.entity(t, alice, "project:quokka", "project", "Quokka pipeline")

	s := quietServer(pg.st, fakeAuth{"ka": alice.Owner, "kb": bob.Owner}, Config{})
	asAlice := connect(t, s.Handler(), "ka")
	asBob := connect(t, s.Handler(), "kb")

	var hits SearchMemoryOutput
	call(t, asAlice, "search_memory", map[string]any{"query": "quokka ingestion pipeline"}, &hits)
	found := false
	for _, h := range hits.Hits {
		if h.Kind == identity.RefKindEpisode && h.ID == epID {
			found = true
		}
	}
	if !found {
		t.Errorf("alice's search didn't find her own episode %s: %+v", epID, hits)
	}

	call(t, asBob, "search_memory", map[string]any{"query": "quokka ingestion pipeline"}, &hits)
	for _, h := range hits.Hits {
		if h.ID == epID {
			t.Errorf("CROSS-SCOPE LEAK: bob's search returned alice's episode: %+v", h)
		}
	}

	var ent GetEntityOutput
	call(t, asAlice, "get_entity", map[string]any{"name_or_id": "quokka"}, &ent)
	if !ent.Found || ent.Entity.ID != "project:quokka" {
		t.Errorf("alice get_entity = %+v", ent)
	}
	call(t, asBob, "get_entity", map[string]any{"name_or_id": "project:quokka"}, &ent)
	if ent.Found || len(ent.Candidates) != 0 {
		t.Errorf("CROSS-SCOPE LEAK: bob get_entity = %+v", ent)
	}

	var list ListEntitiesOutput
	call(t, asBob, "list_entities", map[string]any{"name_contains": "quokka"}, &list)
	if len(list.Entities) != 0 {
		t.Errorf("CROSS-SCOPE LEAK: bob list_entities = %+v", list)
	}

	var recent RecentMemoryOutput
	call(t, asAlice, "recent_memory", nil, &recent)
	if recent.UnconsolidatedExchanges < 1 {
		t.Errorf("alice recent_memory unconsolidated = %d, want >= 1", recent.UnconsolidatedExchanges)
	}

	// Every read is audited as MCP-originated, under the caller's own scope.
	var n int
	if err := dbscope.Run(context.Background(), pg.db, alice, alice, func(tx *sql.Tx) error {
		return tx.QueryRow(`select count(*) from audit_log where acting_scope_owner = $1 and detail->>'via' = 'mcp'`, alice.Owner).Scan(&n)
	}); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if n < 3 {
		t.Errorf("mcp audit rows for alice = %d, want >= 3", n)
	}
}
