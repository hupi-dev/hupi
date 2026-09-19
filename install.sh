#!/usr/bin/env bash
# One-command installer for HUPI — see docs/INSTALL.md for the manual,
# step-by-step version of everything this script automates. This exists
# because that manual process is ~six steps with real decision points
# (which tier, do you already have Postgres, do you already have a
# provider config) — this script makes those decisions explicit up front
# (flags, or interactive prompts when a flag is omitted) and then does
# the same steps INSTALL.md documents, in the same order, so the two
# stay in sync by construction rather than by discipline.
#
# Usage:
#   ./install.sh                     # fully interactive
#   ./install.sh --tier=1 --yes      # non-interactive, sane defaults, fails
#                                     # loudly on anything with no safe default
#                                     # (a real secret, e.g. an LLM API key)
#   ./install.sh --tier=3 --yes      # Tier 3 needs the separately-licensed
#                                     # hupi-t3 extension already overlaid
#                                     # onto this checkout first — see
#                                     # https://github.com/samuel-sujith/hupi-t3
#   ./install.sh --help              # full flag reference
#
# Safe to re-run: every step checks what's already true (schema already
# applied? providers.yaml already exists? container already running?)
# before acting, rather than assuming a clean slate — see apply_migration
# and the file-exists checks in write_providers_yaml/write_env_file.
set -euo pipefail

