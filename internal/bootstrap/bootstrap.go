// Package bootstrap is the shared wiring both entrypoints (cmd/hupi's
// gateway server and cmd/hupi-consolidate's cron job) need: read
// providers.yaml, connect to Postgres, load the field-encryption key.
// Factored out so neither main.go duplicates this, and so it's the one
// place that imports a concrete Postgres driver (see the blank import
// below) — internal/store and internal/consolidation never do.
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"gopkg.in/yaml.v3"

	"hupi/internal/crypto"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// Deps are the config-derived dependencies both entrypoints need.
type Deps struct {
	DB       *sql.DB
	Registry *provider.Registry
	Keys     *crypto.KeyStore // one Encryptor per scope, see docs/HARDENING_PLAN.md D5-D7
}

// Load reads providers.yaml (path from HUPI_PROVIDERS_CONFIG, default
// "./providers.yaml"), connects to Postgres (HUPI_DATABASE_URL), and loads
// the field-encryption key (HUPI_KEK + HUPI_WRAPPED_DEK_PATH) — see
// ARCHITECTURE.md §§ Storage security and Provider abstraction. Any
// missing/invalid piece of config fails Load outright rather than falling
// back to an unencrypted or unconfigured state.
func Load(ctx context.Context) (*Deps, error) {
	registry, err := loadRegistry(configPath())
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load provider config: %w", err)
	}

	dbURL, err := resolveDatabaseURL()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap: connect to database: %w", err)
	}

	keys, err := loadKeyStore(ctx, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrap: load field encryption keys: %w", err)
	}

	return &Deps{DB: db, Registry: registry, Keys: keys}, nil
}

// VerifyEmbedding checks that deps.Registry's active embedding profile
// actually produces vectors the schema can store (every embedding column
// is a fixed vector(1536) — see provider.EmbeddingDimensions), and swaps
// in a dimension-pinned wrapper via Registry.SetEmbedding if the profile
// needs (and supports) requesting that size explicitly. See
// provider.VerifyEmbeddingDimensions for how.
//
// Call this once, right after Load, from any entrypoint that's actually
// going to call Embed — cmd/hupi, hupi-consolidate, hupi-reembed,
// hupi-correct, hupi-trace, hupi-selfcheck — so a misconfigured
// active_embedding_provider fails immediately and loudly at startup
// instead of the first time a real consolidation run or hupi-reembed
// batch tries to write a wrong-length vector.
//
// Deliberately not folded into Load itself: several other entrypoints
// that also call Load (hupi-rotate-key, hupi-audit, hupi-export,
// hupi-import, hupi-admin) never call Embed at all, and shouldn't be
// blocked from starting by an embedding provider outage or
// misconfiguration that has nothing to do with what they're about to do.
func VerifyEmbedding(ctx context.Context, deps *Deps) error {
	verified, err := provider.VerifyEmbeddingDimensions(ctx, deps.Registry.Embedding())
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	deps.Registry.SetEmbedding(verified)
	return nil
}

// resolveDatabaseURL prefers HUPI_APP_DATABASE_URL — expected to
// authenticate as the non-owner `hupi_app` role
// (schema/0004_hardening_phase1_app_role.sql) — falling back to
// HUPI_DATABASE_URL if unset. Row-level security (docs/HARDENING_PLAN.md
// D4) is bypassed by table owners and superusers, so running only on the
// fallback means RLS enforces nothing for this process even after it's
// enabled — logged loudly here rather than left to be discovered later.
func resolveDatabaseURL() (string, error) {
	if url := os.Getenv("HUPI_APP_DATABASE_URL"); url != "" {
		return url, nil
	}
	slog.Warn("HUPI_APP_DATABASE_URL not set, falling back to HUPI_DATABASE_URL — " +
		"row-level security will not be enforced for this connection if the fallback role owns its tables or is a superuser")
	return requireEnv("HUPI_DATABASE_URL")
}

func configPath() string {
	if p := os.Getenv("HUPI_PROVIDERS_CONFIG"); p != "" {
		return p
	}
	return "providers.yaml"
}

// rawProviderProfile/rawConfig mirror providers.yaml's on-disk shape (see
// ARCHITECTURE.md § Provider abstraction) — kept separate from
// provider.ProfileConfig/provider.Config so the YAML tags don't leak into
// a package that has nothing to do with config file parsing.
type rawProviderProfile struct {
	Kind       string `yaml:"kind"`
	Vendor     string `yaml:"vendor"`
	BaseURL    string `yaml:"api_base"`
	APIKeyEnv  string `yaml:"api_key_env"`
	Model      string `yaml:"model"`
	APIVersion string `yaml:"api_version"`
}

