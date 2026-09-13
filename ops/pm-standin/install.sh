#!/usr/bin/env bash
# install.sh — deploy the PM stand-in live scripts as stable symlinks to the
# tracked copies in this repository (ops/pm-standin/). GAP-048.
#
# Payloads (three live destinations):
#   ~/.hermes/scripts/pm-standin-tick.sh        <- ops/pm-standin/pm-standin-tick.sh
#   ~/.hermes/scripts/dagger-role-report.py     <- ops/pm-standin/dagger-role-report.py
#   ~/.hermes/stand-in/ledger_board_reconcile.py <- ops/pm-standin/ledger_board_reconcile.py
#
# Idempotent: running it twice changes nothing the second time.
# Scope guarantee: the ONLY paths touched under $HERMES_HOME (default
# $HOME/.hermes) are the three live scripts, their backups, and the two
# directories that contain them (scripts/ and stand-in/). No other state is
# read, printed, or modified — ledger.json and scheduler.db are never opened.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
    # Not inside a git checkout (e.g. tarball): ops/pm-standin/<this file>.
    REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
fi

DRIVER_SRC="$REPO_ROOT/ops/pm-standin/pm-standin-tick.sh"
HELPER_SRC="$REPO_ROOT/ops/pm-standin/dagger-role-report.py"
LEDGER_SRC="$REPO_ROOT/ops/pm-standin/ledger_board_reconcile.py"
HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
SCRIPTS_DIR="$HERMES_HOME/scripts"
STANDIN_DIR="$HERMES_HOME/stand-in"

for src in "$DRIVER_SRC" "$HELPER_SRC" "$LEDGER_SRC"; do
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
    python3 -c 'import py_compile, sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)' \
        "$LEDGER_SRC" "$pycache_tmp"
fi

# The stand-in dir may not exist yet (fresh host) — create it. Creating this
# directory is part of the narrow install scope; nothing inside it is read.
mkdir -p "$SCRIPTS_DIR" "$STANDIN_DIR"

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
deploy "$LEDGER_SRC" "$STANDIN_DIR/ledger_board_reconcile.py"

echo "install.sh: done."
