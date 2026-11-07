#!/usr/bin/env bash
#
# install-hooks.sh — install the gitreins + lint-guard pre-commit hook
# (SCHED-GAP-221) into the git common dir's hooks/pre-commit.
#
# Idempotent: safe to re-run. The hook ALWAYS invokes the board-commit
# guard first (SCHED-GAP-1699 mixing rule + RELEASE-046 stale-base row-drop
# rule; enforced fail-closed before anything else, never skippable — a
# missing guard script refuses the commit instead of silently skipping).
# The heavy steps — gitreins guard (the primary Tier-1 signal),
# scripts/lint-guard.sh (the additive safety net for the lint class the
# gitreins scaffold check cannot see) and
# scripts/policy-script-deploy-hash-guard.sh — then run only for changesets
# that are NOT board-only (see the RELEASE-046 note in the hook body). If
# any of them fails the commit is blocked.
#
# Use:
#   ./scripts/install-hooks.sh          # install
#   ./scripts/install-hooks.sh --check  # verify without writing
#
# Why this is a separate script (and not just editing the hook in place):
# git hooks live in the git common dir's hooks/, which is not
# version-controlled. This script is the one-line ship path that lets a
# fresh clone reproduce the same hook layout the gitreins + lint-guard
# chain was designed around.

set -u

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [ -z "$REPO_ROOT" ]; then
    echo "install-hooks: not in a git work tree" >&2
    exit 2
fi
cd "$REPO_ROOT"

# Hooks live in --git-common-dir/hooks/ so a single install covers the
# main checkout AND every linked worktree. (.git/ in a worktree is a
# file, not a directory, so resolving via --show-toplevel + .git/hooks
# does not work from a worktree.)
GIT_COMMON_DIR="$(git rev-parse --git-common-dir)"
HOOK="$GIT_COMMON_DIR/hooks/pre-commit"
MARKER="policy-script-deploy-hash-guard.sh"  # presence = hook already installed (SCHED-PERF-006 block)
BOARD_MARKER="board-commit-guard.sh"         # presence of the SCHED-GAP-1699 step (added later than the marker above)

if [ "${1:-}" = "--check" ]; then
    if [ -f "$HOOK" ] && grep -q "$MARKER" "$HOOK"; then
        if grep -q "$BOARD_MARKER" "$HOOK"; then
            echo "install-hooks: OK (lint-guard + board-commit-guard pre-commit hook present)"
            exit 0
        fi
        echo "install-hooks: STALE (board-commit-guard step missing — re-run scripts/install-hooks.sh)" >&2
        exit 1
    fi
    echo "install-hooks: MISSING (run scripts/install-hooks.sh to install)" >&2
    exit 1
fi

if [ -f "$HOOK" ] && grep -q "$MARKER" "$HOOK"; then
    echo "install-hooks: already installed at $HOOK"
    exit 0
fi

mkdir -p "$(dirname "$HOOK")"

cat > "$HOOK" <<'HOOK'
#!/usr/bin/env bash
# GitReins + lint-guard + board-commit-guard pre-commit hook (SCHED-GAP-221,
# board step SCHED-GAP-1699, stale-base step RELEASE-046).
#
# Order matters: the board-commit guard runs FIRST and unconditionally —
# a CODE commit must never touch .coding-hermes/board/* (worker worktrees
# hold a stale board snapshot; committing it with code deletes every row
# appended since the branch — incidents REMOTE-006, REMOTE-008), and a
# board-only commit staged from a base behind origin/main must never drop
# rows (RELEASE-046). It must never be bypassable by LINT_GUARD_SKIP and it
# must never be silently skipped: a missing guard script refuses the commit.
#
# RELEASE-046 gate on the heavy steps: gitreins guard, lint-guard.sh and
# policy-script-deploy-hash-guard.sh run ONLY when the staged changeset is
# NOT board-only (at least one staged path outside .coding-hermes/). WHY:
# the .gitreins guard battery carries a 900 s test timeout, and every lane
# and every linked worktree commits through this common hook dir — running
# that battery on every board-truthkeeping write would stall the fleet's
# board traffic for minutes at a time. Board-only commits pay the board
# guard alone (mixing + stale-base row-drop, both fail-closed); every
# changeset that touches code still runs the full chain below.
#
# gitreins guard is the primary Tier-1 signal (secrets + build + tests);
# scripts/lint-guard.sh is the ADDITIVE safety net for the lint class the
# gitreins scaffold check cannot see — the divergence that bit d775344a
# (prealloc finding only caught by CI, fixed foreman-direct in 86da1e92).
# Set LINT_GUARD_SKIP=1 to bypass the lint check (unchanged; a code commit
# with LINT_GUARD_SKIP=1 still runs gitreins guard).