# ============================================================
# Output helpers
# ============================================================
if [[ -t 1 ]]; then
  C_BOLD=$'\033[1m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_RESET=$'\033[0m'
else
  C_BOLD=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_RESET=''
fi
log()  { printf '%s==>%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
note() { printf '%s    %s%s\n' "$C_BOLD" "$*" "$C_RESET"; }
warn() { printf '%s!!%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()  { printf '%sERROR:%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

# ============================================================
# Defaults — every one of these is overridable by a flag; anything left
# unset by both a flag and its default is filled in interactively unless
# --yes is given, in which case an unresolvable required value is a hard
# error (see require_or_die).
# ============================================================
TIER=""
ASSUME_YES=0
INSTALL_DIR=""
ENV_FILE="hupi.env"
DO_START=0
SKIP_TIER3_BOOTSTRAP=0

PG_MODE=""                 # docker | existing
PG_DOCKER_NAME="hupi-pg"
PG_DOCKER_PORT="5432"
PG_EXISTING_ADMIN_URL=""   # superuser/owner DSN, used only for migrations + ALTER ROLE
PG_HOST="localhost"
PG_PORT="5432"
PG_DBNAME="hupi"
PG_SSLMODE="disable"
PG_APP_PASSWORD=""

PROVIDER_PRESET=""         # openai | anthropic | custom | existing-file
PROVIDER_EXISTING_FILE=""
PROVIDER_BASE_URL=""
PROVIDER_VENDOR=""
PROVIDER_KIND=""
PROVIDER_API_KEY_ENV=""
PROVIDER_API_KEY=""
PROVIDER_MODEL=""
EMBED_BASE_URL=""
EMBED_VENDOR=""
EMBED_API_KEY_ENV=""
EMBED_API_KEY=""
EMBED_MODEL=""

KEK=""
ADMIN_UI=""                # 0 | 1, defaulted from TIER once known
ADMIN_UI_OPERATOR_NAME="admin"
ADMIN_UI_OPERATOR_TOKEN="" # set by setup_admin_ui_operator if it creates one this run
LISTEN_ADDR="127.0.0.1:8787"
ADMIN_UI_LISTEN_ADDR="127.0.0.1:8788"

usage() {
  cat <<'EOF'
Usage: ./install.sh [flags]

Tier:
  --tier=1|2|3              1=Personal, 2=Professional Single (same install
                             as 1), 3=Professional Shared (adds auth +
                             team provisioning). Prompted if omitted.
                             Tier 3 requires the separately-licensed hupi-t3
                             extension already overlaid onto this checkout
                             (see https://github.com/samuel-sujith/hupi-t3)
                             — this installer refuses --tier=3 without it,
                             rather than generating an env file for a
                             gateway that would then fail to start.
  --yes, -y                 Non-interactive: use defaults, fail instead of
                             prompting when a value has no safe default
                             (e.g. an LLM API key).

Postgres — pick one:
  --pg-mode=docker           Spin up pgvector/pgvector:pg16 in Docker (default
                             if you don't already have Postgres).
  --pg-docker-name=NAME      Container name (default: hupi-pg).
  --pg-docker-port=PORT      Host port to bind (default: 5432).
  --pg-mode=existing         Use Postgres you already run.
  --pg-existing-admin-url=DSN  Superuser/owner DSN, used only to apply
                             migrations and set the app role's password —
                             never stored. Required with --pg-mode=existing.
  --pg-host / --pg-port / --pg-dbname / --pg-sslmode
                             Connection details the app itself will use
                             (default: localhost/5432/hupi/disable).
  --pg-app-password=PW       Password to set for the non-owner hupi_app
                             role. Generated if omitted.

LLM provider — pick one:
  --provider-preset=openai      kind=openai_compat, one profile for chat,
                                 consolidation, grounding, and embeddings.
  --provider-preset=anthropic   kind=anthropic for chat/consolidation/
                                 grounding; Anthropic has no embeddings API,
                                 so you'll also configure an OpenAI-compatible
                                 embedding profile (asked separately).
  --provider-preset=custom      Ask every field (also needs
                                 --provider-kind=openai_compat|anthropic).
  --provider-preset=existing-file --provider-existing-file=PATH
                                 Copy an already-written providers.yaml
                                 verbatim; skips the wizard entirely.
  --provider-base-url / --provider-vendor / --provider-api-key-env
  --provider-api-key / --provider-model
                                 Override individual fields of the chosen
                                 preset non-interactively.
  --embedding-base-url / --embedding-vendor / --embedding-api-key-env
  --embedding-api-key / --embedding-model
                                 Same, for the separate embedding profile
                                 the anthropic preset (always) and the
                                 custom preset (if you decline to reuse
                                 one profile for everything) need — set
                                 all five to run --provider-preset=anthropic
                                 under --yes without a prompt.

Encryption:
  --kek=BASE64               Reuse an existing HUPI_KEK (upgrading an
                             existing deployment). Generated if omitted.

Tier 3 only:
  --admin-ui / --no-admin-ui   Also build+configure the browser admin UI
                                (default: on for tier 3, off otherwise).
  --admin-ui-operator-name=NAME  Name of the first admin-UI operator
                                 credential to provision (default: admin).
                                 Its token is generated server-side and
                                 printed once at the end — there's no flag
                                 to set it, same as an API key.
  --skip-tier3-bootstrap        Don't interactively create a first
                                 user/team/key after install.

Other:
  --listen-addr=HOST:PORT           Gateway bind address (default 127.0.0.1:8787).
  --admin-ui-listen-addr=HOST:PORT  Admin UI bind address (default 127.0.0.1:8788).
  --env-file=PATH             Where to write the generated env file
                               (default: ./hupi.env).
  --start                     Start the gateway (and admin UI, if enabled)
                               in the background once install finishes.
  --install-dir=PATH          Repo checkout to install from (default: the
                               directory this script lives in).
  -h, --help                  This message.
EOF
}

# ============================================================
# Argument parsing — long options only, "--flag=value" or bare "--flag"
# for booleans.
# ============================================================
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier=*) TIER="${1#*=}" ;;
    --yes|-y) ASSUME_YES=1 ;;
    --install-dir=*) INSTALL_DIR="${1#*=}" ;;
    --env-file=*) ENV_FILE="${1#*=}" ;;
    --start) DO_START=1 ;;
    --no-start) DO_START=0 ;;
    --skip-tier3-bootstrap) SKIP_TIER3_BOOTSTRAP=1 ;;

    --pg-mode=*) PG_MODE="${1#*=}" ;;
    --pg-docker-name=*) PG_DOCKER_NAME="${1#*=}" ;;
    --pg-docker-port=*) PG_DOCKER_PORT="${1#*=}" ;;
    --pg-existing-admin-url=*) PG_EXISTING_ADMIN_URL="${1#*=}" ;;
    --pg-host=*) PG_HOST="${1#*=}" ;;
    --pg-port=*) PG_PORT="${1#*=}" ;;
    --pg-dbname=*) PG_DBNAME="${1#*=}" ;;
    --pg-sslmode=*) PG_SSLMODE="${1#*=}" ;;
    --pg-app-password=*) PG_APP_PASSWORD="${1#*=}" ;;

    --provider-preset=*) PROVIDER_PRESET="${1#*=}" ;;
    --provider-existing-file=*) PROVIDER_EXISTING_FILE="${1#*=}" ;;
    --provider-base-url=*) PROVIDER_BASE_URL="${1#*=}" ;;
    --provider-vendor=*) PROVIDER_VENDOR="${1#*=}" ;;
    --provider-kind=*) PROVIDER_KIND="${1#*=}" ;;
    --provider-api-key-env=*) PROVIDER_API_KEY_ENV="${1#*=}" ;;
    --provider-api-key=*) PROVIDER_API_KEY="${1#*=}" ;;
    --provider-model=*) PROVIDER_MODEL="${1#*=}" ;;
    --embedding-base-url=*) EMBED_BASE_URL="${1#*=}" ;;
    --embedding-vendor=*) EMBED_VENDOR="${1#*=}" ;;
    --embedding-api-key-env=*) EMBED_API_KEY_ENV="${1#*=}" ;;
    --embedding-api-key=*) EMBED_API_KEY="${1#*=}" ;;
    --embedding-model=*) EMBED_MODEL="${1#*=}" ;;

    --kek=*) KEK="${1#*=}" ;;

    --admin-ui) ADMIN_UI=1 ;;
    --no-admin-ui) ADMIN_UI=0 ;;
    --admin-ui-operator-name=*) ADMIN_UI_OPERATOR_NAME="${1#*=}" ;;
    --listen-addr=*) LISTEN_ADDR="${1#*=}" ;;
    --admin-ui-listen-addr=*) ADMIN_UI_LISTEN_ADDR="${1#*=}" ;;

    -h|--help) usage; exit 0 ;;
    *) die "unknown flag: $1 (see --help)" ;;
  esac
  shift
done

# ============================================================
# Prompt helpers. In --yes mode, ask() returns its default (or dies if
# none) instead of blocking — a "1-click" run must never hang on stdin.
# ============================================================
ask() {
  local prompt="$1" default="${2:-}" reply=""
  if [[ "$ASSUME_YES" -eq 1 ]]; then
    [[ -n "$default" ]] || die "--yes given but no default for: $prompt (pass the matching flag explicitly)"
    printf '%s\n' "$default"
    return
  fi
  if [[ -n "$default" ]]; then
    read -r -p "$prompt [$default]: " reply || true
    printf '%s\n' "${reply:-$default}"
  else
    read -r -p "$prompt: " reply || true
    printf '%s\n' "$reply"
  fi
}

ask_secret() {
  local prompt="$1" reply=""
  if [[ "$ASSUME_YES" -eq 1 ]]; then
    die "--yes given but no value for a required secret: $prompt (pass the matching flag explicitly)"
  fi
  read -r -s -p "$prompt: " reply || true
  echo >&2
  printf '%s\n' "$reply"
}

