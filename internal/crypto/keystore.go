package crypto

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"hupi/internal/identity"
)

// KeyStore resolves and caches Encryptors per (scope, version)
// (docs/HARDENING_PLAN.md D5-D7, docs/GAP_CLOSURE_PLAN.md §4.4): each
// scope's data encryption key (DEK) is generated once per version,
// wrapped under the deployment's single key-encryption key (KEK), and
// persisted in the `scope_keys` table (schema/0006, versioned by
// schema/0010). The KEK itself never touches Postgres — only wrapped DEKs
// live there.
//
// A scope has exactly one *current* (highest) version most of the time,
// and briefly two — the old and the new — while hupi-rotate-key is
// migrating its data (internal/rotate). That's why there are two ways to
// ask for a key, not one:
//
//   - GetOrCreate returns the *current* version — the only thing any
//     write path should ever call, so a write made mid-rotation lands on
//     the new key automatically, never the one being retired.
//   - GetVersion returns a *specific* version — the only thing any read
//     path should call, using whatever key_version a row actually
//     recorded when it was written, since a row written before a
//     rotation completed may still be on the old version.
//
// Before this package existed with versioning, every table's rows shared
// one implicit key per scope and decrypt never had to ask which one.
type KeyStore struct {
	db  *sql.DB
	kek []byte

	mu    sync.Mutex
	cache map[scopeVersion]*Encryptor
}

type scopeVersion struct {
	scope   identity.Scope
	version int
}

func NewKeyStore(db *sql.DB, kek []byte) *KeyStore {
	return &KeyStore{db: db, kek: kek, cache: make(map[scopeVersion]*Encryptor)}
}

// GetOrCreate returns the Encryptor for scope's current (highest)
// version, and that version number — callers about to write new
// ciphertext record this as the row's key_version. Generates version 1 if
// scope has no key at all yet. Deliberately re-checks the current version
// against the database on every call rather than trusting a long-lived
// cache of "the" version for a scope: a long-running process (the
// gateway) must notice a rotation another process (hupi-rotate-key)
// started and switch to the new version for its very next write, not
// keep writing under the old one until restarted. What *is* cached is
// the (scope, version) -> Encryptor mapping, so that check costs one
// indexed query, not a repeated unwrap.
func (k *KeyStore) GetOrCreate(ctx context.Context, scope identity.Scope) (*Encryptor, int, error) {
	version, err := k.CurrentVersion(ctx, scope)
	if err != nil {
		return nil, 0, err
	}
	if version == 0 {
		version, err = k.createVersion(ctx, scope, 1)
		if err != nil {
			return nil, 0, err
		}
	}
	enc, err := k.GetVersion(ctx, scope, version)
	if err != nil {
		return nil, 0, err
	}
	return enc, version, nil
}

// GetVersion returns the Encryptor for scope's specific, already-existing
// version — used to decrypt a row using its own key_version, never to
// create one (a version comes into existence only via GetOrCreate's
// version-1 case or CreateNextVersion's rotation case).
func (k *KeyStore) GetVersion(ctx context.Context, scope identity.Scope, version int) (*Encryptor, error) {
	key := scopeVersion{scope, version}
	if enc := k.cached(key); enc != nil {
		return enc, nil
	}

	var wrapped []byte
	err := k.db.QueryRowContext(ctx,
		`select wrapped_dek from scope_keys where scope_kind = $1 and scope_owner = $2 and version = $3`,
		scope.Kind, scope.Owner, version,
	).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("crypto: no key for %s:%s version %d — versions are never deleted except by an explicit prune, this row's data may be unrecoverable", scope.Kind, scope.Owner, version)
	}
	if err != nil {
		return nil, fmt.Errorf("crypto: load DEK for %s:%s version %d: %w", scope.Kind, scope.Owner, version, err)
	}

	enc, err := k.unwrap(scope, version, wrapped)
	if err != nil {
		return nil, err
	}
	k.store(key, enc)
	return enc, nil
}

