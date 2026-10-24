#!/bin/bash
# fleet-infra-sweep.sh — post-tick consolidation of fleet-infra findings
# (SCHED-GAP-128).
#
# WHY: the qa/dogfood/PM lanes report on the SHARED substrate and each one
# files whatever it saw on the board of the project it happened to poke. The
# routing path (ops/fleet-infra-findings.py) makes the NEXT filing land on the
# shared-infra board once, but a lane that has not been rewired yet still
# writes the old per-project copies. This sweep is the convergence step: after
# a tick it collapses any copies into ONE FLEET-INFRA row per pattern, with a
# cross_ref naming every project that saw it. It is idempotent, so running it
# on a quiet fleet costs one read pass and writes nothing.
#
# CONTRACT for tick callers: ALWAYS exits 0 and prints ONE line. A consolidation
# fault must never fail the tick that produced the findings — the rows on disk
# are still readable, and the next sweep retries.
#
#   ops/fleet-infra-sweep.sh                         # report only (default)
#   ops/fleet-infra-sweep.sh --apply --cross-project # explicitly mutate boards
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY="${FLEET_INFRA_PYTHON:-python3}"

# Keep the tick-caller contract: the Python command gets a final --sweep and
# this wrapper always emits one line and exits zero.  In particular, do not
# manufacture --apply here; mutations need an explicit operator argument.
"$PY" "$HERE/collapse-fleet-infra-findings.py" "$@" --sweep 2>&1 | tail -1
exit 0
