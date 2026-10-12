#!/usr/bin/env bash
# Runs the HUPI agent's Windows end-to-end suite in the dockur/windows VM.
# See ../README.md for the setup. Usage:
#   run-suite.sh [--hupi-code-run RUN_ID] [vscode] [hupicode]
# --hupi-code-run downloads that hupi-dev/hupi-code CI run's
# hupi-code-win32-x64 artifact first (otherwise the one already in the
# shared folder is used). With no app names it runs both apps, each in both
# modes (main, scenario2).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
E2E="$(cd "$HERE/.." && pwd)"
VM_DIR="${HUPI_WIN_DIR:-$HOME/hupi-win}"
SHARED="$VM_DIR/shared"
WORK="${HUPI_E2E_WORK:-$HOME/.cache/hupi-win-e2e}"
ADMIN_DB="${HUPI_E2E_ADMIN_DB:-postgres://postgres:hupi@localhost:5432/hupi_win_e2e?sslmode=disable}"
GH="${GH:-gh}"

run_id=""
apps=()
while [ $# -gt 0 ]; do
  case "$1" in
    --hupi-code-run) run_id="$2"; shift 2 ;;
    vscode|hupicode) apps+=("$1"); shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ ${#apps[@]} -gt 0 ] || apps=(vscode hupicode)

# The guest's files, and the helper extension that opens HUPI's sidebar.
mkdir -p "$SHARED/e2e"
cp "$E2E"/guest/* "$SHARED/e2e/"
# vsce comes from the extension's devDependencies (npm ci in vscode-extension/).
(cd "$HERE/opener" && "$E2E/../../node_modules/.bin/vsce" package --allow-missing-repository --skip-license --out "$SHARED/e2e/opener.vsix" >/dev/null)

if [ -n "$run_id" ]; then
  rm -rf "$SHARED/hupicode-artifact"
  "$GH" run download "$run_id" --repo hupi-dev/hupi-code --name hupi-code-win32-x64 --dir "$SHARED/hupicode-artifact"
fi
if printf '%s\n' "${apps[@]}" | grep -qx hupicode && [ ! -d "$SHARED/hupicode-artifact" ]; then
  echo "no HUPI Code build in $SHARED/hupicode-artifact; pass --hupi-code-run RUN_ID" >&2
  exit 2
fi

"$HERE/start.sh"
trap '"$HERE/stop.sh"' EXIT

episodes() { psql "$ADMIN_DB" -Atc "select count(*) from episodes"; }
latest_tags() { psql "$ADMIN_DB" -Atc "select coalesce(array_to_string(context_tags, ','), '') from episodes order by ts desc limit 1"; }

status=0
for app in "${apps[@]}"; do
  for mode in main scenario2; do
    out="$SHARED/e2e/out-$app-$mode"
    rm -rf "$out"
    before=$(episodes)
    : > "$WORK/run/model.log"
    echo "=== $app / $mode"
    python3 "$HERE/vmtype.py" "powershell -executionpolicy bypass -file z:\\e2e\\run.ps1 -app $app -mode $mode"
    for _ in $(seq 1 180); do [ -f "$out/DONE" ] && break; sleep 5; done
    if [ ! -f "$out/DONE" ]; then
      echo "FAIL timed out; screen saved to $WORK/timeout-$app-$mode.png"
      "$HERE/vmshot.sh" "$WORK/timeout-$app-$mode.png" || true
      status=1; continue
    fi
    python3 "$HERE/check.py" "$out" "$mode" "$before" "$(episodes)" "$(latest_tags)" "$WORK/run/model.log" || status=1
    echo "    screenshots and logs: $out"
  done
done
[ $status = 0 ] && echo "ALL PASSED" || echo "SOME CHECKS FAILED"
exit $status