confirm() {
  local prompt="$1" default="${2:-y}" reply=""
  if [[ "$ASSUME_YES" -eq 1 ]]; then
    [[ "$default" == "y" ]] && return 0 || return 1
  fi
  read -r -p "$prompt [$([[ "$default" == "y" ]] && echo Y/n || echo y/N)]: " reply || true
  reply="${reply:-$default}"
  [[ "$reply" =~ ^[Yy] ]]
}

randbase64() { head -c "$1" /dev/urandom | base64; }
# Passwords/tokens that end up embedded in a postgres:// URL or typed by
# hand (Basic Auth) — base64's '+' and '/' broke DSN parsing here during
# testing (pgx read a '/' in the password as a path separator), so these
# stay in a plain alnum alphabet instead of needing URL-encoding logic.
randalnum() { head -c $(( "$1" * 4 )) /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }

# ============================================================
# Locate the repo and sanity-check it's actually HUPI's source tree —
# this script `go build`s from source (docs/INSTALL.md: no Dockerfile
# exists yet), so it has to run from inside a real checkout.
# ============================================================
resolve_repo_root() {
  if [[ -n "$INSTALL_DIR" ]]; then
    REPO_ROOT="$INSTALL_DIR"
  else
    REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  [[ -f "$REPO_ROOT/go.mod" ]] || die "no go.mod in $REPO_ROOT — pass --install-dir=/path/to/hupi checkout"
  grep -q '^module hupi$' "$REPO_ROOT/go.mod" || die "$REPO_ROOT/go.mod is not HUPI's module (expected 'module hupi')"
  cd "$REPO_ROOT"
  log "installing from $REPO_ROOT"
}

# ============================================================
# Prerequisites. Deliberately does NOT auto-install Go or Docker itself —
# those are system-wide toolchain changes, out of scope for what a
# provisioning script should silently do; this checks and tells you
# exactly what's missing instead.
# ============================================================
check_go() {
  command -v go >/dev/null 2>&1 || die "Go is not installed. Install Go 1.25+ from https://go.dev/dl/ and re-run."
  local ver major minor
  ver="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  major="${ver%%.*}"
  minor="$(printf '%s' "$ver" | cut -d. -f2)"
  if [[ -z "$major" ]] || (( major < 1 )) || { (( major == 1 )) && (( minor < 25 )); }; then
    warn "found Go $ver — HUPI was built and tested against Go 1.25+. Continuing anyway, but if the build fails, that's likely why."
  fi
}

check_docker() {
  command -v docker >/dev/null 2>&1 || die "Docker is not installed, but --pg-mode=docker needs it. Install Docker or use --pg-mode=existing."
  docker info >/dev/null 2>&1 || die "Docker is installed but the daemon isn't reachable (permissions? not running?). Start it, or use --pg-mode=existing."
}

check_psql() {
  command -v psql >/dev/null 2>&1 || die "psql is not installed, but --pg-mode=existing needs it to apply migrations. Install the postgresql-client package, or use --pg-mode=docker."
}

# Re-running this script must not regenerate secrets (HUPI_KEK, hupi_app's
# password) that were already generated on a previous run — that would
# silently lock out anything encrypted or authenticated under the old
# ones. Sourcing an existing env file first means setup_kek and
# set_app_role_password see the real prior values as defaults, the same
# way a flag would override them.
load_existing_env() {
  if [[ -f "$ENV_FILE" ]]; then
    note "found existing $ENV_FILE — reusing its values as defaults except where a flag overrides them"
    # shellcheck disable=SC1090
    source "$ENV_FILE"
  fi
}

# ============================================================
# Tier selection
# ============================================================
resolve_tier() {
  if [[ -z "$TIER" ]]; then
    if [[ "$ASSUME_YES" -eq 1 ]]; then
      TIER=1
    else
      echo
      note "Which tier?"
      note "  1) Personal              — single user, no auth"
      note "  2) Professional Single   — same install as 1; IT-managed, not self-managed"
      note "  3) Professional Shared   — teams, API-key auth, admin provisioning"
      note "     (requires the separately-licensed hupi-t3 extension already"
      note "     overlaid onto this checkout — see https://github.com/samuel-sujith/hupi-t3)"
      TIER="$(ask "Tier (1/2/3)" "1")"
    fi
  fi
  case "$TIER" in
    1|2|3) ;;
    *) die "--tier must be 1, 2, or 3, got: $TIER" ;;
  esac
  if [[ "$TIER" == "3" ]]; then
    # internal/auth/team.go only exists in a checkout that has the
    # separately-licensed hupi-t3 extension overlaid onto it (see
    # ARCHITECTURE.md § "Licensing and the open-core split") — a plain
    # clone of this repo alone can never satisfy Tier 3, since
    # HUPI_REQUIRE_AUTH=true refuses to start without it (a deliberate,
    # loud failure, not silently falling back to Tier 1/2 behavior).
    # Catching that here, before generating an env file that would only
    # produce a gateway that won't start, is worth the extra check.
    [[ -f "$REPO_ROOT/internal/auth/team.go" ]] || die "Tier 3 requires the separately-licensed hupi-t3 extension, not present in this checkout — see https://github.com/samuel-sujith/hupi-t3 (its build.sh overlays the extension's files onto a checkout like this one before building). Run --tier=1 or --tier=2 if you don't have a Tier 3 license."
  fi
  if [[ -z "$ADMIN_UI" ]]; then
    [[ "$TIER" == "3" ]] && ADMIN_UI=1 || ADMIN_UI=0
  fi
  log "tier $TIER selected$( [[ "$TIER" != "3" ]] && echo ' (identical install to tier 1 — tier 2 differs in who operates it, not in code, see docs/INSTALL.md)')"
}

