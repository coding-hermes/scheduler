#!/usr/bin/env bash
#
# check-version-authority.sh — RELEASE-006 version-authority gate.
#
# ONE question, ONE implementation of the rule: does CHANGELOG.md name the
# version being tagged? The rule itself lives in scripts/release-prep.sh
# (--check mode); this wrapper is its CI-safe caller. It exists because
# release-prep's precondition ladder is designed for the OPERATOR's checkout
# (clean tree, HEAD == origin/main, tag not yet created) and a CI checkout
# violates all three: a tag push checks out a detached HEAD, fetches no
# origin/main ref, and the tag being released already exists. Running
# `release-prep.sh --check` directly on the runner would refuse with
# NOT_ON_RELEASE_COMMIT / TAG_ALREADY_EXISTS (or DIRTY_TREE on a developer
# box) before ever reaching the version assertion.
#
# HOW IT CONTROLS ITS INPUTS (the one design decision worth documenting):
# the wrapper copies CHANGELOG.md into a throwaway git repo (git init, one
# commit, refs/remotes/origin/main pointed at that commit, no tags, hooks
# disabled) and runs `release-prep.sh --check --repo <fixture>` against it.
# Every precondition-failure class is eliminated by construction, so the only
# reasons the child can report are the two content assertions this gate is
# for:
#   VERSION_HEADING_EXISTS (--check) -> CHANGELOG does not name the version
#   MISSING_UNRELEASED               -> [Unreleased] absent or not exactly once
# Any other child reason is a wrapper bug or a broken environment and is
# reported as CHILD_UNEXPECTED — never disguised as a version failure.
#
# An EMPTY [Unreleased] body deliberately passes. That is the healthy state
# immediately after `make release-prep` rolls the section over (fresh, empty
# [Unreleased] above the new release heading). release-prep's EMPTY_UNRELEASED
# refusal guards its WRITE path (never promote an empty section); it is
# unreachable in --check mode, and this gate must not false-fail the
# post-rollover tree.
#
# MODES
#   check-version-authority.sh [--repo DIR] vX.Y.Z
#       Tag mode (release.yaml): the CHANGELOG must name vX.Y.Z as a release
#       heading and carry exactly one [Unreleased] section.
#   check-version-authority.sh --structure-only [--repo DIR]
#       Push mode (ci.yaml): an ordinary push carries no version, so this arm
#       CANNOT delegate to release-prep — its rule is version-parameterized
#       and refuses to run without a version argument. The weaker structural
#       invariant is asserted directly: exactly one `## [Unreleased]` heading
#       and the newest release heading parses as `## [X.Y.Z]`. The two modes
#       share the failure convention, not the parsing: the version-assertion
#       rule keeps exactly one implementation, in release-prep.sh.
#
# EXIT CODES
#   0  the assertion holds (PASS line on stdout)
#   2  refusal: REASON=<code> plus a detail line on stderr
#
# REFUSAL CODES
#   BAD_USAGE                        wrapper misused (no/extra version, unknown flag)
#   BAD_VERSION_SHAPE                not vX.Y.Z (surfaced verbatim from release-prep)
#   NOT_A_REPO                       --repo directory does not exist
#   MISSING_CHANGELOG                no CHANGELOG.md in the target repo
#   CHANGELOG_DOES_NOT_NAME_VERSION  tag mode: no '## [X.Y.Z]' release heading
#   CHANGELOG_UNRELEASED_INVALID     tag mode: [Unreleased] absent or not exactly once
#   UNRELEASED_HEADING_COUNT         structure mode: [Unreleased] absent or not exactly once
#   NEWEST_RELEASE_HEADING_MALFORMED structure mode: newest release heading not '## [X.Y.Z]'
#   CHILD_UNEXPECTED                 release-prep refused for a reason the fixture
#                                    makes impossible (wrapper bug / broken env)
#
# Environment-free by construction: git + coreutils only — no network, no
# database, no ~/.hermes state. Every git call is pointed at the fixture
# explicitly (-C), runs with hooks disabled, and strips GIT_DIR /
# GIT_WORK_TREE / GIT_INDEX_FILE so the surrounding CI job or fleet host
# cannot leak its own repo context into the fixture.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RELEASE_PREP="$SCRIPT_DIR/release-prep.sh"

REPO_ROOT=""
STRUCTURE_ONLY=0
VERSION=""

