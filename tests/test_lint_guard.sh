#!/usr/bin/env bash
# test_lint_guard.sh — RED/GREEN proof for scripts/lint-guard.sh (SCHED-GAP-221)
#
# Builds a tiny throwaway Go file in a scratch git work tree, stages it, and
# runs lint-guard.sh against:
#   1. A version carrying a real prealloc finding (must exit 1)
#   2. A version with the prealloc fix (must exit 0)
# Then runs the same checks in full mode and against a no-Go-files staged
# state to cover the skip path.
#
# Each scenario creates a temp work dir with its OWN git repo (not the
# scheduler repo) so the test is hermetic — no side effects on the real
# repo, no flakes from a dirty main, no interaction with the live
# .golangci.yml of the scheduler. The test brings its own minimal
# .golangci.yml that enables prealloc so the RED finding is reliable.
#
# Requires: bash, git, go, golangci-lint, coreutils.
# Runs in ~10s on a warm cache.

set -u

LINT_GUARD="$(cd "$(dirname "$0")/.." && pwd)/scripts/lint-guard.sh"
if [ ! -x "$LINT_GUARD" ]; then
    echo "FAIL: $LINT_GUARD not executable" >&2
    exit 1
fi
if ! command -v golangci-lint >/dev/null 2>&1; then
    echo "SKIP: golangci-lint not on PATH; install it to run this test" >&2
    exit 0
fi

# golangci-lint's default cache (~/.cache/golangci-lint) is keyed by absolute
# file path; if a previous run in the same cache dir used a different tmp
# path, the generated_file_filter can re-read a now-missing path and report
# 0 issues while still printing a warning. Blow the cache away so each run
# starts clean. The cache regenerates on the next golangci-lint invocation
# in well under a second for the tiny fixtures this test uses.
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/golangci-lint"
if [ -d "$CACHE_DIR" ]; then
    rm -rf "$CACHE_DIR"
fi

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

setup_repo() {
    local dir="$1"
    mkdir -p "$dir"
    cd "$dir"
    git init -q -b main
    git config user.email "lint-guard-test@local"
    git config user.name "lint-guard test"
    # Minimal config: only the prealloc linter so the RED finding is
    # deterministic and the run is fast. The full scheduler .golangci.yml
    # is not used here on purpose — this is a unit test of the script.
    cat > .golangci.yml <<'YAML'
version: "2"
linters:
  enable:
    - prealloc
YAML
    cat > go.mod <<'MOD'
module lintguardtest

go 1.22
MOD
    # First commit: empty Go file so HEAD exists and a new staged file
    # has a diff to grade.
    mkdir -p pkg
    cat > pkg/empty.go <<'GO'
package pkg
GO
    git add -A
    git commit -q -m "initial: empty package"
}

# ──────────────────────────────────────────────────────────────────────
# Case 1: prealloc finding must make lint-guard exit 1.
# ──────────────────────────────────────────────────────────────────────
RED_DIR="$TMP_ROOT/red"
setup_repo "$RED_DIR"
cat > "$RED_DIR/pkg/red.go" <<'GO'
package pkg

// BuildRows has a real prealloc finding: the slice grows via append in a
// bounded loop and could be preallocated with `make([]int, 0, len(xs))`.
func BuildRows(xs []int) []int {
	var rows []int
	for _, x := range xs {
		rows = append(rows, x)
	}
	return rows
}
GO
cd "$RED_DIR"
git add -A
RED_OUT="$(LINT_GUARD_REQUIRE=1 "$LINT_GUARD" 2>&1)"
RED_RC=$?
echo "RED case (prealloc finding): exit=$RED_RC"
if [ "$RED_RC" -ne 1 ]; then
    echo "FAIL: lint-guard should exit 1 on a prealloc finding, got $RED_RC"
    echo "--- output ---"
    echo "$RED_OUT"
    exit 1
fi
if ! echo "$RED_OUT" | grep -q "lint-guard: FAIL"; then
    echo "FAIL: expected 'lint-guard: FAIL' line in output"
    echo "--- output ---"
    echo "$RED_OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 2: the prealloc fix must make lint-guard exit 0.