# ============================================================
# Postgres
# ============================================================
resolve_pg_mode() {
  if [[ -z "$PG_MODE" ]]; then
    if confirm "Do you already have a Postgres instance (with pgvector) to use?" "n"; then
      PG_MODE="existing"
    else
      PG_MODE="docker"
    fi
  fi
  [[ "$PG_MODE" == "docker" || "$PG_MODE" == "existing" ]] || die "--pg-mode must be 'docker' or 'existing', got: $PG_MODE"
}

setup_postgres_docker() {
  check_docker
  PG_PORT="$PG_DOCKER_PORT"
  local pg_superuser_password
  pg_superuser_password="$(randalnum 18)"

  local state
  state="$(docker inspect -f '{{.State.Status}}' "$PG_DOCKER_NAME" 2>/dev/null || true)"
  if [[ -z "$state" ]]; then
    log "starting new Postgres container '$PG_DOCKER_NAME' (pgvector/pgvector:pg16) on port $PG_PORT"
    docker run -d --name "$PG_DOCKER_NAME" \
      -e POSTGRES_PASSWORD="$pg_superuser_password" \
      -e POSTGRES_DB="$PG_DBNAME" \
      -p "${PG_PORT}:5432" \
      pgvector/pgvector:pg16 >/dev/null
  elif [[ "$state" != "running" ]]; then
    log "found stopped container '$PG_DOCKER_NAME', starting it"
    docker start "$PG_DOCKER_NAME" >/dev/null
    pg_superuser_password="$(docker exec "$PG_DOCKER_NAME" printenv POSTGRES_PASSWORD)"
  else
    log "reusing already-running container '$PG_DOCKER_NAME'"
    pg_superuser_password="$(docker exec "$PG_DOCKER_NAME" printenv POSTGRES_PASSWORD)"
  fi

  log "waiting for Postgres to accept connections"
  # Not just pg_isready: the official postgres image starts a throwaway
  # server for initdb, which pg_isready happily reports as ready, then
  # restarts for real a moment later — a bare readiness ping here would
  # race that restart. Polling the actual target database waits out both
  # phases.
  local i
  for i in $(seq 1 30); do
    if docker exec "$PG_DOCKER_NAME" psql -U postgres -d "$PG_DBNAME" -c 'select 1' >/dev/null 2>&1; then
      break
    fi
    [[ "$i" -eq 30 ]] && die "Postgres in '$PG_DOCKER_NAME' did not become ready in time — check 'docker logs $PG_DOCKER_NAME'"
    sleep 1
  done

  PSQL_ADMIN=(docker exec -i "$PG_DOCKER_NAME" psql -v ON_ERROR_STOP=1 -U postgres -d "$PG_DBNAME")
  PG_HOST="localhost"
}

setup_postgres_existing() {
  check_psql
  if [[ -z "$PG_EXISTING_ADMIN_URL" ]]; then
    PG_EXISTING_ADMIN_URL="$(ask "Admin/superuser Postgres DSN (used only to apply migrations, never stored)")"
  fi
  [[ -n "$PG_EXISTING_ADMIN_URL" ]] || die "--pg-existing-admin-url is required with --pg-mode=existing"
  PG_HOST="$(ask "App-facing Postgres host" "$PG_HOST")"
  PG_PORT="$(ask "App-facing Postgres port" "$PG_PORT")"
  PG_DBNAME="$(ask "Database name" "$PG_DBNAME")"
  PG_SSLMODE="$(ask "sslmode" "$PG_SSLMODE")"

  log "checking connectivity to the admin DSN"
  psql "$PG_EXISTING_ADMIN_URL" -v ON_ERROR_STOP=1 -c 'select 1' >/dev/null </dev/null \
    || die "could not connect using --pg-existing-admin-url"

  PSQL_ADMIN=(psql -v ON_ERROR_STOP=1 "$PG_EXISTING_ADMIN_URL")
}

# migration file -> a query that prints 't' iff that migration's effect
# is already present. Re-running raw `create table`/`alter table` isn't
# idempotent on its own (schema/*.sql has no IF NOT EXISTS guards), so
# this is what makes re-running install.sh safe.
migration_probe() {
  case "$1" in
    0001_init.sql)                     echo "select (to_regclass('public.episodes') is not null)" ;;
    0002_tier3_phase1_identity.sql)    echo "select exists(select 1 from information_schema.columns where table_name='episodes' and column_name='scope_kind')" ;;
    0003_tier3_phase2_retrieved_refs.sql) echo "select exists(select 1 from information_schema.columns where table_name='episodes' and column_name='retrieved_refs')" ;;
    0004_hardening_phase1_app_role.sql) echo "select exists(select 1 from pg_roles where rolname='hupi_app')" ;;
    0005_hardening_phase3_rls.sql)     echo "select coalesce((select relrowsecurity from pg_class where relname='episodes'), false)" ;;
    0006_hardening_phase4_scope_keys.sql) echo "select (to_regclass('public.scope_keys') is not null)" ;;
    0007_audit_log.sql)                echo "select (to_regclass('public.audit_log') is not null)" ;;
    0008_admin_operators.sql)          echo "select (to_regclass('public.operators') is not null)" ;;
    0009_export_import_audit_events.sql) echo "select (select pg_get_constraintdef(oid) from pg_constraint where conname = 'audit_log_event_type_check') like '%export%'" ;;
    0010_key_rotation.sql)             echo "select (to_regclass('public.key_rotations') is not null)" ;;
    0011_key_rotation_audit_event.sql) echo "select (select pg_get_constraintdef(oid) from pg_constraint where conname = 'audit_log_event_type_check') like '%key_rotation%'" ;;
    0012_entity_embeddings.sql)        echo "select exists(select 1 from information_schema.columns where table_name='entities' and column_name='embedding')" ;;
    0013_embedding_model_tracking.sql)  echo "select exists(select 1 from information_schema.columns where table_name='summaries' and column_name='embedding_model')" ;;
    0014_demo_sessions.sql)            echo "select (to_regclass('public.demo_sessions') is not null)" ;;
    *) die "no idempotency probe defined for migration $1 (add one to migration_probe)" ;;
  esac
}

