#!/bin/bash
# Advisory pre-push check: warn when upstream beads has a newer TAGGED release
# than the beads version brain is currently based on, and file a sync task bead.
#
# brain is a SUPERSET of upstream gastownhall/beads. cmd/bd/version.go's
# `Version` var records the upstream beads version brain is presently built on.
# When upstream publishes a newer git-tagged release, brain's superset should be
# restored on top of it (see the Reconcile skill and bead task-iy4i).
#
# This check is ADVISORY ONLY. It ALWAYS exits 0 and never blocks a push.
# Any failure (offline, missing tools) degrades to a warning and exits 0.
#
# Env:
#   DRY_RUN=1   Print what would be filed without calling `task create`.
#
# Manual install of the git hook (worktrees share the common git dir, so this
# activates the check for every checkout of this repo):
#   printf '#!/bin/bash\nexec "$(git rev-parse --show-toplevel)/scripts/check-beads-upstream.sh" "$@"\n' \
#     > "$(git rev-parse --git-common-dir)/hooks/pre-push"
#   chmod +x "$(git rev-parse --git-common-dir)/hooks/pre-push"
# Or, to install all tracked hooks at once: ./scripts/install-hooks.sh

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Repo-root guard (matches house style). Advisory: warn, don't fail the push.
if [ ! -f "cmd/bd/version.go" ]; then
    ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
    if [ -n "$ROOT" ] && [ -f "$ROOT/cmd/bd/version.go" ]; then
        cd "$ROOT"
    else
        # Not in the brain repo root; nothing to check. Stay quiet, never block.
        exit 0
    fi
fi

warn() { echo -e "${YELLOW}⚠ beads-upstream check: $*${NC}" >&2; }

# --- Current beads base version (the upstream beads version brain is on) ------
CURRENT_BASE=$(grep -E '^[[:space:]]*Version = ' cmd/bd/version.go \
    | sed 's/.*"\(.*\)".*/\1/' | head -1 || true)
if [ -z "$CURRENT_BASE" ]; then
    warn "could not read Version from cmd/bd/version.go; skipping (advisory)."
    exit 0
fi

