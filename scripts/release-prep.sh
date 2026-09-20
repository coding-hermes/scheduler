#!/usr/bin/env bash
#
# release-prep.sh — promote CHANGELOG.md's [Unreleased] section to a release
# heading for the version you are about to tag.
#
# WHAT IT DOES
#   1. Validates the version shape and every precondition (refusing by name).
#   2. Promotes the [Unreleased] body into `## [X.Y.Z] — <today>`.
#   3. Leaves a fresh, empty `## [Unreleased] — <today>` above it.
#   4. Prints the exact next commands (gates, commit, tag, push) and runs NONE
#      of them.
#
# WHAT IT DELIBERATELY DOES NOT DO
#   It never creates or pushes a tag, never commits, never pushes. A script
#   that tags and pushes when invoked is how a wrong tag gets published (see
#   docs/releases.md §3). `make tag VERSION=vX.Y.Z` is the separate, explicitly
#   typed command for the tag step.
#
# REFUSALS (exit 2, one REASON= line on stderr)
#   BAD_USAGE, BAD_VERSION_SHAPE, NOT_A_REPO, MISSING_CHANGELOG,
#   DIRTY_TREE, NOT_ON_RELEASE_COMMIT, MISSING_UNRELEASED,
#   VERSION_HEADING_EXISTS, TAG_ALREADY_EXISTS, EMPTY_UNRELEASED
#
# Exit 0 = CHANGELOG.md rewritten (or, with --check, it already is correct).
#
set -euo pipefail

REPO_ROOT=""
CHECK_ONLY=0
DRY_RUN=0
VERSION=""

usage() {
  cat <<'EOF'
usage: scripts/release-prep.sh [--check] [--dry-run] [--repo DIR] vX.Y.Z

  vX.Y.Z        the tag you are about to push (annotated-tag shape; the 'v' is
                required because release.yaml matches tags as 'v*')

  --check       do not write; exit 0 if CHANGELOG.md already names VERSION,
                non-zero (and print the reason) otherwise. CI-friendly.
  --dry-run     run every precondition check, print the diff that would be
                written, write nothing.
  --repo DIR    operate on DIR instead of the repo containing this script.
  -h, --help    this text

The script refuses rather than guessing: every precondition failure exits 2
with a REASON=<code> line naming what is wrong.
EOF
}

die() { # $1 = REASON code, rest = detail
  local reason="$1"; shift
  printf 'REASON=%s\n' "$reason" >&2
  printf 'release-prep: %s\n' "$*" >&2
  exit 2
}

# ---------- argument parsing ----------
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --check)   CHECK_ONLY=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --repo)    [ $# -ge 2 ] || die BAD_USAGE "--repo needs a directory"
               REPO_ROOT="$2"; shift 2 ;;
    -*)        die BAD_USAGE "unknown option '$1' (see --help)" ;;
    *)         if [ -n "$VERSION" ]; then
                 die BAD_USAGE "more than one version given: '$VERSION' and '$1'"
               fi
               VERSION="$1"; shift ;;
  esac
done

if [ -z "$VERSION" ]; then
  die BAD_USAGE "no version given. usage: scripts/release-prep.sh vX.Y.Z"
fi

# ---------- version shape ----------
# vMAJOR.MINOR.PATCH only. annotated tags in this repo are all plain semver and
# release.yaml derives the version string from the tag verbatim, so anything
# carrying a suffix (v1.4.0-rc1) would ship that suffix into --version, health,
# the OpenAPI spec and MCP. Reject it here rather than discover it in the field.
case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) die BAD_VERSION_SHAPE "'$VERSION' is not vMAJOR.MINOR.PATCH (e.g. v1.4.0)" ;;
esac
if ! printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  die BAD_VERSION_SHAPE "'$VERSION' is not vMAJOR.MINOR.PATCH with numeric components (e.g. v1.4.0)"
fi
SEMVER="${VERSION#v}"

# ---------- locate the repo ----------
if [ -z "$REPO_ROOT" ]; then
  REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi
[ -d "$REPO_ROOT" ] || die NOT_A_REPO "no such directory: $REPO_ROOT"
cd "$REPO_ROOT"

git rev-parse --is-inside-work-tree >/dev/null 2>&1 \
  || die NOT_A_REPO "$REPO_ROOT is not a git work tree"

CHANGELOG="CHANGELOG.md"
[ -f "$CHANGELOG" ] || die MISSING_CHANGELOG "no $CHANGELOG in $REPO_ROOT"

# ---------- preconditions ----------
# 1. clean tree
if [ -n "$(git status --porcelain)" ]; then
  die DIRTY_TREE "working tree is not clean; commit or clean these first:
$(git status --porcelain)"
fi

# 2. HEAD == origin/main (the release commit must be what CI ran on)
if ! git rev-parse --verify -q origin/main >/dev/null; then
  die NOT_ON_RELEASE_COMMIT "origin/main is not known locally; run: git fetch origin main"
fi
HEAD_SHA="$(git rev-parse HEAD)"
MAIN_SHA="$(git rev-parse origin/main)"
[ "$HEAD_SHA" = "$MAIN_SHA" ] \
  || die NOT_ON_RELEASE_COMMIT "HEAD ($HEAD_SHA) != origin/main ($MAIN_SHA); releases are cut from origin/main"