apply_migration() {
  local file="$1" probe already
  probe="$(migration_probe "$file")"
  already="$("${PSQL_ADMIN[@]}" -tAc "$probe" </dev/null 2>/dev/null | tr -d '[:space:]')"
  if [[ "$already" == "t" ]]; then
    note "schema/$file already applied, skipping"
    return
  fi
  log "applying schema/$file"
  "${PSQL_ADMIN[@]}" < "schema/$file"
}

apply_all_migrations() {
  local f
  for f in 0001_init.sql 0002_tier3_phase1_identity.sql 0003_tier3_phase2_retrieved_refs.sql \
           0004_hardening_phase1_app_role.sql 0005_hardening_phase3_rls.sql 0006_hardening_phase4_scope_keys.sql \
           0007_audit_log.sql 0008_admin_operators.sql 0009_export_import_audit_events.sql \
           0010_key_rotation.sql 0011_key_rotation_audit_event.sql 0012_entity_embeddings.sql 0013_embedding_model_tracking.sql \
           0014_demo_sessions.sql; do
    apply_migration "$f"
  done
}

set_app_role_password() {
  # A re-run must not silently rotate a credential something else may
  # already be using. If hupi_app already has a password and none was
  # given explicitly, try to recover the one already on disk (nothing
  # server-side can be read back — pg_authid only stores a hash) before
  # ever considering a rotation.
  local has_pw
  has_pw="$("${PSQL_ADMIN[@]}" -tAc "select (rolpassword is not null) from pg_authid where rolname='hupi_app'" </dev/null 2>/dev/null | tr -d '[:space:]')"

  if [[ "$has_pw" == "t" && -z "$PG_APP_PASSWORD" ]]; then
    local existing
    existing="$(printf '%s' "${HUPI_APP_DATABASE_URL:-}" | sed -n 's#postgres://hupi_app:\([^@]*\)@.*#\1#p')"
    if [[ -n "$existing" ]]; then
      note "hupi_app already has a password set — reusing the one loaded from $ENV_FILE (pass --pg-app-password to rotate it)"
      PG_APP_PASSWORD="$existing"
      HUPI_APP_DATABASE_URL="postgres://hupi_app:${PG_APP_PASSWORD}@${PG_HOST}:${PG_PORT}/${PG_DBNAME}?sslmode=${PG_SSLMODE}"
      return
    fi
    if [[ "$ASSUME_YES" -eq 1 ]]; then
      die "hupi_app already has a password set, no --pg-app-password was given, and it can't be recovered from $ENV_FILE (not found) — refusing to silently rotate a credential something else may depend on. Re-run with --pg-app-password=<the existing password> if you know it."
    fi
    warn "hupi_app already has a password set, but $ENV_FILE (where it would normally be recorded) isn't here to recover it from."
    confirm "Rotate hupi_app's password now? (breaks anything still using the old one)" "n" \
      || die "aborted — re-run with --pg-app-password=<existing password> if you know it"
  fi

  [[ -n "$PG_APP_PASSWORD" ]] || PG_APP_PASSWORD="$(randalnum 18)"
  log "setting hupi_app's password"
  "${PSQL_ADMIN[@]}" -c "ALTER ROLE hupi_app WITH PASSWORD '${PG_APP_PASSWORD//\'/\'\'}';" >/dev/null </dev/null
  HUPI_APP_DATABASE_URL="postgres://hupi_app:${PG_APP_PASSWORD}@${PG_HOST}:${PG_PORT}/${PG_DBNAME}?sslmode=${PG_SSLMODE}"
}

# ============================================================
# Binaries
# ============================================================

# build_admin_ui_frontend runs the npm build that produces
# cmd/hupi-admin-ui/web/dist, which cmd/hupi-admin-ui/assets.go embeds via
# //go:embed at Go compile time. A committed placeholder dist/index.html
# keeps a plain `go build` from failing on a fresh clone, but this script
# always wants the real React app, so it runs the npm build first whenever
# the admin UI is being built at all.
build_admin_ui_frontend() {
  command -v npm >/dev/null 2>&1 || die "npm is not installed, but the admin UI frontend (cmd/hupi-admin-ui/web) needs it to build. Install Node.js/npm and re-run, or use --no-admin-ui."
  log "building hupi-admin-ui's React frontend (cmd/hupi-admin-ui/web)"
  ( cd cmd/hupi-admin-ui/web && npm ci && npm run build )
}

build_binaries() {
  log "building binaries into ./bin"
  mkdir -p bin
  local targets=(hupi hupi-consolidate hupi-selfcheck hupi-trace hupi-correct hupi-audit hupi-export hupi-import hupi-rotate-key)
  # hupi-admin is needed by ADMIN_UI too, not just tier 3 directly —
  # setup_admin_ui_operator provisions the first operator credential
  # through it (docs/GAP_CLOSURE_PLAN.md §4.3).
  { [[ "$TIER" == "3" ]] || [[ "$ADMIN_UI" == "1" ]]; } && targets+=(hupi-admin)
  [[ "$ADMIN_UI" == "1" ]] && targets+=(hupi-admin-ui)
  [[ "$ADMIN_UI" == "1" ]] && build_admin_ui_frontend
  local t
  for t in "${targets[@]}"; do
    go build -o "bin/$t" "./cmd/$t"
  done
}