set -u

REPO_ROOT="$(git rev-parse --show-toplevel)"

# 0. board-commit-guard.sh (SCHED-GAP-1699 + RELEASE-046) — ALWAYS enforced,
#    fail-closed, BEFORE the gitreins-config early exit (a checkout without
#    gitreins must still refuse board-mixing and row-dropping commits).
if [ ! -x "$REPO_ROOT/scripts/board-commit-guard.sh" ]; then
    echo "pre-commit (SCHED-GAP-1699): board-commit-guard.sh missing or not executable at $REPO_ROOT/scripts/ — refusing (fail-closed)" >&2
    exit 2
fi
"$REPO_ROOT/scripts/board-commit-guard.sh"
BOARD_RC=$?
if [ "$BOARD_RC" -ne 0 ]; then
    exit "$BOARD_RC"
fi

# RELEASE-046: board-only changeset? (>=1 staged path and every staged path
# under .coding-hermes/ — exactly the class the board guard just admitted.)
STAGED_ALL="$(git diff --cached --name-only 2>/dev/null || true)"
BOARD_ONLY=0
if [ -n "$STAGED_ALL" ]; then
    BOARD_ONLY=1
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        case "$p" in
            .coding-hermes/*) ;;
            *) BOARD_ONLY=0; break ;;
        esac
    done <<<"$STAGED_ALL"
fi

if [ "$BOARD_ONLY" -ne 1 ]; then
    if [ ! -f "$REPO_ROOT/.gitreins/config.yaml" ]; then
        exit 0
    fi

    cd "$REPO_ROOT"

    # 1. gitreins guard — secrets, build, tests (scaffold lint). Heavy
    #    (900 s class): skipped for board-only changesets (see above).
    gitreins guard
    GUARD_RC=$?
    if [ "$GUARD_RC" -ne 0 ]; then
        exit "$GUARD_RC"
    fi

    # 2. lint-guard.sh — real golangci-lint on staged Go files. LINT_GUARD_SKIP=1
    #    bypasses this (board-only / docs-only commits). Exit non-zero blocks
    #    the commit.
    if [ "${LINT_GUARD_SKIP:-0}" != "1" ]; then
        if [ -x "$REPO_ROOT/scripts/lint-guard.sh" ]; then
            "$REPO_ROOT/scripts/lint-guard.sh"
            LINT_RC=$?
            if [ "$LINT_RC" -ne 0 ]; then
                exit "$LINT_RC"
            fi
        fi
    fi

    # 3. policy-script-deploy-hash-guard.sh (SCHED-PERF-006) — deploy-hash
    #    tripwire for the live fleet-cooldown-policy.py. Sits AFTER the gitreins
    #    guard (primary Tier-1 signal) and AFTER lint-guard.sh. Skips when
    #    LINT_GUARD_SKIP=1 (board-only / docs-only commits share the skip
    #    path) and when no Go files are staged (same lineage as lint-guard's
    #    staged-Go-files logic — the guard adds nothing to a shell/README
    #    commit). Exit non-zero blocks the commit.
    if [ "${LINT_GUARD_SKIP:-0}" != "1" ]; then
        STAGED_GO_CFG="$(git diff --cached --name-only --diff-filter=ACMR -- '*.go' 2>/dev/null || true)"
        if [ -n "$STAGED_GO_CFG" ] && [ -x "$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh" ]; then
            "$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh"
            POLICY_RC=$?
            if [ "$POLICY_RC" -ne 0 ]; then
                exit "$POLICY_RC"
            fi
        fi
    fi
fi

exit 0
HOOK

chmod +x "$HOOK"
echo "install-hooks: wrote $HOOK"
echo "install-hooks: run scripts/install-hooks.sh --check to verify"
