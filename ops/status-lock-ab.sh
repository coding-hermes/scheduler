#!/usr/bin/env bash
# PERF-001-R2 — /api/v1/status eval-pass A/B driver.
#
# Answers the row's question against a RUNNING daemon that carries this
# change (any build with the lock_attribution surface):
#
#   arm A (idle)      — /api/v1/status p50/p99/max with no evaluate() pass
#                       window open (lock_attribution.eval_in_flight == false)
#   arm B (in-window) — /api/v1/status p50/p99/max sampled while a pass is
#                       inside its decision window (eval_in_flight == true),
#                       the pass triggered through the production entry point
#                       POST /api/v1/evaluate (operator credential required)
#
# …plus the block profile, armed by evaluate() itself (PERF-001-R2 step 1):
# GET /debug/pprof/block names what a blocked request actually blocked on.
#
# Reading the result: after PERF-001-R3 the read path holds no lock, so the
# contract is "arm B does NOT track the window" (eval_pass). If arm B's latency
# climbs to the window's order of magnitude, the read path has been re-coupled
# to the pass. Set BUDGET_S low and expect partial arms when the daemon is
# under heavy load: a request that blocks for a pass cannot be sampled quickly
# — that is itself the symptom, and the per-window numbers in `eval_pass` and
# `stages` are the reliable attribution in that state.
#
# Usage:
#   SCHEDULER_OPERATOR_TOKEN=… ops/status-lock-ab.sh [BASE_URL] [SAMPLES] [BUDGET_S]
#     BASE_URL  default http://127.0.0.1:9090
#     SAMPLES   samples per arm, default 20
#     BUDGET_S  wall-clock budget for the in-window arm, default 180
#
# Fleet state is untouched: the only write is one forced evaluation pass per
# iteration of the forcing loop — the operator's own documented lever.

set -uo pipefail

BASE="${1:-http://127.0.0.1:9090}"
N="${2:-20}"
BUDGET_S="${3:-180}"
TOKEN="${SCHEDULER_OPERATOR_TOKEN:-}"
OUT="${PERF001R2_OUT:-/tmp/perf001r2-ab}"

mkdir -p "$OUT"
command -v curl >/dev/null || { echo "missing curl" >&2; exit 2; }
command -v jq >/dev/null || { echo "missing jq" >&2; exit 2; }

status_json() { curl -s -m 300 "$BASE/api/v1/status"; }
inflight() { status_json | jq -r '.lock_attribution.eval_in_flight // "null"' 2>/dev/null; }

# one <file> — append a "<seconds>\t<in_flight>\t<code>" sample
one() {
  local t code
  read -r t code < <(curl -s -m 300 -o /dev/null -w '%{time_total} %{http_code}\n' "$BASE/api/v1/status")
  printf '%s\t%s\t%s\n' "$t" "$(inflight)" "$code" >>"$1"
  printf '.'
}

pct() { # pct <p> — reads numbers on stdin, prints milliseconds
  awk -v p="$1" '{a[NR]=$1} END{ if (NR==0) { print "n/a"; exit } i=int((NR-1)*p); if (i<0) i=0; printf "%.1f", a[i+1]*1000 }'
}

stats() { # stats <file> <label>
  local f="$1" label="$2" n
  n=$(wc -l <"$f" | tr -d ' ')
  if [[ "$n" == "0" ]]; then printf '%-8s no samples\n' "$label"; return; fi
  printf '%-8s n=%-3s p50=%9s ms  p99=%9s ms  max=%9s ms  [http codes: %s]\n' \
    "$label" "$n" \
    "$(cut -f1 "$f" | sort -n | pct 0.50)" \
    "$(cut -f1 "$f" | sort -n | pct 0.99)" \
    "$(cut -f1 "$f" | sort -n | pct 1.0)" \
    "$(cut -f3 "$f" | sort | uniq -c | tr '\n' ' ')"
}

echo "== PERF-001-R2 eval-pass A/B — $BASE (samples/arm=$N, in-window budget=${BUDGET_S}s)"
echo "-- attribution BEFORE"
status_json | jq -c '{block_profile, attribution: {eval_in_flight: .lock_attribution.eval_in_flight, eval_pass: .lock_attribution.eval_pass, read_path_lock_site: .lock_attribution.read_path_lock_site, stages: (.lock_attribution.stages // [])}}'

echo
echo "-- arm A: idle (no pass window open)"
: >"$OUT/armA.tsv"
for ((i = 0; i < N; i++)); do
  one "$OUT/armA.tsv"
done
printf '\n'
stats "$OUT/armA.tsv" "arm A"

echo
echo "-- arm B: in-window (a pass is inside its decision window)"
: >"$OUT/armB.tsv"
if [[ -z "$TOKEN" ]]; then
  echo "   SCHEDULER_OPERATOR_TOKEN unset — cannot force a pass; sampling whatever pass happens to run."
else
  curl -s -m 60 -X POST -H "X-Operator-Token: $TOKEN" "$BASE/api/v1/evaluate" | jq -c . || true
fi
( for _ in $(seq 1 60); do
    sleep 2
    [[ -n "$TOKEN" ]] && curl -s -m 60 -X POST -H "X-Operator-Token: $TOKEN" "$BASE/api/v1/evaluate" >/dev/null
  done ) &
forcer=$!
deadline=$((SECONDS + BUDGET_S))
until [[ "$(inflight)" == "true" ]] || (( SECONDS >= deadline )); do sleep 1; done
while (( SECONDS < deadline )) && [[ "$(inflight)" == "true" ]]; do
  one "$OUT/armB.tsv"
done
printf '\n'
kill "$forcer" 2>/dev/null
wait "$forcer" 2>/dev/null
stats "$OUT/armB.tsv" "arm B"

echo
echo "-- attribution AFTER (eval_pass is the window arm B must NOT track)"
status_json | jq -c '{attribution: {eval_in_flight: .lock_attribution.eval_in_flight, eval_pass: .lock_attribution.eval_pass, read_path_lock_site: .lock_attribution.read_path_lock_site, stages: (.lock_attribution.stages // [])}}'

echo
echo "-- block profile (armed by evaluate(); names what a blocked request waited on)"
( for _ in $(seq 1 300); do curl -s -m 300 -o /dev/null "$BASE/api/v1/status"; done ) &
driver=$!
curl -s -m 90 "$BASE/debug/pprof/block?seconds=20&debug=1" >"$OUT/block.txt"
kill "$driver" 2>/dev/null
wait "$driver" 2>/dev/null
if [[ -s "$OUT/block.txt" ]]; then
  echo "   raw profile: $OUT/block.txt ($(wc -c <"$OUT/block.txt" | tr -d ' ') bytes)"
  awk '
    /cycles\/second/ { next }
    /^[0-9]+ [0-9]+ @/ { if (cyc!="") printf "%14s %6s %s\n", cyc, cnt, first; cyc=$1; cnt=$2; first=""; next }
    /^#/ { if (first=="") first=$3; next }
    END { if (cyc!="") printf "%14s %6s %s\n", cyc, cnt, first }
  ' "$OUT/block.txt" | sort -rn | head -10 | sed 's/^/   /'
  printf '   frames mentioning status/ZeroSelectStats/RWMutex: %s\n' \
    "$(grep -c -E 'ZeroSelectStats|RWMutex|status' "$OUT/block.txt")"
else
  echo "   block profile EMPTY — check block_profile above (rate 0 = disabled)."
fi

echo
echo "== raw samples: $OUT/armA.tsv $OUT/armB.tsv"
