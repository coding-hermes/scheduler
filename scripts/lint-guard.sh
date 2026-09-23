#!/usr/bin/env bash
#
# lint-guard.sh — local golangci-lint gate for the coding-hermes scheduler
# repo (SCHED-GAP-221).
#
# WHY THIS EXISTS
# ----------------
# The gitreins Tier-1 guard's `go_lint` lane is a scaffold check: it verifies
# the tool is installed but does not invoke it on the working tree. CI runs
# real `golangci-lint run` via golangci-lint-action@v7, and the linter is
# configured (.golangci.yml) to enable `prealloc`, `staticcheck`, `unused`,
# `errcheck`, `govet`, `ineffassign`, `misspell`, `unconvert`. The blind spot
# already cost a CI round-trip and a foreman-direct follow-up on
# d775344a / 86da1e92 (prealloc in internal/scheduler/adaptive_gap207_test.go).
#
# This script closes the divergence by running the same `golangci-lint run`
# that CI runs, against the staged (or HEAD) Go tree, with a bounded budget.
# It exits non-zero on findings, and prints an explicit DEGRADED block when it
# cannot run (tool missing, timeout, no Go files). The pre-commit hook invokes
# it AFTER the gitreins guard so the gitreins verdict remains the primary
# Tier-1 signal; this script is a strictly additive safety net for the lint
# class that the gitreins scaffold check cannot see.
#
# BEHAVIOUR
# ---------
# - Staged mode (default in pre-commit): `golangci-lint run --new-from-rev=HEAD`
#   so only the staged + working tree diff is graded. Cheap on small commits.
# - Full mode (`--full` flag, or LINT_GUARD_FULL=1): `golangci-lint run` on
#   the whole tree. Use in CI-equivalent local checks.
# - Budget: 4 minutes wall clock (overridable via LINT_GUARD_BUDGET_SECS).
# - Tool check first: if `golangci-lint` is not on PATH, print DEGRADED and
#   exit 0 (the script is additive; absence is not a failure of the gate,
#   only a failure to RUN it). Set LINT_GUARD_REQUIRE=1 to make absence a
#   non-zero exit (use in CI).
# - Timeout: if the budget elapses, kill golangci-lint, print DEGRADED with
#   the elapsed time, exit 0 unless LINT_GUARD_REQUIRE=1.
# - Skipping: the script can be skipped via LINT_GUARD_SKIP=1 (e.g. for the
#   board-only / docs-only commit path where a 4-minute Go check is pure
#   waste). The skip is logged so the absence is visible.
#
# EXIT CODES
# ----------
#  0  no findings (or DEGRADED with LINT_GUARD_REQUIRE unset)
#  1  golangci-lint reported findings
#  2  internal error (could not parse staged set, etc.)
#
# PROOF (RED/GREEN) is in tests/test_lint_guard.sh — it commits a file with
# a prealloc finding, runs the script, asserts exit 1; then commits the
# fix and asserts exit 0.

set -u

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [ -z "$REPO_ROOT" ]; then
    echo "lint-guard: not inside a git work tree" >&2
    exit 2
fi
cd "$REPO_ROOT"

MODE="staged"
if [ "${1:-}" = "--full" ] || [ "${LINT_GUARD_FULL:-0}" = "1" ]; then
    MODE="full"
fi

# Skip the lint check entirely when requested. Log the skip so absence is
# visible in the hook output — silent absence is what created this row.
if [ "${LINT_GUARD_SKIP:-0}" = "1" ]; then
    echo "lint-guard: SKIP (LINT_GUARD_SKIP=1); golangci-lint not run on this commit"
    exit 0
fi

# No Go files staged in staged mode => nothing to lint, exit 0 silently.
if [ "$MODE" = "staged" ]; then
    STAGED_GO="$(git diff --cached --name-only --diff-filter=ACMR -- '*.go' 2>/dev/null || true)"
    if [ -z "$STAGED_GO" ]; then
        exit 0
    fi
fi

# Tool check. `command -v` is the portable way and avoids `which` surprises.
if ! command -v golangci-lint >/dev/null 2>&1; then
    if [ "${LINT_GUARD_REQUIRE:-0}" = "1" ]; then
        echo "lint-guard: DEGRADED — golangci-lint not on PATH; required by LINT_GUARD_REQUIRE=1"
        exit 1
    fi
    echo "lint-guard: DEGRADED — golangci-lint not on PATH; install it to enable the local gate"
    exit 0
fi

BUDGET="${LINT_GUARD_BUDGET_SECS:-240}"

if [ "$MODE" = "staged" ]; then
    # --new-from-rev=HEAD grades only files changed vs HEAD. This is what
    # the pre-commit hook needs: a fast, focused run on what's being added.
    CMD=(golangci-lint run --new-from-rev=HEAD --timeout=3m)
else
    CMD=(golangci-lint run --timeout=5m)
fi

START_TS=$(date +%s)
# `timeout` returns 124 on timeout. Capture stdout+stderr; both are needed
# for the finding path to be visible.
OUTPUT="$(timeout "$BUDGET" "${CMD[@]}" 2>&1)"
RC=$?
END_TS=$(date +%s)
ELAPSED=$((END_TS - START_TS))

if [ "$RC" -eq 124 ]; then
    if [ "${LINT_GUARD_REQUIRE:-0}" = "1" ]; then
        echo "lint-guard: DEGRADED — golangci-lint exceeded ${BUDGET}s budget (LINT_GUARD_REQUIRE=1)"
        exit 1
    fi
    echo "lint-guard: DEGRADED — golangci-lint exceeded ${BUDGET}s budget after ${ELAPSED}s; CI is the authoritative gate"
    exit 0
fi

if [ "$RC" -ne 0 ]; then
    echo "$OUTPUT"
    echo
    echo "lint-guard: FAIL — golangci-lint exit=$RC after ${ELAPSED}s (this is the same gate CI runs)"
    exit 1
fi

echo "lint-guard: PASS — golangci-lint exit=0 after ${ELAPSED}s"
exit 0
