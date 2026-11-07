#!/usr/bin/env bash
#
# release-claim.sh — the single-writer claim for board-releasing actions
# (RELEASE-048, arm 3).
#
# A release action in this repo is anything that closes a RELEASE-* board row
# or pushes a release tag. On 2026-11-07 two writers did both independently:
# the releng lane landed a full v1.15.0/v1.15.1 cut while a sibling foreman
# tick closed the same RELEASE-047 row — a double release writer, discovered
# only because the duplicate commit showed up in git log. The claim marker
# makes the second writer REFUSED instead of discovered.
#
# WHAT IT DOES
#   claim <target> <owner>    Record the caller as the writer for target.
#                             Refuses (exit 3) when a DIFFERENT owner already
#                             holds the claim; the same owner re-claiming is
#                             idempotent (refreshes claimed_at, exit 0).
#   status [target]           Print the current claim (or "no claim"). Exit 0.
#   release <target> <owner>  Release the claim. Refuses (exit 3) when the
#                             caller is not the recorded owner or the recorded
#                             target differs; exit 4 when there is no claim.
#
# TARGET SHAPE
#   A RELEASE-* row id ("RELEASE-047") or a tag name ("tag:v1.15.1" — the
#   tag: prefix is how the Makefile's `tag` target claims; it keeps a row id
#   and a tag in the same namespace without colliding).
#
# THE MARKER
#   <repo root>/.coding-hermes/board/release-claim.json
#     {"target": "...", "owner": "...", "claimed_at": "<RFC3339>"}
#   Runtime state, deliberately gitignored (a marker present in the tree
#   would fail `make tag`'s clean-tree precondition). Writers share one
#   marker: a repo cuts one release at a time, so one claim file is the
#   whole protocol — flock makes check-and-set atomic against concurrent
#   writers.
#
# EXIT CODES
#   0  success
#   2  usage error / not a repo / bad target or owner
#   3  CONFLICTING CLAIM — another owner holds the target (the double-writer
#      refusal; this is the code every caller must treat as "stand down")
#   4  no claim to release / not yours to release
#
# Stale claims are NOT auto-expired on purpose: a crashed cutter's claim is
# real evidence someone was mid-release. The escape hatches are `release` by
# the recorded owner, or deleting the marker file by hand after checking
# `status` — a human decision, not a timeout.
#
# set -euo pipefail

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage:
  release-claim.sh claim <target> <owner>
  release-claim.sh status [target]
  release-claim.sh release <target> <owner>
exit codes: 0 ok | 2 usage | 3 conflicting claim | 4 no claim / not owner
USAGE
}

die() { # die <exit-code> <REASON=...> <message>
  local code="$1" reason="$2" msg="$3"
  printf 'REFUSED: REASON=%s %s\n' "$reason" "$msg" >&2
  exit "$code"
}

need_repo_root() {
  local root
  root="$(git rev-parse --show-toplevel 2>/dev/null)" ||
    die 2 NOT_A_REPO "release-claim.sh must run inside the scheduler repo"
  printf '%s' "$root"
}

marker_path() {
  local root="$1"
  printf '%s/.coding-hermes/board/release-claim.json' "$root"
}

lock_path() {
  local root="$1"
  printf '%s/.coding-hermes/board/release-claim.json.lock' "$root"
}

# validate_token rejects anything that could break the JSON or the shell:
# the marker is written with printf, so quotes, backslashes and control
# characters in target/owner are refused by name (BAD_TARGET/BAD_OWNER).
validate_token() {
  local kind="$1" value="$2" name="$3"
  [[ -n "$value" ]] || die 2 "BAD_${kind}" "empty ${name} argument"
  [[ "$value" =~ ^[A-Za-z0-9._:@/#+-]+$ ]] ||
    die 2 "BAD_${kind}" "${name} must match [A-Za-z0-9._:@/#+-]+ (got unprintable/quote characters)"
}

read_field() { # read_field <marker> <field> — prints the field's value or ""
  local marker="$1" field="$2"
  [[ -f "$marker" ]] || { printf ''; return; }
  sed -n "s/.*\"$field\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$marker" | head -n 1
}

cmd="${1:-}"
[[ -n "$cmd" ]] || { usage; exit 2; }
shift

ROOT="$(need_repo_root)"
MARKER="$(marker_path "$ROOT")"
LOCK="$(lock_path "$ROOT")"

case "$cmd" in
  status)
    if [[ ! -f "$MARKER" ]]; then
      echo "no claim"
      exit 0
    fi
    cat "$MARKER"
    if [[ $# -ge 1 ]]; then
      held="$(read_field "$MARKER" target)"
      [[ "$held" == "$1" ]] || printf 'note: the held claim targets %s, not %s\n' "$held" "$1" >&2
    fi
    exit 0
    ;;

  claim|release)
    [[ $# -ge 2 ]] || { usage; die 2 BAD_USAGE "claim/release need <target> <owner>"; }
    target="$1"
    owner="$2"
    validate_token TARGET "$target" target
    validate_token OWNER "$owner" owner
    ;;

  *)
    usage
    die 2 BAD_USAGE "unknown subcommand $cmd"
    ;;
esac

# One flock hold covers check-and-set (claim) and check-and-delete (release):
# two concurrent writers can never both observe "unclaimed".
exec 9>"$LOCK"
flock 9

held_target="$(read_field "$MARKER" target)"
held_owner="$(read_field "$MARKER" owner)"

if [[ "$cmd" == "claim" ]]; then
  if [[ -n "$held_target" && "$held_target" == "$target" && "$held_owner" != "$owner" ]]; then
    printf 'REFUSED: release claim conflict on %s: held by owner %s (claimed_at %s); writer %s must stand down (RELEASE-048 arm 3)\n' \
      "$target" "$held_owner" "$(read_field "$MARKER" claimed_at)" "$owner" >&2
    exit 3
  fi
  if [[ -n "$held_target" && "$held_target" != "$target" ]]; then
    printf 'REFUSED: release claim conflict: %s is held by owner %s (claimed_at %s); writer %s asked for %s — one release at a time (RELEASE-048 arm 3)\n' \
      "$held_target" "$held_owner" "$(read_field "$MARKER" claimed_at)" "$owner" "$target" >&2
    exit 3
  fi
  printf '{"target": "%s", "owner": "%s", "claimed_at": "%s"}\n' \
    "$target" "$owner" "$(date +%Y-%m-%dT%H:%M:%S%:z)" >"$MARKER"
  echo "claimed: target=$target owner=$owner"
  exit 0
fi

# cmd == release
if [[ -z "$held_target" ]]; then
  printf 'REFUSED: no release claim to release for %s (RELEASE-048 arm 3)\n' "$target" >&2
  exit 4
fi
if [[ "$held_target" != "$target" || "$held_owner" != "$owner" ]]; then
  printf 'REFUSED: release claim on %s is held by owner %s; %s may not release it (RELEASE-048 arm 3)\n' \
    "$held_target" "$held_owner" "$owner" >&2
  exit 3
fi
rm -f "$MARKER"
echo "released: target=$target owner=$owner"
exit 0
