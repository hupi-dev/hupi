-- Hardening Phase 4 (docs/HARDENING_PLAN.md D5-D7): one data encryption
-- key (DEK) per scope instead of one for the whole deployment, so a
-- compromised or leaked key only exposes the one scope it belongs to.
--
-- Storing wrapped (KEK-encrypted) DEKs in the same Postgres instance as
-- the data they protect is safe: the KEK itself never lives here (env var
-- today, a real KMS in a hardened build — see internal/bootstrap), so
-- possessing this table alone still isn't enough to decrypt anything.
--
-- Not RLS-protected: key material is looked up by internal/crypto.KeyStore
-- directly (application code, not per-request scoped queries), and every
-- lookup already filters by the exact (scope_kind, scope_owner) it needs.

create table scope_keys (
    scope_kind  text not null check (scope_kind in ('private', 'shared')),
    scope_owner text not null,
    wrapped_dek bytea not null,
    created_at  timestamptz not null default now(),
    primary key (scope_kind, scope_owner)
);

-- hupi_app (schema/0004) needs this table too — created after that
-- migration ran, so it needs its own grant.
grant select, insert, update, delete on scope_keys to hupi_app;
