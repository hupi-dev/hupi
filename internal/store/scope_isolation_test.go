package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// This file is the concrete start of the integration test suite
// docs/TIER3_PLAN.md §9 calls a prerequisite, not optional parallel work:
// a real Postgres instance, checking the one thing that matters most for
// Tier 3 — that a scope boundary actually holds. Skips itself (not a
// failure) if HUPI_TEST_DATABASE_URL isn't set, so `go test ./...` stays
// green with no database available.
//
// Run against a real instance with, e.g.:
//
//	docker run -d -e POSTGRES_PASSWORD=hupi -e POSTGRES_DB=hupi -p 15432:5432 pgvector/pgvector:pg16
//	HUPI_ADMIN_DATABASE_URL='postgres://postgres:hupi@localhost:15432/hupi?sslmode=disable' schema/migrate.sh
//	psql "$HUPI_ADMIN_DATABASE_URL" -c "alter role hupi_app with password 'hupi'"
//	HUPI_TEST_DATABASE_URL='postgres://hupi_app:hupi@localhost:15432/hupi?sslmode=disable' go test ./internal/store/...
//
// Must connect as hupi_app, not postgres: RLS is deliberately bypassed
// by the table owner and superusers (schema/0004's own comment), so
// rls_test.go's enforcement checks would silently pass for the wrong
// reason — or rather, fail to catch a real regression — under a
// superuser connection.

var errFakeNotImplemented = errors.New("fakeEmbedder: not implemented, not needed by these tests")

// fakeEmbedder returns a deterministic, content-independent vector — these
// tests check scope isolation, not similarity ranking, so a real embedding
// model isn't needed. ChatCompletion/StreamChatCompletion are unused by
// Store and deliberately not implemented.
type fakeEmbedder struct{}

func (fakeEmbedder) Name() string   { return "fake" }
func (fakeEmbedder) Vendor() string { return "fake" }
func (fakeEmbedder) Model() string  { return "fake-embed" }

func (fakeEmbedder) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errFakeNotImplemented
}

func (fakeEmbedder) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (fakeEmbedder) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	vecs := make([][]float32, len(req.Input))
	for i := range req.Input {
		v := make([]float32, 1536)
		v[0] = 1 // same nonzero vector for everything; fine, see doc comment
		vecs[i] = v
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test (see file doc comment)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
	return New(db, keys, fakeEmbedder{})
}

// hash produces a syntactically valid, unique-enough dedup hash for test
// episodes — the real format (sha256:<hex>) is enforced by
// schema/0001_init.sql's check on hash's shape only loosely (it's just
// `not null`), but callers elsewhere always produce this shape, so tests
// should too.
func hash(tag string) string {
	return "sha256:" + strings.Repeat("a", 58) + tag
}