# ============================================================
# Provider config
# ============================================================
resolve_provider_preset() {
  if [[ -n "$PROVIDER_EXISTING_FILE" && -z "$PROVIDER_PRESET" ]]; then
    PROVIDER_PRESET="existing-file"
  fi
  if [[ -z "$PROVIDER_PRESET" ]]; then
    if [[ -f providers.yaml ]] && confirm "providers.yaml already exists here — keep it as-is?" "y"; then
      PROVIDER_PRESET="existing-file"
      PROVIDER_EXISTING_FILE="providers.yaml"
    elif [[ "$ASSUME_YES" -eq 1 ]]; then
      PROVIDER_PRESET="openai"
    else
      echo
      note "LLM provider setup:"
      note "  openai)   one OpenAI profile, reused for chat/consolidation/grounding/embeddings"
      note "  anthropic) Claude for chat/consolidation/grounding, + a separate OpenAI embedding profile"
      note "  custom)   ask every field"
      note "  existing-file) point at a providers.yaml you already wrote"
      PROVIDER_PRESET="$(ask "Preset (openai/anthropic/custom/existing-file)" "openai")"
    fi
  fi
}

setup_provider_openai_like() {
  # Shared by the openai preset (used for every role) and the anthropic
  # preset's separate embedding profile (Anthropic has no embeddings
  # endpoint, so something OpenAI-compatible always has to fill that
  # role — see providers.yaml.example's local-ollama entry for the same
  # pattern with a local model instead).
  local prefix="$1" default_base="$2" default_key_env="$3" default_model="$4"
  local base key_env key model
  base="$(ask "  API base URL" "$default_base")"
  key_env="$(ask "  Env var name to hold the API key" "$default_key_env")"
  if [[ -n "${!key_env:-}" ]]; then
    note "  using existing \$$key_env from your environment"
    key="${!key_env}"
  else
    key="$(ask_secret "  API key for $key_env")"
  fi
  model="$(ask "  Model" "$default_model")"
  printf -v "${prefix}_BASE_URL" '%s' "$base"
  printf -v "${prefix}_API_KEY_ENV" '%s' "$key_env"
  printf -v "${prefix}_API_KEY" '%s' "$key"
  printf -v "${prefix}_MODEL" '%s' "$model"
}

setup_provider() {
  resolve_provider_preset
  case "$PROVIDER_PRESET" in
    existing-file)
      [[ -n "$PROVIDER_EXISTING_FILE" ]] || PROVIDER_EXISTING_FILE="$(ask "Path to your providers.yaml")"
      [[ -f "$PROVIDER_EXISTING_FILE" ]] || die "no such file: $PROVIDER_EXISTING_FILE"
      if [[ "$(cd "$(dirname "$PROVIDER_EXISTING_FILE")" && pwd)/$(basename "$PROVIDER_EXISTING_FILE")" != "$REPO_ROOT/providers.yaml" ]]; then
        cp "$PROVIDER_EXISTING_FILE" providers.yaml
      fi
      log "using existing provider config at providers.yaml — not touching it further"
      ;;
    openai)
      PROVIDER_VENDOR=openai; PROVIDER_KIND=openai_compat
      note "OpenAI profile (used for chat, consolidation, grounding, and embeddings):"
      [[ -n "$PROVIDER_BASE_URL" ]] || setup_provider_openai_like PROVIDER "https://api.openai.com/v1" "OPENAI_API_KEY" "gpt-4.1"
      EMBED_MODEL="$(ask "  Embedding model (same OpenAI profile)" "${EMBED_MODEL:-text-embedding-3-small}")"
      write_providers_yaml_single_profile
      ;;
    anthropic)
      PROVIDER_VENDOR=anthropic; PROVIDER_KIND=anthropic
      note "Anthropic profile (chat, consolidation, grounding):"
      [[ -n "$PROVIDER_BASE_URL" ]] || setup_provider_openai_like PROVIDER "https://api.anthropic.com/v1" "ANTHROPIC_API_KEY" "claude-sonnet-5"
      note "Anthropic has no embeddings API — a separate OpenAI-compatible profile is required for that role:"
      [[ -n "$EMBED_BASE_URL" ]] || setup_provider_openai_like EMBED "https://api.openai.com/v1" "OPENAI_API_KEY" "text-embedding-3-small"
      [[ -n "$EMBED_VENDOR" ]] || EMBED_VENDOR=openai
      write_providers_yaml_dual_profile
      ;;
    custom)
      PROVIDER_KIND="$(ask "Kind (openai_compat/anthropic)" "${PROVIDER_KIND:-openai_compat}")"
      PROVIDER_VENDOR="$(ask "Vendor label (recorded on every episode, e.g. groq/ollama/openai)" "${PROVIDER_VENDOR:-custom}")"
      [[ -n "$PROVIDER_BASE_URL" ]] || setup_provider_openai_like PROVIDER "https://api.openai.com/v1" "PROVIDER_API_KEY" "gpt-4.1"
      if [[ -z "$EMBED_BASE_URL" ]] && confirm "Use this same profile for embeddings too?" "y"; then
        [[ -n "$EMBED_MODEL" ]] || EMBED_MODEL="$(ask "  Embedding model" "text-embedding-3-small")"
        write_providers_yaml_single_profile
      else
        note "Separate embedding profile (must be openai_compat — HUPI has no other embedding-capable kind):"
        [[ -n "$EMBED_BASE_URL" ]] || setup_provider_openai_like EMBED "https://api.openai.com/v1" "OPENAI_API_KEY" "text-embedding-3-small"
        [[ -n "$EMBED_VENDOR" ]] || EMBED_VENDOR="$PROVIDER_VENDOR-embed"
        write_providers_yaml_dual_profile
      fi
      ;;
    *) die "--provider-preset must be openai, anthropic, custom, or existing-file, got: $PROVIDER_PRESET" ;;
  esac
}