type rawConfig struct {
	ActiveChatProvider          string                        `yaml:"active_chat_provider"`
	ActiveConsolidationProvider string                        `yaml:"active_consolidation_provider"`
	ActiveGroundingProvider     string                        `yaml:"active_grounding_provider"` // optional, defaults to consolidation's profile
	ActiveEmbeddingProvider     string                        `yaml:"active_embedding_provider"`
	Providers                   map[string]rawProviderProfile `yaml:"providers"`
}

func loadRegistry(path string) (*provider.Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg := provider.Config{
		ActiveChatProvider:          raw.ActiveChatProvider,
		ActiveConsolidationProvider: raw.ActiveConsolidationProvider,
		ActiveGroundingProvider:     raw.ActiveGroundingProvider,
		ActiveEmbeddingProvider:     raw.ActiveEmbeddingProvider,
		Providers:                   make(map[string]provider.ProfileConfig, len(raw.Providers)),
	}
	for name, p := range raw.Providers {
		var apiKey string
		if p.APIKeyEnv != "" {
			apiKey = os.Getenv(p.APIKeyEnv)
		}
		cfg.Providers[name] = provider.ProfileConfig{
			Kind:       provider.Kind(p.Kind),
			Vendor:     p.Vendor,
			BaseURL:    p.BaseURL,
			APIKey:     apiKey,
			Model:      p.Model,
			APIVersion: p.APIVersion,
		}
	}
	return provider.NewRegistry(cfg)
}

// loadKeyStore sources the shared KEK from HUPI_KEK (base64, 32 bytes) —
// still one per deployment, not per tenant, per docs/HARDENING_PLAN.md D5
// — and builds a crypto.KeyStore on top of it, one DEK per scope
// (schema/0006_hardening_phase4_scope_keys.sql). On first run against a
// deployment that predates per-tenant DEKs, migrates the legacy
// single-file DEK (HUPI_WRAPPED_DEK_PATH) into scope_keys under
// identity.DefaultScope (D7) — the already-wrapped bytes are reused
// as-is, not re-wrapped, so this never touches any already-encrypted
// data.
func loadKeyStore(ctx context.Context, db *sql.DB) (*crypto.KeyStore, error) {
	kek, err := loadKEK()
	if err != nil {
		return nil, err
	}

	if err := migrateLegacyDEK(ctx, db, kek); err != nil {
		return nil, fmt.Errorf("migrate legacy DEK: %w", err)
	}

	return crypto.NewKeyStore(db, kek), nil
}

// migrateLegacyDEK is a one-time, idempotent D7 migration: if
// scope_keys already has a row for identity.DefaultScope, it's a no-op.
// Otherwise, if the legacy wrapped-DEK file exists, its bytes become that
// scope's entry verbatim. If neither exists, there's nothing to migrate —
// KeyStore.GetOrCreate will generate identity.DefaultScope's key on first
// use, same as any other new scope.
func migrateLegacyDEK(ctx context.Context, db *sql.DB, kek []byte) error {
	var exists bool
	err := db.QueryRowContext(ctx,
		`select exists(select 1 from scope_keys where scope_kind = $1 and scope_owner = $2)`,
		identity.DefaultScope.Kind, identity.DefaultScope.Owner,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check for existing default-scope key: %w", err)
	}
	if exists {
		return nil
	}

	path := dekPath()
	wrapped, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // nothing to migrate; a fresh key will be generated on first use
	}
	if err != nil {
		return fmt.Errorf("read legacy wrapped DEK from %s: %w", path, err)
	}

	// Sanity-check it unwraps under the current KEK before persisting it —
	// fail loudly here rather than silently storing a key nothing can use.
	if _, err := crypto.UnwrapDEK(kek, wrapped); err != nil {
		return fmt.Errorf("legacy wrapped DEK from %s does not unwrap under HUPI_KEK: %w", path, err)
	}

	if _, err := db.ExecContext(ctx,
		`insert into users (id) values ($1) on conflict (id) do nothing`,
		identity.DefaultUserID,
	); err != nil {
		return fmt.Errorf("ensure default user exists: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		insert into scope_keys (scope_kind, scope_owner, version, wrapped_dek) values ($1, $2, 1, $3)
		on conflict (scope_kind, scope_owner, version) do nothing
	`, identity.DefaultScope.Kind, identity.DefaultScope.Owner, wrapped)
	if err != nil {
		return fmt.Errorf("persist migrated DEK: %w", err)
	}
	return nil
}

func loadKEK() ([]byte, error) {
	encoded, err := requireEnv("HUPI_KEK")
	if err != nil {
		return nil, err
	}
	kek, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("HUPI_KEK is not valid base64: %w", err)
	}
	return kek, nil
}

func dekPath() string {
	if p := os.Getenv("HUPI_WRAPPED_DEK_PATH"); p != "" {
		return p
	}
	return "hupi.dek.wrapped"
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}
	return v, nil
}