# --- Latest upstream beads TAGGED release -------------------------------------
# Read-only ls-remote (no mutating fetch). The trailing $ anchor keeps this to
# final releases only (vX.Y.Z), excluding prerelease tags like vX.Y.Z-rc.1.
LATEST_TAG=$(git ls-remote --tags upstream 2>/dev/null \
    | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
if [ -z "$LATEST_TAG" ]; then
    warn "could not resolve latest upstream beads release (offline?); skipping (advisory)."
    exit 0
fi
LATEST_VER="${LATEST_TAG#v}"

# --- Proper semver comparison (prerelease < release) --------------------------
# NOTE: `sort -V` is NOT used for the rc-vs-release decision. GNU/BSD version
# sort ranks "1.1.0-rc.1" ABOVE "1.1.0" (the longer string wins), the opposite
# of the semver rule that a prerelease is LESS than its final release. So we
# compare by hand. Returns 0 (true) when $1 > $2 per semver.
semver_gt() {
    local a="$1" b="$2"
    local a_core="${a%%-*}" b_core="${b%%-*}"
    local a_pre="" b_pre=""
    [ "$a" != "$a_core" ] && a_pre="${a#*-}"
    [ "$b" != "$b_core" ] && b_pre="${b#*-}"

    local a1 a2 a3 b1 b2 b3
    IFS=. read -r a1 a2 a3 <<< "$a_core"
    IFS=. read -r b1 b2 b3 <<< "$b_core"
    a1=${a1:-0}; a2=${a2:-0}; a3=${a3:-0}
    b1=${b1:-0}; b2=${b2:-0}; b3=${b3:-0}

    [ "$a1" -ne "$b1" ] && { [ "$a1" -gt "$b1" ]; return; }
    [ "$a2" -ne "$b2" ] && { [ "$a2" -gt "$b2" ]; return; }
    [ "$a3" -ne "$b3" ] && { [ "$a3" -gt "$b3" ]; return; }

    # Cores equal: a version WITH a prerelease is LESS than one WITHOUT.
    if [ -z "$a_pre" ] && [ -z "$b_pre" ]; then return 1; fi   # equal -> not >
    if [ -z "$a_pre" ]; then return 0; fi                      # a release > b prerelease
    if [ -z "$b_pre" ]; then return 1; fi                      # a prerelease < b release

    # Both prerelease: dot-wise identifier compare (numeric < non-numeric,
    # numbers numerically, strings lexically, fewer fields < more fields).
    local IFSsave="$IFS"; IFS=.
    local -a apa bpa
    read -r -a apa <<< "$a_pre"
    read -r -a bpa <<< "$b_pre"
    IFS="$IFSsave"
    local i n=${#apa[@]}
    [ "${#bpa[@]}" -gt "$n" ] && n=${#bpa[@]}
    for ((i=0; i<n; i++)); do
        local ai="${apa[i]-}" bi="${bpa[i]-}"
        [ -z "$ai" ] && { return 1; }   # a ran out -> a < b
        [ -z "$bi" ] && { return 0; }   # b ran out -> a > b
        if [[ "$ai" =~ ^[0-9]+$ && "$bi" =~ ^[0-9]+$ ]]; then
            [ "$ai" -ne "$bi" ] && { [ "$ai" -gt "$bi" ]; return; }
        elif [[ "$ai" =~ ^[0-9]+$ ]]; then
            return 1   # numeric < non-numeric
        elif [[ "$bi" =~ ^[0-9]+$ ]]; then
            return 0   # non-numeric > numeric
        else
            [ "$ai" != "$bi" ] && { [[ "$ai" > "$bi" ]]; return; }
        fi
    done
    return 1   # equal
}

if ! semver_gt "$LATEST_VER" "$CURRENT_BASE"; then
    # Up to date (or brain ahead). Stay near-silent.
    exit 0
fi

# --- A newer upstream release exists: warn + file a sync bead ------------------
echo -e "${YELLOW}⚠ upstream beads ${LATEST_TAG} is newer than brain's current base ${CURRENT_BASE}.${NC}" >&2

TITLE="Sync brain onto beads ${LATEST_TAG} (currently on ${CURRENT_BASE})"

# Idempotency: skip if an OPEN task bead already targets this exact upstream tag.
if command -v task >/dev/null 2>&1; then
    EXISTING=$(task list --status open --json 2>/dev/null \
        | grep -oE "beads ${LATEST_TAG}[^\"]*" | head -1 || true)
    if [ -n "$EXISTING" ]; then
        echo -e "${GREEN}✓ sync bead for beads ${LATEST_TAG} already open; nothing to file.${NC}" >&2
        exit 0
    fi
else
    warn "'task' CLI not found; cannot file sync bead (advisory)."
    exit 0
fi

BODY_FILE="$(mktemp -t check-beads-upstream.XXXXXX)"
trap 'rm -f "$BODY_FILE"' EXIT
cat > "$BODY_FILE" <<EOF
Upstream gastownhall/beads has published ${LATEST_TAG}; brain's current base is ${CURRENT_BASE} (cmd/bd/version.go).

brain is a SUPERSET of beads. Restore brain's functionality ON TOP OF beads ${LATEST_TAG} — a TAGGED release only, never an untagged upstream/main HEAD.

Steps:
- Rebase/restore brain's superset onto beads ${LATEST_TAG}.
- Verify brain's surface survives: cmd/bd/brain_*.go, the isa/stores commands, README, and a clean build.
- Document the beads changes pulled in with this bump.

References: the Reconcile skill, and related bead task-iy4i (re-integrate upstream beads without clobbering brain's cmd surface).
EOF

if [ "${DRY_RUN:-}" = "1" ]; then
    echo -e "${YELLOW}[DRY_RUN] would file 1 bead:${NC}" >&2
    echo "  task create \"${TITLE}\" --body-file <tmp> -p 3" >&2
    echo "  --- body ---" >&2
    sed 's/^/  /' "$BODY_FILE" >&2
    exit 0
fi

NEW_ID=$(task create "$TITLE" --body-file "$BODY_FILE" -p 3 2>/dev/null | grep -oE 'task-[a-z0-9]+' | head -1 || true)
if [ -n "$NEW_ID" ]; then
    echo -e "${GREEN}✓ filed sync bead ${NEW_ID}: ${TITLE}${NC}" >&2
else
    warn "failed to file sync bead (advisory); please run: task create \"${TITLE}\" -p 3"
fi

exit 0