// CurrentVersion returns scope's highest existing version, or 0 if it has
// no key yet (a scope that's never captured anything) — 0 is never a
// valid real version (they start at 1), so callers can treat it as "not
// provisioned yet" without a separate bool.
func (k *KeyStore) CurrentVersion(ctx context.Context, scope identity.Scope) (int, error) {
	var version int
	err := k.db.QueryRowContext(ctx,
		`select coalesce(max(version), 0) from scope_keys where scope_kind = $1 and scope_owner = $2`,
		scope.Kind, scope.Owner,
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("crypto: determine current key version for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	return version, nil
}

// CreateNextVersion generates a brand-new DEK as scope's next version
// (current+1) and persists it — used only by internal/rotate to start a
// rotation. Existing versions are untouched; this is purely additive.
// Race-safe the same way createVersion is: on a concurrent caller winning
// the insert first, reloads whatever version actually exists at that
// slot rather than assuming its own generated DEK won.
func (k *KeyStore) CreateNextVersion(ctx context.Context, scope identity.Scope) (int, error) {
	current, err := k.CurrentVersion(ctx, scope)
	if err != nil {
		return 0, err
	}
	if current == 0 {
		return 0, fmt.Errorf("crypto: %s:%s has no existing key to rotate — nothing to do", scope.Kind, scope.Owner)
	}
	return k.createVersion(ctx, scope, current+1)
}

// createVersion generates a fresh DEK for scope at exactly `version` and
// persists it, handling the case where another process created the same
// (scope, version) concurrently (ON CONFLICT DO NOTHING, then reload —
// never trust a locally generated DEK over whichever one actually won the
// race, or two processes could end up encrypting under different keys
// for what's supposed to be the same version).
func (k *KeyStore) createVersion(ctx context.Context, scope identity.Scope, version int) (int, error) {
	dek, err := GenerateDEK()
	if err != nil {
		return 0, fmt.Errorf("crypto: generate DEK for %s:%s version %d: %w", scope.Kind, scope.Owner, version, err)
	}
	wrapped, err := WrapDEK(k.kek, dek)
	if err != nil {
		return 0, fmt.Errorf("crypto: wrap DEK for %s:%s version %d: %w", scope.Kind, scope.Owner, version, err)
	}
	_, err = k.db.ExecContext(ctx, `
		insert into scope_keys (scope_kind, scope_owner, version, wrapped_dek) values ($1, $2, $3, $4)
		on conflict (scope_kind, scope_owner, version) do nothing
	`, scope.Kind, scope.Owner, version, wrapped)
	if err != nil {
		return 0, fmt.Errorf("crypto: persist DEK for %s:%s version %d: %w", scope.Kind, scope.Owner, version, err)
	}
	// Whether this call's insert won the race or lost it to a concurrent
	// caller, GetVersion below loads and caches whatever is actually
	// there now — never this call's own possibly-discarded dek.
	if _, err := k.GetVersion(ctx, scope, version); err != nil {
		return 0, fmt.Errorf("crypto: reload DEK for %s:%s version %d: %w", scope.Kind, scope.Owner, version, err)
	}
	return version, nil
}

func (k *KeyStore) unwrap(scope identity.Scope, version int, wrapped []byte) (*Encryptor, error) {
	dek, err := UnwrapDEK(k.kek, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK for %s:%s version %d (wrong KEK?): %w", scope.Kind, scope.Owner, version, err)
	}
	return NewEncryptor(dek)
}

// Evict removes scope's specific version from this KeyStore's in-memory
// cache, if present — called by internal/rotate.Prune right after it
// deletes that version's wrapped DEK from the database, so this process
// stops being able to serve decrypts from a key that's supposed to be
// gone. This only affects the calling process's own cache: a separate,
// already-running process (a live gateway) that resolved the same
// version earlier keeps its own cached copy until it restarts — pruning
// removes the *persisted* key material an attacker with future database
// access could use, not every process's memory everywhere, and that
// distinction is worth knowing before treating a prune as an instant,
// total revocation.
func (k *KeyStore) Evict(scope identity.Scope, version int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.cache, scopeVersion{scope, version})
}

func (k *KeyStore) cached(key scopeVersion) *Encryptor {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cache[key]
}

func (k *KeyStore) store(key scopeVersion, enc *Encryptor) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cache[key] = enc
}
