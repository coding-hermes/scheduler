#!/bin/sh
# lane-lib.sh — the ONE place the lane→branch rule lives (S14).
#
# A lane name is `<project>-<role>`, the same split lanehook.RoleFor does in Go
# (internal/lanehook/role.go). The branch is lane/<project>/<role>.
#
# The rule existed twice, and both copies were wrong the same way: they spelled
# the branch `lane/<lane>` with every `-` turned into `/`, so `code-factory-qa`
# became lane/code/factory/qa — the project `code-factory` was silently split
# into two path segments. Any lane whose PROJECT contains a dash drifted, and a
# reader looking for "the code-factory lanes" under lane/code-factory/ found
# nothing. Source this file; do not re-spell the rule.
#
# Usage:  . "$(dirname "$0")/lane-lib.sh"
#         BRANCH=$(lane_branch "$LANE" "$PROJECT" "$ROLE")   # empty = derive

# lane_role_suffixes mirrors lanehook.roleSuffixes. Keep the two lists equal;
# the Go side is canonical and internal/lanehook/role_test.go pins it.
lane_role_suffixes() {
  printf '%s\n' foreman qa ops pm sync releng dogfood docs readme review perf
}

# lane_derive <lane> -> prints "<project> <role>"
lane_derive() {
  _lane=$1
  _best=""
  for _r in $(lane_role_suffixes); do
    case "$_lane" in
      *-"$_r")
        if [ "${#_r}" -gt "${#_best}" ]; then _best="$_r"; fi
        ;;
    esac
  done
  if [ -n "$_best" ]; then
    _proj=${_lane%-$_best}
    if [ -n "$_proj" ]; then
      printf '%s %s\n' "$_proj" "$_best"
      return 0
    fi
  fi
  # no role suffix, or the lane IS the role: the lane is its own project
  printf '%s %s\n' "$_lane" primary
}

# lane_branch <lane> [project] [role] -> prints "lane/<project>/<role>"
lane_branch() {
  _lane=$1; _proj=${2:-}; _role=${3:-}
  if [ -z "$_proj" ] || [ -z "$_role" ]; then
    set -- $(lane_derive "$_lane")
    _proj=$1; _role=$2
  fi
  case "$_proj$_role" in
    */*) return 1 ;;
  esac
  case "$_proj" in
    ""|-*) return 1 ;;
  esac
  printf 'lane/%s/%s\n' "$_proj" "$_role"
}
