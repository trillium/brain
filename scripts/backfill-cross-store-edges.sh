#!/bin/bash
# backfill-cross-store-edges.sh — the one-time repair for dependency rows whose
# target is stored as text in depends_on_external even though this database
# holds it (see docs/design/brain-single-database.md §"An edge to a bead this
# database holds is a real edge").
#
# This is a thin wrapper, on purpose: it execs the store's own `bd` so the
# repair inherits exactly the store resolution, credentials and host that bd
# already uses — no host, port or password is hardcoded here, and repointing
# the store to another machine needs no change to this script (brain-e5tlj,
# captain's requirement that tools follow the store).
#
# Dry run by default: without --apply the command reads and reports, and writes
# nothing. With --apply the whole repair lands as ONE Dolt commit (staging only
# the edge table and `issues`), so reverting it is one `dolt revert`, and the
# repair refuses to start when those tables already carry uncommitted work.
#
# NOT run automatically by anything. Run it from the store whose database holds
# the rows, or through that store's wrapper, e.g.:
#   scripts/backfill-cross-store-edges.sh              # report the plan
#   scripts/backfill-cross-store-edges.sh --apply      # repair, one commit
#   BD_NAME=brain scripts/backfill-cross-store-edges.sh --json
#
# Usage: backfill-cross-store-edges.sh [--dry-run | --apply] [--json] [--bd path]

set -euo pipefail

BD_BIN="bd"
DRY_RUN_EXPLICIT=false
APPLY=false
ARGS=()

while [ "$#" -gt 0 ]; do
    case "$1" in
        --dry-run)
            DRY_RUN_EXPLICIT=true
            shift
            ;;
        --apply)
            APPLY=true
            ARGS+=("--apply")
            shift
            ;;
        -h|--help)
            sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        --json)
            ARGS+=("--json")
            shift
            ;;
        --bd)
            [ "$#" -ge 2 ] || { echo "Error: --bd needs a path" >&2; exit 2; }
            BD_BIN="$2"
            shift 2
            ;;
        *)
            echo "Error: unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

if [ "$APPLY" = true ] && [ "$DRY_RUN_EXPLICIT" = true ]; then
    echo "Error: --dry-run and --apply are mutually exclusive" >&2
    exit 2
fi

if ! command -v "$BD_BIN" >/dev/null 2>&1; then
    echo "Error: '$BD_BIN' not found on PATH; run this where a store wrapper is installed" >&2
    exit 1
fi

# Say which store's database this will act on before acting on it.
STORE_LABEL="${BD_NAME:-no BD_NAME set (the local workspace store)}"
if [ "$APPLY" = true ]; then
    echo "Applying the external-target repair to the store behind: ${STORE_LABEL}" >&2
    echo "One Dolt commit; revert it with: bd dolt revert <hash>" >&2
else
    echo "Dry run (nothing written) against the store behind: ${STORE_LABEL}" >&2
fi

# ${ARGS[@]+...} keeps an empty array safe under 'set -u' in bash 3.2.
exec "$BD_BIN" dep reclassify ${ARGS[@]+"${ARGS[@]}"}