write_providers_yaml_single_profile() {
  cat > providers.yaml <<EOF
# Generated by install.sh. See ARCHITECTURE.md § Provider abstraction.
active_chat_provider: main
active_consolidation_provider: main
active_grounding_provider: main
active_embedding_provider: main

providers:
  main:
    kind: ${PROVIDER_KIND}
    vendor: ${PROVIDER_VENDOR}
    api_base: ${PROVIDER_BASE_URL}
    api_key_env: ${PROVIDER_API_KEY_ENV}
    model: ${PROVIDER_MODEL}
EOF
}

write_providers_yaml_dual_profile() {
  cat > providers.yaml <<EOF
# Generated by install.sh. See ARCHITECTURE.md § Provider abstraction.
active_chat_provider: main
active_consolidation_provider: main
active_grounding_provider: main
active_embedding_provider: embeddings

providers:
  main:
    kind: ${PROVIDER_KIND}
    vendor: ${PROVIDER_VENDOR}
    api_base: ${PROVIDER_BASE_URL}
    api_key_env: ${PROVIDER_API_KEY_ENV}
    model: ${PROVIDER_MODEL}
$( [[ "$PROVIDER_KIND" == "anthropic" ]] && printf '    api_version: "2023-06-01"\n' )

  embeddings:
    kind: openai_compat
    vendor: ${EMBED_VENDOR}
    api_base: ${EMBED_BASE_URL}
    api_key_env: ${EMBED_API_KEY_ENV}
    model: ${EMBED_MODEL}
EOF
}

# ============================================================
# Encryption key
# ============================================================
setup_kek() {
  if [[ -n "$KEK" ]]; then
    log "using provided HUPI_KEK"
  elif [[ -n "${HUPI_KEK:-}" ]]; then
    KEK="$HUPI_KEK"
    log "reusing HUPI_KEK loaded from $ENV_FILE"
  else
    KEK="$(randbase64 32)"
    log "generated a new HUPI_KEK"
  fi
}