# git context of the SURROUNDING job must not leak into the fixture.
FIXTURE_GIT_ENV=(-u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES)

usage() {
  cat <<'EOF'
usage: scripts/check-version-authority.sh [--repo DIR] vX.Y.Z
       scripts/check-version-authority.sh --structure-only [--repo DIR]

  vX.Y.Z            the tag being released; CHANGELOG.md must name it as a
                    release heading ('## [X.Y.Z]') and carry exactly one
                    '[Unreleased]' section. (release.yaml tag gate.)

  --structure-only  an ordinary push has no version: assert structural
                    CHANGELOG hygiene instead — exactly one '## [Unreleased]'
                    heading and the newest release heading parses as
                    '## [X.Y.Z]'. (ci.yaml.)

  --repo DIR        check DIR's CHANGELOG.md (default: the repo containing
                    this script). The version assertion always runs on a
                    throwaway fixture copy, never against the target tree's
                    git state.
  -h, --help        this text

Exit 0 = the assertion holds. Exit 2 = refusal, with REASON=<code> on stderr:
BAD_USAGE, BAD_VERSION_SHAPE, NOT_A_REPO, MISSING_CHANGELOG,
CHANGELOG_DOES_NOT_NAME_VERSION, CHANGELOG_UNRELEASED_INVALID,
UNRELEASED_HEADING_COUNT, NEWEST_RELEASE_HEADING_MALFORMED, CHILD_UNEXPECTED.
The version rule itself is implemented once, in scripts/release-prep.sh.
EOF
}

die() { # $1 = REASON code, rest = detail
  local reason="$1"; shift
  printf 'REASON=%s\n' "$reason" >&2
  printf 'check-version-authority: %s\n' "$*" >&2
  exit 2
}

# ---------- argument parsing ----------
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --structure-only) STRUCTURE_ONLY=1; shift ;;
    --repo)    [ $# -ge 2 ] || die BAD_USAGE "--repo needs a directory"
               REPO_ROOT="$2"; shift 2 ;;
    -*)        die BAD_USAGE "unknown option '$1' (see --help)" ;;
    *)         if [ "$STRUCTURE_ONLY" -eq 1 ]; then
                 die BAD_USAGE "--structure-only takes no version argument (got '$1')"
               fi
               if [ -n "$VERSION" ]; then
                 die BAD_USAGE "more than one version given: '$VERSION' and '$1'"
               fi
               VERSION="$1"; shift ;;
  esac
done

if [ -z "$REPO_ROOT" ]; then
  REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
fi
[ -d "$REPO_ROOT" ] || die NOT_A_REPO "no such directory: $REPO_ROOT"

CHANGELOG="$REPO_ROOT/CHANGELOG.md"
[ -f "$CHANGELOG" ] || die MISSING_CHANGELOG "no CHANGELOG.md in $REPO_ROOT"

if [ "$STRUCTURE_ONLY" -eq 1 ]; then
  # ---- structure mode: version-free hygiene, asserted directly -------------
  # Hard reason this does not go through release-prep: its rule requires a
  # version argument (BAD_USAGE otherwise) and asserts version-specific facts;
  # an ordinary push has none. This arm shares the failure convention above,
  # not the parsing.
  UNRELEASED_COUNT="$(grep -c '^## \[Unreleased\]' "$CHANGELOG" || true)"
  if [ "$UNRELEASED_COUNT" -ne 1 ]; then
    die UNRELEASED_HEADING_COUNT "$UNRELEASED_COUNT '## [Unreleased]' headings in $CHANGELOG; expected exactly 1"
  fi
  # The newest release heading is the FIRST '## ' heading after the
  # [Unreleased] section — whatever its shape. Filtering to bracket-shaped
  # headings here would silently skip a malformed newest heading ('## WIP
  # stuff') and pass a structurally broken CHANGELOG.
  UNRELEASED_LINE="$(grep -n '^## \[Unreleased\]' "$CHANGELOG" | head -n 1 | cut -d: -f1)"
  NEWEST_HEADING="$(awk -v start="$UNRELEASED_LINE" 'NR>start && /^## / {print; exit}' "$CHANGELOG")"
  if [ -z "$NEWEST_HEADING" ] || ! printf '%s' "$NEWEST_HEADING" | grep -qE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]'; then
    die NEWEST_RELEASE_HEADING_MALFORMED "newest release heading '${NEWEST_HEADING:-<none>}' does not parse as '## [X.Y.Z]' in $CHANGELOG"
  fi
  echo "version-authority: PASS (structure) — exactly one [Unreleased]; newest release heading '$NEWEST_HEADING'"
  exit 0
