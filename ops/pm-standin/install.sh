#!/usr/bin/env bash
# install.sh — deploy the PM stand-in live scripts as stable symlinks to the
# tracked copies in this repository (ops/pm-standin/). GAP-048.
#
# Idempotent: running it twice changes nothing the second time.
# Scope guarantee: the ONLY paths touched under $HERMES_HOME (default
# $HOME/.hermes) are the two live scripts, their backups, and the scripts
# directory that contains them. No other state is read, printed, or modified.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
    # Not inside a git checkout (e.g. tarball): ops/pm-standin/<this file>.
    REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
fi

DRIVER_SRC="$REPO_ROOT/ops/pm-standin/pm-standin-tick.sh"
HELPER_SRC="$REPO_ROOT/ops/pm-standin/dagger-role-report.py"
HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
SCRIPTS_DIR="$HERMES_HOME/scripts"

for src in "$DRIVER_SRC" "$HELPER_SRC"; do
    if [[ ! -s "$src" ]]; then
        echo "install.sh: FATAL tracked source missing or empty: $src" >&2
        exit 1
    fi
done

# Validate payload syntax before touching any live path.
bash -n "$DRIVER_SRC"
if command -v python3 >/dev/null 2>&1; then
    # Bytecode cache goes to a temp file OUTSIDE the repository.
    pycache_tmp="$(mktemp "${TMPDIR:-/tmp}/install-pycache.XXXXXX")"
    trap 'rm -f "$pycache_tmp"' EXIT
    python3 -c 'import py_compile, sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)' \
        "$HELPER_SRC" "$pycache_tmp"
fi

mkdir -p "$SCRIPTS_DIR"

deploy() {
    local src="$1" dst="$2" tmp
    if [[ -L "$dst" ]] && [[ "$(readlink -f "$dst")" == "$(readlink -f "$src")" ]]; then
        echo "install.sh: $dst already links to $src (no change)"
        return 0
    fi
    if [[ -e "$dst" && ! -L "$dst" ]]; then
        local backup
        backup="$dst.bak-$(date +%Y%m%d-%H%M%S)"
        cp -p "$dst" "$backup"
        echo "install.sh: backed up pre-existing file $dst -> $backup"
    fi
    # Atomic refresh: create the new symlink under a temp name in the same
    # directory, then rename(2) it over the destination.
    tmp="$dst.link.$$"
    ln -sfn "$src" "$tmp"
    mv -T "$tmp" "$dst"
    echo "install.sh: linked $dst -> $src"
}

deploy "$DRIVER_SRC" "$SCRIPTS_DIR/pm-standin-tick.sh"
deploy "$HELPER_SRC" "$SCRIPTS_DIR/dagger-role-report.py"

echo "install.sh: done."