# 3. the tag must be free
if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
  die TAG_ALREADY_EXISTS "tag $VERSION already exists; a version is spent once tagged"
fi

# 4. [Unreleased] must exist exactly once
UNRELEASED_COUNT="$(grep -c '^## \[Unreleased\]' "$CHANGELOG" || true)"
if [ "$UNRELEASED_COUNT" -eq 0 ]; then
  die MISSING_UNRELEASED "no '## [Unreleased]' heading in $CHANGELOG"
fi
if [ "$UNRELEASED_COUNT" -gt 1 ]; then
  die MISSING_UNRELEASED "$UNRELEASED_COUNT '## [Unreleased]' headings in $CHANGELOG; expected exactly 1"
fi

# 5. the target heading must NOT already exist (idempotency guard)
if grep -qF "## [$SEMVER]" "$CHANGELOG"; then
  if [ "$CHECK_ONLY" -eq 1 ]; then
    echo "release-prep: $CHANGELOG already names $SEMVER — nothing to do"
    exit 0
  fi
  die VERSION_HEADING_EXISTS "heading '## [$SEMVER]' is already in $CHANGELOG; refusing to duplicate it"
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  die VERSION_HEADING_EXISTS "$CHANGELOG does not yet name $SEMVER; run make release-prep VERSION=$VERSION"
fi

# 6. [Unreleased] must actually have content
UNRELEASED_LINE="$(grep -n '^## \[Unreleased\]' "$CHANGELOG" | head -1 | cut -d: -f1)"
NEXT_HEADING_LINE="$(awk -v start="$UNRELEASED_LINE" \
  'NR>start && /^## \[/ {print NR; exit}' "$CHANGELOG")"
if [ -z "$NEXT_HEADING_LINE" ]; then
  NEXT_HEADING_LINE="$(( $(wc -l < "$CHANGELOG") + 1 ))"
fi
BODY="$(sed -n "$((UNRELEASED_LINE + 1)),$((NEXT_HEADING_LINE - 1))p" "$CHANGELOG")"
if [ -z "$(printf '%s' "$BODY" | tr -d '[:space:]')" ]; then
  die EMPTY_UNRELEASED "[Unreleased] has no content; a release heading with nothing under it is what this tool exists to prevent"
fi

# ---------- rewrite ----------
TODAY="$(date -u +%Y-%m-%d)"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

# Everything above the [Unreleased] heading is the preamble (title + intro).
{
  sed -n "1,$((UNRELEASED_LINE - 1))p" "$CHANGELOG"      # preamble (incl. blank line)
  printf '## [Unreleased] — %s\n\n' "$TODAY"              # fresh empty section
  printf '## [%s] — %s\n' "$SEMVER" "$TODAY"              # promoted heading
  printf '%s\n\n' "$BODY"                                 # its body + section separator
  sed -n "$((NEXT_HEADING_LINE)),\$p" "$CHANGELOG"        # every older section
} > "$TMP"

# ---------- validate the RESULT before installing it ----------
# Written to $TMP first so a failed check leaves CHANGELOG.md untouched: a
# half-applied rollover is worse than a refusal.
grep -qF "## [$SEMVER] — $TODAY" "$TMP" \
  || die MISSING_UNRELEASED "post-write check failed: heading '## [$SEMVER] — $TODAY' is absent from the result"
[ "$(grep -c '^## \[Unreleased\]' "$TMP")" -eq 1 ] \
  || die MISSING_UNRELEASED "post-write check failed: expected exactly one '## [Unreleased]' heading in the result"
# Every NON-[Unreleased] heading that existed before must still exist. The
# [Unreleased] heading itself is deliberately excluded: its date is rewritten by
# design, so comparing it verbatim would fail every correct run.
DROPPED=""
while IFS= read -r h; do
  [ -z "$h" ] && continue
  case "$h" in "## [Unreleased]"*) continue ;; esac
  grep -qF "$h" "$TMP" || DROPPED="$DROPPED
$h"
done < <(git show "HEAD:$CHANGELOG" 2>/dev/null | grep '^## \[' || true)
[ -z "$DROPPED" ] \
  || die MISSING_UNRELEASED "post-write check failed: these headings were dropped:$DROPPED"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "release-prep: dry run for $VERSION — nothing written. Diff that would be applied:"
  diff -u "$CHANGELOG" "$TMP" || true
  exit 0
fi

cat "$TMP" > "$CHANGELOG"

# ---------- next steps (printed, never run) ----------
cat <<EOF

release-prep: $CHANGELOG now names $SEMVER.

Next commands — run them in order; this script performs none of them:

  1. review the rollover
       git diff $CHANGELOG

  2. gates (fleet host: add -p 1)
       make test

  3. commit the rollover and push it, then wait for CI on that exact commit
       git add $CHANGELOG
       git commit -m "chore(release): $VERSION"
       git push origin main
       gh run list --branch main --limit 5

  4. tag and push — THIS is the release (annotated tag, one positional message)
       git tag -a $VERSION -m 'Release $VERSION'
       git push origin $VERSION

  5. verify the release
       gh run list --workflow=release.yaml --limit 5
       gh release view $VERSION

Full procedure: docs/releases.md
EOF
