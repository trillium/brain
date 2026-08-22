#!/usr/bin/env bash
# deploy-local.sh — rebuild and install the live beads binary from origin/main.
#
# WHY THIS EXISTS (robots-k2r4): merging a PR to trillium/brain changed nothing
# on the box. The live binary kept whatever SHA it was last built from — which,
# for two days, was the tip of an *unmerged* PR branch. Agents ran unreviewed
# code, merged fixes were invisible, and a defect already fixed in main got
# re-filed as fresh (robots-xp07). Nothing local runs when a PR merges on
# GitHub, so the deploy has to be an explicit, idempotent step.
#
# WHY NOT `make install`: on this box ~/.local/bin/bd is the beads *federation
# wrapper* (a shell script) and ~/.local/bin/beads is the real binary. `make
# install` writes the binary to bd and makes beads a symlink to it — clobbering
# the wrapper and breaking every store command. This script installs the binary
# to $BEADS_BIN (default ~/.local/bin/beads) and never touches bd.
#
# Usage:
#   scripts/deploy-local.sh            # deploy origin/main if the live binary is stale
#   scripts/deploy-local.sh --check    # report drift only; exit 1 if stale
#   scripts/deploy-local.sh --force    # rebuild and install even if current
#
# Env:
#   BEADS_BIN   install path for the binary (default: $HOME/.local/bin/beads)
#   DEPLOY_REF  ref to deploy (default: origin/main) — must be a landed ref
#
# Cron it if you want deploys without thinking about them, e.g.:
#   0 * * * * cd ~/code/brain && scripts/deploy-local.sh >>~/.local/state/brain-deploy.log 2>&1

set -euo pipefail

BEADS_BIN="${BEADS_BIN:-$HOME/.local/bin/beads}"
DEPLOY_REF="${DEPLOY_REF:-origin/main}"

mode=deploy
case "${1:-}" in
    --check) mode=check ;;
    --force) mode=force ;;
    "") ;;
    *) echo "deploy-local.sh: unknown argument '$1' (expected --check or --force)" >&2; exit 2 ;;
esac

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# Fetch first, and treat a failed fetch as fatal (exit 2 = cannot determine).
# Falling back to the cached remote-tracking ref would let this script report
# "up to date" while origin/main has moved — which is precisely the silent
# staleness robots-k2r4 is about. Better to say "I don't know" than to say
# "you're current" and be wrong.
if ! git fetch origin main --quiet 2>/dev/null; then
    echo "deploy-local.sh: ERROR: could not fetch origin/main — refusing to compare against a possibly stale remote-tracking ref" >&2
    exit 2
fi

if ! target_sha="$(git rev-parse --verify -q "$DEPLOY_REF^{commit}")"; then
    echo "deploy-local.sh: ERROR: ref '$DEPLOY_REF' does not exist" >&2
    exit 2
fi

# Refuse to deploy anything that has not landed. Without this, an override like
# DEPLOY_REF=some-open-branch would install an unlanded binary and only notice
# in the post-install check — after the previous binary was already replaced.
# Installing unreviewed code as the system default is the defect, not a warning.
# (`--is-ancestor X X` is true, so the default origin/main passes.)
if ! git merge-base --is-ancestor "$target_sha" origin/main 2>/dev/null; then
    echo "deploy-local.sh: ERROR: ref '$DEPLOY_REF' ($(git rev-parse --short "$target_sha")) has not landed on origin/main — refusing to deploy it" >&2
    exit 2
fi
target_short="$(git rev-parse --short "$target_sha")"

# build_sha_of prints the Build SHA baked into a beads binary. `make build` sets
# Build to `git rev-parse --short HEAD`, and `bd version` prints it inside the
# parentheses on line 1:
#   bd version 1.1.0-rc.1+brain.0.4.0 (ab6ca60eb)
# The version string itself contains no 7+ character hex run, so the first such
# run on that line is the Build SHA. The text form is parsed rather than --json
# on purpose: the whole point is that the live binary may be old enough to
# predate newer flags. Prints nothing if the binary is missing or unparseable.
build_sha_of() {
    [ -x "$1" ] || return 0
    "$1" version 2>/dev/null | head -1 | grep -oE '[0-9a-f]{7,40}' | head -1 || true
}

