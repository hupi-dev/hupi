#!/bin/sh
# docker-compose's `migrate` service entrypoint. Runs the same idempotent
# schema/migrate.sh the Kubernetes migrate Job uses (see that file's own
# doc comment for why it's a separate script rather than shared with
# install.sh), then sets the hupi_app role's password.
#
# That second step exists because migration 0004 deliberately creates
# hupi_app with no password (see 0004's own comment: so no secret ever
# lands in a file meant to be checked into version control) — something
# has to set it out of band. Safe to re-run: migrate.sh is idempotent, and
# ALTER ROLE ... PASSWORD just resets it to the same value each time.
set -eu

: "${HUPI_ADMIN_DATABASE_URL:?HUPI_ADMIN_DATABASE_URL must be set}"
: "${HUPI_APP_DB_PASSWORD:?HUPI_APP_DB_PASSWORD must be set}"

/app/schema/migrate.sh

psql "$HUPI_ADMIN_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -c "alter role hupi_app with password '$HUPI_APP_DB_PASSWORD'"

echo "hupi_app password set"