fi

# ---- tag mode: delegate the assertion to release-prep --check ---------------
if [ -z "$VERSION" ]; then
  die BAD_USAGE "no version given. usage: scripts/check-version-authority.sh vX.Y.Z"
fi

FIXTURE="$(mktemp -d)"
trap 'rm -rf "$FIXTURE"' EXIT
cp "$CHANGELOG" "$FIXTURE/CHANGELOG.md"

if ! env "${FIXTURE_GIT_ENV[@]}" git -C "$FIXTURE" init -q >/dev/null 2>&1; then
  die CHILD_UNEXPECTED "git init failed in fixture $FIXTURE"
fi
if ! env "${FIXTURE_GIT_ENV[@]}" git -C "$FIXTURE" \
      -c user.name=check-version-authority -c user.email=noreply@example.invalid \
      -c commit.gpgsign=false -c core.hooksPath=/dev/null \
      add CHANGELOG.md >/dev/null 2>&1; then
  die CHILD_UNEXPECTED "git add failed in fixture $FIXTURE"
fi
if ! env "${FIXTURE_GIT_ENV[@]}" git -C "$FIXTURE" \
      -c user.name=check-version-authority -c user.email=noreply@example.invalid \
      -c commit.gpgsign=false -c core.hooksPath=/dev/null \
      commit -q -m "fixture" >/dev/null 2>&1; then
  die CHILD_UNEXPECTED "fixture commit failed in $FIXTURE"
fi
FIXTURE_HEAD="$(env "${FIXTURE_GIT_ENV[@]}" git -C "$FIXTURE" rev-parse HEAD)" \
  || die CHILD_UNEXPECTED "cannot resolve fixture HEAD in $FIXTURE"
env "${FIXTURE_GIT_ENV[@]}" git -C "$FIXTURE" \
    update-ref refs/remotes/origin/main "$FIXTURE_HEAD" \
  || die CHILD_UNEXPECTED "cannot set origin/main in fixture $FIXTURE"

# The child's precondition ladder is now satisfied by construction (clean
# tree, HEAD == origin/main, tag-free fixture), so its REASON= is one of the
# two content assertions — or a wrapper bug, surfaced honestly below.
child_rc=0
child_err="$(env "${FIXTURE_GIT_ENV[@]}" bash "$RELEASE_PREP" \
               --check --repo "$FIXTURE" "$VERSION" 2>&1 1>/dev/null)" || child_rc=$?
child_reason="$(printf '%s\n' "$child_err" | grep -m1 '^REASON=' | cut -d= -f2 || true)"
# The child prefixes its detail lines with "release-prep: " — strip it so the
# wrapper's own line does not stutter.
child_detail="$(printf '%s\n' "$child_err" | grep -v '^REASON=' | sed 's/^release-prep: //' | head -n 2 | tr '\n' ' ' || true)"

case "$child_rc" in
  0)
    echo "version-authority: PASS — $CHANGELOG names $VERSION; [Unreleased] present exactly once"
    exit 0
    ;;
  2)
    case "$child_reason" in
      VERSION_HEADING_EXISTS)
        die CHANGELOG_DOES_NOT_NAME_VERSION \
            "$CHANGELOG does not name $VERSION as a release heading (release-prep: ${child_detail:-no detail})" ;;
      MISSING_UNRELEASED)
        die CHANGELOG_UNRELEASED_INVALID \
            "[Unreleased] section absent or not exactly once in $CHANGELOG (release-prep: ${child_detail:-no detail})" ;;
      BAD_VERSION_SHAPE)
        die BAD_VERSION_SHAPE "${child_detail:-'$VERSION' is not vMAJOR.MINOR.PATCH}" ;;
      *)
        die CHILD_UNEXPECTED \
            "release-prep --check refused with REASON=${child_reason:-<none>} — this class should be impossible against the fixture; investigate the wrapper (detail: ${child_detail:-none})" ;;
    esac
    ;;
  *)
    die CHILD_UNEXPECTED "release-prep --check exited rc=$child_rc (expected 0 or 2); detail: ${child_detail:-<none>}"
    ;;
esac
