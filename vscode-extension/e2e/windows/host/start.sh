#!/usr/bin/env bash
# Host side of the Windows e2e test: the scripted model (localhost only) and
# HUPI's gateway, built from this checkout, listening on Docker's default
# bridge so the VM can reach it. See ../README.md.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../../.." && pwd)"
WORK="${HUPI_E2E_WORK:-$HOME/.cache/hupi-win-e2e}"
ADMIN_DB="${HUPI_E2E_ADMIN_DB:-postgres://postgres:hupi@localhost:5432/hupi_win_e2e?sslmode=disable}"
APP_DB="${HUPI_E2E_APP_DB:-postgres://hupi_app:hupi_app_test_only@localhost:5432/hupi_win_e2e?sslmode=disable}"
LISTEN="${HUPI_E2E_LISTEN:-172.17.0.1:18790}"
MODEL_PORT=18802
mkdir -p "$WORK/run"

"$HERE/stop.sh" >/dev/null 2>&1 || true

# The test database: created on first use, migrated every time (idempotent).
db_name="${ADMIN_DB##*/}"; db_name="${db_name%%\?*}"
server_db="${ADMIN_DB%/*}/postgres?${ADMIN_DB#*\?}"
if [ "$(psql "$server_db" -Atc "select 1 from pg_database where datname='$db_name'")" != 1 ]; then
  psql "$server_db" -qc "create database $db_name"
fi
HUPI_ADMIN_DATABASE_URL="$ADMIN_DB" "$REPO/schema/migrate.sh" >/dev/null

(cd "$REPO" && go build -o "$WORK/hupi-gw" ./cmd/hupi)

nohup python3 "$HERE/scripted_model.py" "$MODEL_PORT" "$WORK/run/model.log" > "$WORK/run/model.out" 2>&1 &
echo $! > "$WORK/run/model.pid"
# A fixed all-zero key: this database only ever holds test turns.
HUPI_APP_DATABASE_URL="$APP_DB" HUPI_KEK="$(head -c32 /dev/zero | base64)" \
  HUPI_PROVIDERS_CONFIG="${HUPI_E2E_PROVIDERS:-$HERE/providers.yaml}" HUPI_LISTEN_ADDR="$LISTEN" \
  nohup "$WORK/hupi-gw" > "$WORK/run/gw.log" 2>&1 &
echo $! > "$WORK/run/gw.pid"
for _ in $(seq 1 30); do
  if curl -s -m1 "http://$LISTEN/healthz" >/dev/null; then echo "gateway up on $LISTEN (logs in $WORK/run)"; exit 0; fi
  sleep 1
done
echo "gateway did not start; see $WORK/run/gw.log" >&2
exit 1
