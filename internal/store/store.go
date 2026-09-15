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

var (
	_ gateway.Capturer  = (*Store)(nil)
	_ gateway.Retriever = (*Store)(nil)
)