# ============================================================
# Env file — every binary (hupi, hupi-consolidate, hupi-admin, ...)
# reads the same variables; this is the one file to source before
# running any of them by hand.
# ============================================================
write_env_file() {
  if [[ -f "$ENV_FILE" ]] && ! confirm "$ENV_FILE already exists — overwrite it?" "n"; then
    log "keeping existing $ENV_FILE untouched"
    return
  fi
  log "writing $ENV_FILE"
  {
    echo "# Generated by install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ). Contains secrets — keep this out of version control."
    echo "export HUPI_APP_DATABASE_URL='${HUPI_APP_DATABASE_URL}'"
    echo "export HUPI_KEK='${KEK}'"
    echo "export HUPI_PROVIDERS_CONFIG='${REPO_ROOT}/providers.yaml'"
    [[ "$LISTEN_ADDR" != "127.0.0.1:8787" ]] && echo "export HUPI_LISTEN_ADDR='${LISTEN_ADDR}'"
    if [[ -n "${PROVIDER_API_KEY_ENV:-}" && -n "${PROVIDER_API_KEY:-}" ]]; then
      echo "export ${PROVIDER_API_KEY_ENV}='${PROVIDER_API_KEY}'"
    fi
    if [[ -n "${EMBED_API_KEY_ENV:-}" && -n "${EMBED_API_KEY:-}" && "${EMBED_API_KEY_ENV}" != "${PROVIDER_API_KEY_ENV:-}" ]]; then
      echo "export ${EMBED_API_KEY_ENV}='${EMBED_API_KEY}'"
    fi
    if [[ "$TIER" == "3" ]]; then
      echo "export HUPI_REQUIRE_AUTH=true"
      [[ "$ADMIN_UI" == "1" && "$ADMIN_UI_LISTEN_ADDR" != "127.0.0.1:8788" ]] && echo "export HUPI_ADMIN_UI_LISTEN_ADDR='${ADMIN_UI_LISTEN_ADDR}'"
    fi
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
}

# ============================================================
# Tier 3: admin UI operator credential + first user/team/key
# ============================================================
# Unlike the old single shared HUPI_ADMIN_UI_TOKEN, this is not an env
# var the server reads — it's a named credential in the operators table
# (schema/0008_admin_operators.sql, docs/GAP_CLOSURE_PLAN.md §4.3), the
# same way an API key is a row in api_keys, not a setting. That also
# means it genuinely can't be recovered on a re-run — if
# ADMIN_UI_OPERATOR_NAME already exists, this leaves it alone rather than
# creating a confusing second credential under the same name.
setup_admin_ui_operator() {
  [[ "$ADMIN_UI" == "1" ]] || return 0
  local exists
  exists="$("${PSQL_ADMIN[@]}" -tAc "select exists(select 1 from operators where name = '${ADMIN_UI_OPERATOR_NAME//\'/\'\'}')" </dev/null 2>/dev/null | tr -d '[:space:]')"
  if [[ "$exists" == "t" ]]; then
    note "admin UI operator '$ADMIN_UI_OPERATOR_NAME' already exists — leaving it as-is (its token can't be recovered; run 'hupi-admin create-operator -name <other>' for an additional one)"
    return 0
  fi
  log "provisioning admin UI operator '$ADMIN_UI_OPERATOR_NAME'"
  local output
  output="$(HUPI_APP_DATABASE_URL="$HUPI_APP_DATABASE_URL" HUPI_KEK="$KEK" HUPI_PROVIDERS_CONFIG="$REPO_ROOT/providers.yaml" \
    ./bin/hupi-admin create-operator -name "$ADMIN_UI_OPERATOR_NAME" -actor install.sh)"
  ADMIN_UI_OPERATOR_TOKEN="$(printf '%s' "$output" | tail -1)"
}

bootstrap_tier3_identity() {
  [[ "$TIER" == "3" ]] || return 0
  [[ "$SKIP_TIER3_BOOTSTRAP" -eq 0 ]] || return 0
  confirm "Create a first user/team/API key now?" "y" || return 0

  local user team
  user="$(ask "First user id" "user:admin")"
  team="$(ask "First team id" "team:default")"

  HUPI_APP_DATABASE_URL="$HUPI_APP_DATABASE_URL" HUPI_KEK="$KEK" HUPI_PROVIDERS_CONFIG="$REPO_ROOT/providers.yaml" \
    ./bin/hupi-admin create-user -id "$user" -email ""
  HUPI_APP_DATABASE_URL="$HUPI_APP_DATABASE_URL" HUPI_KEK="$KEK" HUPI_PROVIDERS_CONFIG="$REPO_ROOT/providers.yaml" \
    ./bin/hupi-admin create-team -id "$team" -name "$team"
  HUPI_APP_DATABASE_URL="$HUPI_APP_DATABASE_URL" HUPI_KEK="$KEK" HUPI_PROVIDERS_CONFIG="$REPO_ROOT/providers.yaml" \
    ./bin/hupi-admin add-member -team "$team" -user "$user" -role admin
  log "issuing API key for $user — save this, it will not be shown again:"
  HUPI_APP_DATABASE_URL="$HUPI_APP_DATABASE_URL" HUPI_KEK="$KEK" HUPI_PROVIDERS_CONFIG="$REPO_ROOT/providers.yaml" \
    ./bin/hupi-admin create-key -user "$user"
}

# ============================================================
# Start
# ============================================================
maybe_start() {
  [[ "$DO_START" -eq 1 ]] || return 0
  log "starting hupi in the background (log: hupi.out, pid: hupi.pid)"
  ( set -a; source "$ENV_FILE"; set +a; nohup ./bin/hupi >hupi.out 2>&1 & echo $! > hupi.pid )
  if [[ "$ADMIN_UI" == "1" ]]; then
    log "starting hupi-admin-ui in the background (log: hupi-admin-ui.out, pid: hupi-admin-ui.pid)"
    ( set -a; source "$ENV_FILE"; set +a; nohup ./bin/hupi-admin-ui >hupi-admin-ui.out 2>&1 & echo $! > hupi-admin-ui.pid )
  fi
  sleep 1
  if kill -0 "$(cat hupi.pid)" 2>/dev/null; then
    note "gateway running, PID $(cat hupi.pid)"
  else
    warn "gateway process exited immediately — check hupi.out"
  fi
}

# ============================================================
# Summary
# ============================================================
print_summary() {
  echo
  log "install complete"
  note "env file:        $ENV_FILE  (source this before running any bin/hupi-* by hand)"
  note "provider config: providers.yaml"
  note "binaries:        ./bin/"
  echo
  if [[ "$DO_START" -eq 1 ]]; then
    note "hupi is running — try:"
    note "  curl http://${LISTEN_ADDR}/v1/chat/completions -H 'Content-Type: application/json' -d '{\"model\":\"gpt-4.1\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'"
  else
    note "start it with:"
    note "  source $ENV_FILE && ./bin/hupi"
  fi
  if [[ "$TIER" == "3" ]]; then
    note "provision more identities with: source $ENV_FILE && ./bin/hupi-admin -h"
    if [[ "$ADMIN_UI" == "1" ]]; then
      note "admin UI: source $ENV_FILE && ./bin/hupi-admin-ui   (then http://${ADMIN_UI_LISTEN_ADDR})"
      if [[ -n "$ADMIN_UI_OPERATOR_TOKEN" ]]; then
        note "log in as operator '$ADMIN_UI_OPERATOR_NAME' with this token — save it now, it will not be shown again:"
        note "  $ADMIN_UI_OPERATOR_TOKEN"
      else
        note "operator '$ADMIN_UI_OPERATOR_NAME' already existed — use its existing token, or: source $ENV_FILE && ./bin/hupi-admin create-operator -name <name>"
      fi
    fi
  fi
  note "audit trail: source $ENV_FILE && ./bin/hupi-audit tail"
  note "cron jobs (hupi-consolidate nightly, hupi-selfcheck weekly) are not set up by this script — see docs/INSTALL.md § Cron jobs."
  echo
  note "full manual reference: docs/INSTALL.md   |   admin UI details: docs/ADMIN_UI.md"
}

# ============================================================
main() {
  resolve_repo_root
  load_existing_env
  check_go
  resolve_tier

  resolve_pg_mode
  if [[ "$PG_MODE" == "docker" ]]; then setup_postgres_docker; else setup_postgres_existing; fi
  apply_all_migrations
  set_app_role_password

  build_binaries
  setup_provider
  setup_kek
  write_env_file
  setup_admin_ui_operator

  bootstrap_tier3_identity
  maybe_start
  print_summary
}

main "$@"