# What SHA is the currently-installed binary built from? An absent or
# unparseable binary counts as "not deployed".
live_sha="$(build_sha_of "$BEADS_BIN")"

report_drift() {
    echo "  live:   ${live_sha:-<none>}"
    echo "  $DEPLOY_REF: $target_short"
}

if [ -n "$live_sha" ] && git merge-base --is-ancestor "$live_sha" "$target_sha" 2>/dev/null; then
    # Live build is an ancestor of (or equal to) the target. Equal means current.
    if [ "$(git rev-parse "$live_sha^{commit}")" = "$target_sha" ]; then
        if [ "$mode" = force ]; then
            echo "deploy-local.sh: live binary already at $target_short; --force given, rebuilding anyway"
        else
            echo "deploy-local.sh: up to date — $BEADS_BIN is built from $target_short ($DEPLOY_REF)"
            exit 0
        fi
    else
        echo "deploy-local.sh: STALE — live binary is behind $DEPLOY_REF"
        report_drift
        [ "$mode" = check ] && exit 1
    fi
elif [ -n "$live_sha" ]; then
    # Not an ancestor: the live binary was built from an unlanded branch, or from
    # a commit this clone has never seen. This is the robots-k2r4 failure mode.
    echo "deploy-local.sh: UNLANDED — live binary was NOT built from a commit on $DEPLOY_REF"
    report_drift
    [ "$mode" = check ] && exit 1
else
    echo "deploy-local.sh: no usable binary at $BEADS_BIN — deploying $target_short"
    [ "$mode" = check ] && exit 1
fi

# --- build ------------------------------------------------------------------
# Build from a throwaway detached worktree at the target ref so the deployed
# binary is exactly what landed: no local edits, no branch switching, and the
# caller's checkout is left on whatever branch it was already on.
build_dir="$(mktemp -d "${TMPDIR:-/tmp}/brain-deploy.XXXXXX")"
worktree_dir="$build_dir/src"
cleanup() {
    git worktree remove --force "$worktree_dir" >/dev/null 2>&1 || true
    rm -rf "$build_dir"
}
trap cleanup EXIT

echo "deploy-local.sh: building $target_short in $worktree_dir ..."
git worktree add --detach --quiet "$worktree_dir" "$target_sha"

# SKIP_UPDATE_CHECK: the worktree is a detached checkout of the target ref by
# construction, so the "are you on origin/main" guard is redundant here.
make -C "$worktree_dir" build SKIP_UPDATE_CHECK=1

built="$worktree_dir/bd"
[ -x "$built" ] || { echo "deploy-local.sh: ERROR: build produced no binary at $built" >&2; exit 1; }

# --- install ----------------------------------------------------------------
# Install atomically next to the target so a crashed copy can never leave a
# half-written binary in place, and so a running agent's exec sees old-or-new.
mkdir -p "$(dirname "$BEADS_BIN")"
tmp_install="$BEADS_BIN.new.$$"
cp "$built" "$tmp_install"
chmod +x "$tmp_install"
if [ "$(uname)" = "Darwin" ]; then
    codesign -s - -f "$tmp_install" 2>/dev/null || true
fi
mv -f "$tmp_install" "$BEADS_BIN"
echo "deploy-local.sh: installed $target_short -> $BEADS_BIN"

# --- verify -----------------------------------------------------------------
# A deploy that silently installed the wrong thing is the defect we are fixing,
# so confirm the binary we just wrote actually reports the target SHA.
installed_version="$("$BEADS_BIN" version 2>&1 || true)"
echo "deploy-local.sh: $installed_version"

# Compare the embedded Build SHA exactly, not as a substring of the whole line.
# `bd version` can also print the checkout's commit/branch, so a substring match
# would pass whenever *any* displayed SHA happened to contain target_short even
# though the binary was built from something else.
installed_sha="$(build_sha_of "$BEADS_BIN")"
if [ "$installed_sha" != "$target_short" ]; then
    echo "deploy-local.sh: ERROR: installed binary reports build ${installed_sha:-<none>}, expected $target_short" >&2
    exit 1
fi
case "$installed_version" in
    *UNLANDED*)
        echo "deploy-local.sh: ERROR: installed binary reports UNLANDED after deploying $DEPLOY_REF" >&2
        exit 1
        ;;
esac

echo "deploy-local.sh: OK"