// TestScopeIsolation_Entities is the exact scenario docs/TIER3_PLAN.md D2
// worried about: two different scopes each have an entity with the same
// id. Capture/Retrieve must never let one scope see the other's.
func TestScopeIsolation_Entities(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	userScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-iso-a"}
	teamScope := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-iso-a"}
	t.Cleanup(func() { cleanupScope(t, s, userScope); cleanupScope(t, s, teamScope) })

	insertEntity(t, s, userScope, "project:hupi", "project", "HUPI (user copy)")
	insertEntity(t, s, teamScope, "project:hupi", "project", "HUPI (team copy)")

	// Stage 1 exact-match retrieval for the user asking about "hupi" must
	// only ever surface the user's own copy of project:hupi, never the
	// team's, even though both rows share the same bare id.
	result, err := s.Retrieve(ctx, userScope, userScope, []provider.Message{
		{Role: provider.RoleUser, Content: "tell me about the hupi project"},
	})
	if err != nil {
		t.Fatalf("Retrieve (user scope): %v", err)
	}
	if !strings.Contains(result.ContextMessage, "user copy") {
		t.Errorf("expected user scope's own project:hupi in context, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "team copy") {
		t.Errorf("CROSS-SCOPE LEAK: user scope's retrieval saw the team's project:hupi: %q", result.ContextMessage)
	}
	for _, ref := range result.Refs {
		if ref.Scope != userScope {
			t.Errorf("CROSS-SCOPE LEAK: user scope's retrieval returned a ref outside its own scope: %+v", ref)
		}
	}
}

// TestScopeIsolation_Episodes confirms a private episode never surfaces
// in another scope's vector search, even when the content would otherwise
// match well.
func TestScopeIsolation_Episodes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	userScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-iso-b"}
	teamScope := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-iso-b"}
	t.Cleanup(func() { cleanupScope(t, s, userScope); cleanupScope(t, s, teamScope) })

	secretEpisodeID := "ep_test_iso_secret"
	err := s.Capture(ctx, userScope, gateway.Episode{
		ID: secretEpisodeID, TS: time.Now(), Type: "interaction",
		InputText: "what's my private salary", OutputText: "your private salary is a secret number",
		Hash: hash("secret"), Importance: 0.9,
	})
	if err != nil {
		t.Fatalf("capture into user scope: %v", err)
	}
	// Directly embed it (bypassing consolidation's importance-threshold
	// batch job, which isn't under test here) so vector search has
	// something to find. Must run inside a scoped transaction under RLS
	// (docs/HARDENING_PLAN.md D2) — an unscoped UPDATE would silently
	// affect zero rows (RLS's USING clause hides the row instead of
	// erroring), which would make this test pass for the wrong reason:
	// not because scope isolation blocked the match, but because there'd
	// be nothing to match at all.
	testVector := make([]float32, 1536)
	testVector[0] = 1
	err = dbscope.Run(ctx, s.db, userScope, userScope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`update episodes set embedding = $1::vector where id = $2`,
			pgfmt.VectorLiteral(testVector), secretEpisodeID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed embedding for %s: %v", secretEpisodeID, err)
	}
	// Confirm the seed actually took effect before trusting the isolation
	// check below — see the comment above on why a silent no-op here
	// would make this test meaningless. This check must itself run scoped
	// (as userScope, which can see the row) — an unscoped read would also
	// see zero rows under RLS regardless of whether the seed worked.
	var embeddedCount int
	err = dbscope.Run(ctx, s.db, userScope, userScope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from episodes where id = $1 and embedding is not null`, secretEpisodeID).Scan(&embeddedCount)
	})
	if err != nil {
		t.Fatalf("verify embedding seed: %v", err)
	}
	if embeddedCount != 1 {
		t.Fatalf("test setup failed: expected the seed episode to have an embedding, found %d", embeddedCount)
	}

	result, err := s.Retrieve(ctx, teamScope, teamScope, []provider.Message{
		{Role: provider.RoleUser, Content: "what did we decide about salary last time"},
	})
	if err != nil {
		t.Fatalf("Retrieve (team scope): %v", err)
	}
	if strings.Contains(result.ContextMessage, "secret number") {
		t.Errorf("CROSS-SCOPE LEAK: team scope's retrieval surfaced the user's private episode: %q", result.ContextMessage)
	}
	for _, ref := range result.Refs {
		if ref.ID == secretEpisodeID {
			t.Errorf("CROSS-SCOPE LEAK: team scope's retrieval returned a ref to the user's private episode")
		}
	}
}

// TestSelfModelAnchorsToActingUser is docs/TIER3_PLAN.md D3, directly: a
// turn happening inside a team's shared workspace must still anchor to
// the *acting user's own* self_model, never the workspace's — even though
// self_model:test-d3 lives in a completely different scope than the
// workspace being searched.
func TestSelfModelAnchorsToActingUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	userScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-d3"}
	teamScope := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-d3"}
	t.Cleanup(func() { cleanupScope(t, s, userScope); cleanupScope(t, s, teamScope) })

	insertEntity(t, s, userScope, "self_model:user:test-d3", "self_model", "test-d3")

	// A message with no stage-1 signal is enough here: buildAnchor runs
	// (and returns) before any workspace search does, so this exercises
	// exactly the anchor logic under test without needing workspace
	// content to match against.
	result, err := s.Retrieve(ctx, userScope, teamScope, []provider.Message{
		{Role: provider.RoleUser, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !strings.Contains(result.ContextMessage, "self_model") {
		t.Errorf("expected the acting user's self_model in the anchor even in a team workspace, got: %q", result.ContextMessage)
	}
	foundSelfModelRef := false
	for _, ref := range result.Refs {
		if ref.ID == "self_model:user:test-d3" {
			foundSelfModelRef = true
			if ref.Scope != userScope {
				t.Errorf("self_model ref scope = %+v, want the acting user's scope %+v", ref.Scope, userScope)
			}
		}
	}
	if !foundSelfModelRef {
		t.Error("expected a ref to the acting user's self_model entity")
	}
}

// insertEntity, like any real write, must run inside a scoped transaction
// under RLS (docs/HARDENING_PLAN.md D2) — the WITH CHECK clause rejects an
// unscoped insert outright (a loud failure here, unlike a read's silent
// zero rows), which is exactly the fail-closed behavior the policy is
// designed to have.
func insertEntity(t *testing.T, s *Store, scope identity.Scope, id, kind, name string) {
	t.Helper()
	// Must use scope's own KeyStore-resolved encryptor — real reads of
	// this row (buildAnchor, formatEntity) resolve their decryptor the
	// same way, and a mismatched key would fail to decrypt.
	enc, _, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt test entity attrs: %v", err)
	}
	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5, $6)
		`, id, kind, name, attrsCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity: %v", err)
	}
}

// cleanupScope's deletes must also run scoped — under RLS, an unscoped
// DELETE doesn't error, it just matches zero rows (USING hides them), which
// would silently stop cleaning up test data between runs.
func cleanupScope(t *testing.T, s *Store, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
}
