#!/usr/bin/env bash
#
# check-release-vcs-stamps.sh — RELEASE-037 release gate.
#
# WHY THIS EXISTS
#   release.yaml used to build into ./dist INSIDE the checkout. Go's VCS
#   stamping reads the working tree, so the first binary was stamped clean
#   and every later build saw the earlier outputs as untracked files: 7 of
#   the 8 released assets shipped with `vcs.modified=true` and a
#   `v1.14.0+dirty` main-module version. Nothing in the release path
#   asserted the stamps, so the release still went green.
#
#   The durable fix is structural — the release build writes to
#   $RUNNER_TEMP/dist, never into the tree — and this gate is the
#   falsifiable half of it: it fails the release if ANY asset carries a
#   dirty stamp, whichever way the output directory regresses.
#
# WHAT IT CHECKS
#   For every one of the 8 assets in the release matrix
#   (schedulerd|migrate x linux|darwin x amd64|arm64):
#     1. the file exists in the artifact directory and is non-empty;
#     2. its build info carries a `vcs.modified=` record at all — a binary
#        built with -buildvcs=false would otherwise pass silently;
#     3. that record is `vcs.modified=false`;
#     4. no line of the build info carries the `+dirty` version suffix.
#
# MODES
#   check-release-vcs-stamps.sh [ARTIFACT_DIR]
#       ARTIFACT_DIR defaults to $DIST, then ./dist.
#
# EXIT CODES
#   0  all 8 assets present, stamped, and clean
#   1  at least one asset missing, unstamped, or dirty
#   2  precondition problem (no `go` on PATH, artifact dir missing)
#
set -uo pipefail

DIR="${1:-${DIST:-dist}}"

if ! command -v go >/dev/null 2>&1; then
  echo "GATE ERROR: go is not on PATH; cannot read build info" >&2
  exit 2
fi

if [ ! -d "$DIR" ]; then
  echo "GATE ERROR: artifact directory not found: $DIR" >&2
  exit 2
fi

total=0
clean=0
failed=0

report_fail() {
  printf 'FAIL  %-24s %s\n' "$1" "$2"
  failed=$((failed + 1))
}

for tool in schedulerd migrate; do
  for os in linux darwin; do
    for arch in amd64 arm64; do
      name="${tool}-${os}-${arch}"
      total=$((total + 1))
      path="${DIR}/${name}"

      if [ ! -f "$path" ] || [ ! -s "$path" ]; then
        report_fail "$name" "missing or empty in ${DIR}"
        continue
      fi

      if ! meta="$(go version -m "$path" 2>&1)"; then
        report_fail "$name" "go version -m failed: ${meta}"
        continue
      fi

      modified="$(printf '%s\n' "$meta" | awk '$1 == "build" && $2 ~ /^vcs\.modified=/ { sub(/^vcs\.modified=/, "", $2); print $2; exit }')"
      version="$(printf '%s\n' "$meta" | awk '$1 == "mod" { print $3; exit }')"

      if [ -z "$modified" ]; then
        report_fail "$name" "unstamped: no vcs.modified record (built outside version control, or -buildvcs=false)"
        continue
      fi

      if [ "$modified" != "false" ]; then
        report_fail "$name" "vcs.modified=${modified} version=${version:-unknown}"
        continue
      fi

      if printf '%s\n' "$meta" | grep -q '+dirty'; then
        report_fail "$name" "version=${version:-unknown} carries the +dirty suffix"
        continue
      fi

      clean=$((clean + 1))
      printf 'PASS  %-24s vcs.modified=false version=%s\n' "$name" "${version:-unknown}"
    done
  done
done

echo "--------------------------------------------------------------------------"
if [ "$failed" -ne 0 ]; then
  echo "RESULT: FAIL — ${failed} of ${total} release assets are missing, unstamped, or dirty"
  echo "        Rebuild outside the checkout: release output must never land in the git tree."
  exit 1
fi

echo "RESULT: PASS — ${clean} of ${total} release assets stamped vcs.modified=false, no +dirty suffix"
exit 0
