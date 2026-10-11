#!/usr/bin/env bash
# Stops what start.sh started.
WORK="${HUPI_E2E_WORK:-$HOME/.cache/hupi-win-e2e}"
for p in gw model; do
  if [ -f "$WORK/run/$p.pid" ]; then kill "$(cat "$WORK/run/$p.pid")" 2>/dev/null || true; rm -f "$WORK/run/$p.pid"; fi
done
