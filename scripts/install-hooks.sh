#!/usr/bin/env bash
#
# install-hooks.sh — install the gitreins + lint-guard pre-commit hook
# (SCHED-GAP-221) into .git/hooks/pre-commit.
#
# Idempotent: safe to re-run. The hook always invokes gitreins guard first
# (the primary Tier-1 signal) and then scripts/lint-guard.sh (the additive
# safety net for the lint class the gitreins scaffold check cannot see).
# If either fails the commit is blocked.
#
# Use:
#   ./scripts/install-hooks.sh          # install
#   ./scripts/install-hooks.sh --check  # verify without writing
#
# Why this is a separate script (and not just editing the hook in place):
# git hooks live in .git/hooks/, which is not version-controlled. This
# script is the one-line ship path that lets a fresh clone reproduce the
# same hook layout the gitreins + lint-guard chain was designed around.

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

if [ "${1:-}" = "--check" ]; then
    if [ -f "$HOOK" ] && grep -q "$MARKER" "$HOOK"; then
        echo "install-hooks: OK (lint-guard pre-commit hook present)"
        exit 0
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
# GitReins + lint-guard pre-commit hook (SCHED-GAP-221).
#
# Order matters: gitreins guard is the primary Tier-1 signal (secrets +
# build + tests). scripts/lint-guard.sh is the ADDITIVE safety net for
# the lint class the gitreins scaffold check cannot see — the divergence
# that bit d775344a (prealloc finding only caught by CI, fixed
# foreman-direct in 86da1e92). Set LINT_GUARD_SKIP=1 to bypass the
# lint check on board-only / docs-only commits.

set -u

REPO_ROOT="$(git rev-parse --show-toplevel)"
if [ ! -f "$REPO_ROOT/.gitreins/config.yaml" ]; then
    exit 0
fi

cd "$REPO_ROOT"

# 1. gitreins guard — secrets, build, tests (scaffold lint).
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

exit 0
HOOK

chmod +x "$HOOK"
echo "install-hooks: wrote $HOOK"
echo "install-hooks: run scripts/install-hooks.sh --check to verify"
