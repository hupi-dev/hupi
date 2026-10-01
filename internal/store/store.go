// Package store implements gateway.Capturer and gateway.Retriever against
// Postgres + pgvector (schema/0001_init.sql). It's one package, one Store
// type, because both halves share the same *sql.DB and key store —
// Capture writes encrypted columns, Retrieve reads and decrypts them, and
// there's no reason to duplicate that plumbing across two packages.
//
// Only database/sql (stdlib) is imported here, not a concrete driver —
// registering one (blank-importing github.com/jackc/pgx/v5/stdlib) is
// internal/bootstrap's job, so this package's own tests/build never need
// a live driver registered, and swapping drivers later doesn't touch it.
package store

import (
	"database/sql"

	"hupi/internal/crypto"
	"hupi/internal/gateway"
	"hupi/internal/provider"
)

// Store implements both gateway.Capturer and gateway.Retriever.
type Store struct {
	db   *sql.DB
	keys *crypto.KeyStore // one Encryptor per scope, see docs/HARDENING_PLAN.md D5-D7

	// embedder is the active_embedding_provider (see
	// ARCHITECTURE.md § Provider abstraction) — used at query time to
	// embed the user's message for the pgvector search in Retrieve.
	embedder provider.Provider
}

func New(db *sql.DB, keys *crypto.KeyStore, embedder provider.Provider) *Store {
	return &Store{db: db, keys: keys, embedder: embedder}
}

// currentEmbeddingModel identifies the active embedding provider the same
// way internal/consolidation.EmbedderIdentity does ("<vendor>:<model>",
// schema/0013_embedding_model_tracking.sql) — duplicated here rather than
// imported, since store and consolidation are deliberately sibling
// packages with no dependency between them (see this package's own doc
// comment), and this is a one-line, dependency-free computation, the same
// "small tools duplicate rather than force an awkward cross-package
// dependency" convention already used elsewhere in this repo (e.g.
// internal/gateway/attribution.go's extractJSON).
//
// Used at retrieval time (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A3)
// to exclude vectors recorded under a *different*, no-longer-active
// embedding model from vector search — without this, a provider switch
// silently mixed cosine-incomparable embedding spaces in the same ranked
// result set until a full hupi-reembed backfill completed, with no error
// anywhere.
func (s *Store) currentEmbeddingModel() string {
	return s.embedder.Vendor() + ":" + s.embedder.Model()
}

var (
	_ gateway.Capturer  = (*Store)(nil)
	_ gateway.Retriever = (*Store)(nil)
)
