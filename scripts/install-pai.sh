#!/bin/sh
# install-pai.sh — PAI-safe install for the inverted beads/bd layout.
#
# Background (see robots-bpig):
#   On the PAI host the layout is INVERTED relative to a stock `make install`:
#     ~/.local/bin/beads  = the real compiled bd binary
#     ~/.local/bin/bd      = a federation-discovery SHELL wrapper (resolves the
#                            active store, prints the federation menu with no
#                            store context, and delegates to `beads`)
#   Stock `make install` makes `bd` the binary and `beads` a symlink -> bd,
#   which DELETES the federation wrapper and breaks every store's resolution.
#
# This script installs the freshly built binary to `beads`, re-signs it on
# macOS (the cp invalidates the ad-hoc code signature -> the kernel SIGKILLs the
# binary with exit 137 until it is re-signed), and NEVER touches
# ~/.local/bin/bd.
#
# Usage:
#   make install-pai                 # builds, then runs this script
#   ./scripts/install-pai.sh         # after a `make build`, install only
#
# Honors INSTALL_DIR and BUILD_DIR from the environment (the Makefile passes
# both); defaults match the Makefile.

set -eu

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
BUILD_DIR="${BUILD_DIR:-.}"
SRC="$BUILD_DIR/bd"
DEST="$INSTALL_DIR/beads"
BD_WRAPPER="$INSTALL_DIR/bd"

# The freshly built binary must exist. `make install-pai` depends on `build`,
# but a bare invocation of this script does not, so fail loudly if missing.
if [ ! -f "$SRC" ]; then
    echo "ERROR: built binary not found at '$SRC'." >&2
    echo "Run 'make build' first, or use 'make install-pai'." >&2
    exit 1
fi

mkdir -p "$INSTALL_DIR"

# Safety: refuse to overwrite $DEST if it is currently a shell script. In the
# inverted layout `beads` is always the binary; a script there means the layout
# is not what we expect and blindly overwriting could destroy a wrapper.
if [ -f "$DEST" ] && [ "$(head -c 2 "$DEST" 2>/dev/null)" = '#!' ]; then
    echo "ERROR: '$DEST' looks like a shell script, not the beads binary." >&2
    echo "Refusing to overwrite. Inspect '$DEST' manually." >&2
    exit 1
fi

# Record the pre-existing bd wrapper so we can assert we left it untouched.
BD_WAS_SCRIPT=0
if [ -f "$BD_WRAPPER" ] && [ "$(head -c 2 "$BD_WRAPPER" 2>/dev/null)" = '#!' ]; then
    BD_WAS_SCRIPT=1
fi

cp "$SRC" "$DEST"
echo "Installed beads binary -> $DEST"

# macOS: re-sign ad-hoc. Without this the copied binary is SIGKILLed (exit 137).
if [ "$(uname)" = "Darwin" ]; then
    codesign -s - -f "$DEST"
    echo "Re-signed '$DEST' (ad-hoc) for macOS"
fi

# Never touch the federation wrapper. Prove it is still a script if it was one.
if [ "$BD_WAS_SCRIPT" -eq 1 ]; then
    if [ -f "$BD_WRAPPER" ] && [ "$(head -c 2 "$BD_WRAPPER" 2>/dev/null)" = '#!' ]; then
        echo "Left federation wrapper intact: $BD_WRAPPER"
    else
        echo "ERROR: federation wrapper '$BD_WRAPPER' changed unexpectedly!" >&2
        exit 1
    fi
elif [ -e "$BD_WRAPPER" ]; then
    echo "Left '$BD_WRAPPER' untouched (not a federation wrapper)."
fi

echo "install-pai complete."
