#!/usr/bin/env bash
# install-from-repo.sh — the single repo-owned install path for bd/brain.
#
# Builds the binary from THIS checkout's HEAD, stamped with the commit, a dirty
# flag and the build time (see `bd version`), and installs it atomically to the
# user install location. Every host runs this so every machine runs the commands
# the repo documents; `brain stores doctor` warns when the installed binary's
# commit is not the repo's current main or carries no stamp at all.
#
# Like deploy-local.sh it writes only the binary path ($BEADS_BIN, default
# ~/.local/bin/beads) and never touches the `bd` federation wrapper, store-host
# config or any Dolt server. Use `make install` only on a box with no wrapper.
#
# Usage:
#   scripts/install-from-repo.sh            # build + install HEAD of this checkout
#   scripts/install-from-repo.sh --dry-run  # build and verify the stamp, install nothing
#
# Env:
#   BEADS_BIN  install path (default: $HOME/.local/bin/beads)

set -euo pipefail

BEADS_BIN="${BEADS_BIN:-$HOME/.local/bin/beads}"
dry_run=0
case "${1:-}" in
    --dry-run) dry_run=1 ;;
    "") ;;
    *) echo "install-from-repo.sh: unknown argument '$1' (expected --dry-run)" >&2; exit 2 ;;
esac

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
head_sha="$(git rev-parse HEAD)"

# SKIP_UPDATE_CHECK: installing a checkout is deliberate; the doctor reports
# any drift from main afterwards.
make -C "$repo_root" build SKIP_UPDATE_CHECK=1
built="$repo_root/bd"
[ -x "$built" ] || { echo "install-from-repo.sh: ERROR: build produced no binary at $built" >&2; exit 1; }

# Verify the stamp before installing anything over the live binary.
stamp="$("$built" version --json 2>/dev/null || true)"
case "$stamp" in
    *"\"commit\": \"$head_sha\""*|*"\"commit\":\"$head_sha\""*) ;;
    *) echo "install-from-repo.sh: ERROR: built binary does not carry commit $head_sha in its stamp" >&2; exit 1 ;;
esac

if [ "$dry_run" = 1 ]; then
    echo "install-from-repo.sh: dry run OK — built $built stamped $head_sha; nothing installed"
    exit 0
fi

mkdir -p "$(dirname "$BEADS_BIN")"
tmp_install="$BEADS_BIN.new.$$"
trap 'rm -f "$tmp_install"' EXIT
cp -f "$built" "$tmp_install"
chmod +x "$tmp_install"
if [ "$(uname)" = "Darwin" ]; then
    codesign -s - -f "$tmp_install" 2>/dev/null || true
fi
mv -f "$tmp_install" "$BEADS_BIN"
echo "install-from-repo.sh: installed $(git rev-parse --short HEAD) -> $BEADS_BIN"
"$BEADS_BIN" version
