#!/usr/bin/env sh
# Applies every schema/NNNN_*.sql migration in order, idempotently —
# the containerized/Kubernetes counterpart to install.sh's
# apply_all_migrations, for exactly the same reason: schema/*.sql has no
# `IF NOT EXISTS` guards, so a Kubernetes Job that might run again on a
# Helm upgrade needs to check what's already applied before re-running
# raw `CREATE TABLE`/`ALTER TABLE` statements.
#
# Deliberately a second implementation of the same probe logic rather
# than install.sh calling this script (or vice versa): one is a bare-metal
# bash script assuming a real TTY and interactive prompts, the other a
# `sh`-only, non-interactive container entrypoint with no prompts at
# all — sharing code between them would mean threading interactivity
# concerns through logic that has none, for marginal reuse of what's
# fundamentally an ordered list plus a lookup table.
#
# Usage: HUPI_ADMIN_DATABASE_URL=postgres://... ./migrate.sh
set -eu

: "${HUPI_ADMIN_DATABASE_URL:?HUPI_ADMIN_DATABASE_URL must be set to a superuser/owner DSN}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

probe() {
  case "$1" in
    0001_init.sql)                        echo "select (to_regclass('public.episodes') is not null)" ;;
    0002_tier3_phase1_identity.sql)       echo "select exists(select 1 from information_schema.columns where table_name='episodes' and column_name='scope_kind')" ;;
    0003_tier3_phase2_retrieved_refs.sql) echo "select exists(select 1 from information_schema.columns where table_name='episodes' and column_name='retrieved_refs')" ;;
    0004_hardening_phase1_app_role.sql)   echo "select exists(select 1 from pg_roles where rolname='hupi_app')" ;;
    0005_hardening_phase3_rls.sql)        echo "select coalesce((select relrowsecurity from pg_class where relname='episodes'), false)" ;;
    0006_hardening_phase4_scope_keys.sql) echo "select (to_regclass('public.scope_keys') is not null)" ;;
    0007_audit_log.sql)                   echo "select (to_regclass('public.audit_log') is not null)" ;;
    0008_admin_operators.sql)             echo "select (to_regclass('public.operators') is not null)" ;;
    0009_export_import_audit_events.sql)  echo "select (select pg_get_constraintdef(oid) from pg_constraint where conname = 'audit_log_event_type_check') like '%export%'" ;;
    0010_key_rotation.sql)                echo "select (to_regclass('public.key_rotations') is not null)" ;;
    0011_key_rotation_audit_event.sql)    echo "select (select pg_get_constraintdef(oid) from pg_constraint where conname = 'audit_log_event_type_check') like '%key_rotation%'" ;;
    0012_entity_embeddings.sql)           echo "select exists(select 1 from information_schema.columns where table_name='entities' and column_name='embedding')" ;;
    0013_embedding_model_tracking.sql)     echo "select exists(select 1 from information_schema.columns where table_name='summaries' and column_name='embedding_model')" ;;
    0014_demo_sessions.sql)               echo "select (to_regclass('public.demo_sessions') is not null)" ;;
    *) echo "no idempotency probe defined for $1" >&2; exit 1 ;;
  esac
}

for f in 0001_init.sql 0002_tier3_phase1_identity.sql 0003_tier3_phase2_retrieved_refs.sql \
         0004_hardening_phase1_app_role.sql 0005_hardening_phase3_rls.sql 0006_hardening_phase4_scope_keys.sql \
         0007_audit_log.sql 0008_admin_operators.sql 0009_export_import_audit_events.sql \
         0010_key_rotation.sql 0011_key_rotation_audit_event.sql 0012_entity_embeddings.sql 0013_embedding_model_tracking.sql \
         0014_demo_sessions.sql; do
  already="$(psql "$HUPI_ADMIN_DATABASE_URL" -tAc "$(probe "$f")" 2>/dev/null | tr -d '[:space:]')"
  if [ "$already" = "t" ]; then
    echo "schema/$f already applied, skipping"
    continue
  fi
  echo "applying schema/$f"
  psql "$HUPI_ADMIN_DATABASE_URL" -v ON_ERROR_STOP=1 -f "$SCRIPT_DIR/$f"
done

echo "migrations up to date"