# ──────────────────────────────────────────────────────────────────────
GREEN_DIR="$TMP_ROOT/green"
setup_repo "$GREEN_DIR"
cat > "$GREEN_DIR/pkg/green.go" <<'GO'
package pkg

// BuildRows preallocates the slice with the right capacity, then uses
// the idiomatic append that satisfies both prealloc AND staticcheck
// (S1011: prefer `append(s, other...)` over an explicit range loop when
// the elements come from a slice).
func BuildRows(xs []int) []int {
	rows := make([]int, 0, len(xs))
	return append(rows, xs...)
}
GO
cd "$GREEN_DIR"
git add -A
GREEN_OUT="$("$LINT_GUARD" 2>&1)"
GREEN_RC=$?
echo "GREEN case (prealloc fix): exit=$GREEN_RC"
if [ "$GREEN_RC" -ne 0 ]; then
    echo "FAIL: lint-guard should exit 0 on a prealloc-fixed file, got $GREEN_RC"
    echo "--- output ---"
    echo "$GREEN_OUT"
    exit 1
fi
if ! echo "$GREEN_OUT" | grep -q "lint-guard: PASS"; then
    echo "FAIL: expected 'lint-guard: PASS' line in output"
    echo "--- output ---"
    echo "$GREEN_OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 3: no Go files staged => silent exit 0 (the board-only-commit path).
# ──────────────────────────────────────────────────────────────────────
NOGO_DIR="$TMP_ROOT/nogo"
setup_repo "$NOGO_DIR"
cd "$NOGO_DIR"
echo "noise" > notes.md
git add -A
NOGO_OUT="$("$LINT_GUARD" 2>&1)"
NOGO_RC=$?
echo "NOGO case (only markdown staged): exit=$NOGO_RC"
if [ "$NOGO_RC" -ne 0 ]; then
    echo "FAIL: lint-guard should exit 0 when no Go files are staged, got $NOGO_RC"
    echo "--- output ---"
    echo "$NOGO_OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 4: LINT_GUARD_SKIP=1 produces the visible skip line.
# ──────────────────────────────────────────────────────────────────────
SKIP_OUT="$(cd "$GREEN_DIR" && LINT_GUARD_SKIP=1 "$LINT_GUARD" 2>&1)"
SKIP_RC=$?
echo "SKIP case (LINT_GUARD_SKIP=1): exit=$SKIP_RC"
if [ "$SKIP_RC" -ne 0 ]; then
    echo "FAIL: lint-guard should exit 0 on explicit skip, got $SKIP_RC"
    exit 1
fi
if ! echo "$SKIP_OUT" | grep -q "lint-guard: SKIP"; then
    echo "FAIL: expected 'lint-guard: SKIP' line in output"
    echo "--- output ---"
    echo "$SKIP_OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 5: full mode exercises the whole-tree grading path.
# ──────────────────────────────────────────────────────────────────────
FULL_DIR="$TMP_ROOT/full"
setup_repo "$FULL_DIR"
# Add a prealloc finding in a non-staged file (i.e. already in the tree).
# Full mode must catch it because it grades the whole tree, not just staged.
cat > "$FULL_DIR/pkg/red.go" <<'GO'
package pkg

func BuildRows(xs []int) []int {
	var rows []int
	for _, x := range xs {
		rows = append(rows, x)
	}
	return rows
}
GO
cd "$FULL_DIR"
git add -A
git commit -q -m "introduce prealloc finding"
FULL_OUT="$(LINT_GUARD_REQUIRE=1 "$LINT_GUARD" --full 2>&1)"
FULL_RC=$?
echo "FULL case (prealloc finding, --full): exit=$FULL_RC"
if [ "$FULL_RC" -ne 1 ]; then
    echo "FAIL: lint-guard --full should exit 1 on a prealloc finding in tree, got $FULL_RC"
    echo "--- output ---"
    echo "$FULL_OUT"
    exit 1
fi

echo
echo "OK: lint-guard.sh — RED/GREEN/SKIP/FULL all behave as specified (SCHED-GAP-221)"
exit 0
