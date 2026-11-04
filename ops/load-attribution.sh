#!/usr/bin/env bash
# load-attribution.sh — read-only probe: WHO is holding the host's load, and is
# that within policy? (SCHED-GAP-16977)
#
# WHY. The scheduler's load gate is correct and blunt: when load1 crosses
# --load-gate-threshold, ADMIT defers every candidate and no tick runs. What the
# gate cannot say is WHY. Measured twice on 2026-11-03:
#
#   20:59  load1=33.12 vs gate=20.0 — ADMIT deferring all 677 candidates
#   21:20  load1=58.47 — three unrelated heavy jobs plus dockerd:
#            auger's 9-suite pytest gate      (cwd /home/kara/auger)
#            crier lane running go test ./... (cwd /home/kara/crier-wt-…)
#            xz -T4 rehearsal of a 42 GB dump (cwd /mnt/bulk/rehearsal-full)
#
# Each of those is legitimate work. Together they make the box unusable for
# ticks, and earlyoom is armed with a preference list that includes
# pytest/go/cargo/rustc — so the scheduler's own verification can be killed
# mid-run by the kernel. The missing piece is coordination ABOVE the gate, and
# coordination needs attribution first: this probe names the consumers, maps each
# to the lane that owns its working directory, and checks the host against the
# documented policy (AT MOST 1 heavy job per host, docs/load-attribution.md).
#
# READ-ONLY. It reads /proc, ps, and the scheduler's HTTP API. It writes nothing,
# signals nothing, and kills nothing.
#
# EXIT CODES
#   0  within policy (including a genuinely idle or merely-busy host)
#   1  POLICY VIOLATION: more concurrent heavy jobs than the limit, or the gate
#      is shut and at least one heavy job is named as the holder
#   2  cannot determine (no gate value available, or no process table readable)
#
# USAGE
#   ops/load-attribution.sh [--limit N] [--top K] [--api URL] [--gate F]
#                           [--json] [--ps-fixture FILE] [--proc-root DIR]
#
#   --ps-fixture FILE   read `pid pcpu etimes args` lines from FILE instead of
#                       running ps (the self-test's seam; also handy on a box
#                       whose ps is restricted)
#   --proc-root DIR     resolve cwd/cmdline under DIR instead of /proc
#
# The script never assumes a particular host: every threshold comes from the
# scheduler's own configuration unless explicitly overridden.

set -uo pipefail

API_DEFAULT="http://127.0.0.1:9090"
LIMIT=1
TOP=8
API="$API_DEFAULT"
GATE=""
JSON=0
PS_FIXTURE=""
PROC_ROOT="/proc"
HEAVY_CPU_MIN=25   # %CPU a process must show to count as heavy; below this it
                   # is not what is holding the box

# Anything in this list is a HEAVY job: the classes that saturate a core-count
# like this one. Deliberately command-shaped, not user-shaped — the probe must
# not care who started it.
HEAVY_RE='pytest|py\.test|go test|go build|cargo|rustc|make|cmake|ninja|xz |xz$|pigz|gzip|zstd|tar -|docker (build|buildx|compose)|buildkitd|uv sync|pip install|npm (ci|install|run build)|pnpm (install|run)|yarn install|tsc|vitest|jest|webpack|next build|esbuild'

while [ $# -gt 0 ]; do
  case "$1" in
    --limit) LIMIT="$2"; shift 2 ;;
    --top) TOP="$2"; shift 2 ;;
    --api) API="$2"; shift 2 ;;
    --gate) GATE="$2"; shift 2 ;;
    --json) JSON=1; shift ;;
    --ps-fixture) PS_FIXTURE="$2"; shift 2 ;;
    --proc-root) PROC_ROOT="$2"; shift 2 ;;
    -h|--help) sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "load-attribution: unknown argument: $1" >&2; exit 2 ;;
  esac
done

say() { [ "$JSON" -eq 1 ] || printf '%s\n' "$*"; }

# --- inputs ---------------------------------------------------------------

load1=""
if [ -r /proc/loadavg ]; then
  read -r l1 _ < /proc/loadavg
  load1="$l1"
fi

api_json() { # api_json <path> -> body on stdout, empty on any failure
  command -v curl >/dev/null 2>&1 || return 0
  curl -sS -m 8 "$API$1" 2>/dev/null || true
}

# The gate value comes from the scheduler itself, never from a constant here: a
# probe that hardcoded 20 would lie the moment the fleet is retuned.
if [ -z "$GATE" ]; then
  cfg="$(api_json /api/v1/config)"
  if [ -n "$cfg" ] && command -v python3 >/dev/null 2>&1; then
    GATE="$(printf '%s' "$cfg" | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: raise SystemExit
g=d.get("load_gate_threshold")
print("" if g is None else g)
' 2>/dev/null)"
  fi
fi

# --- process table --------------------------------------------------------

procs_tmp="$(mktemp "${TMPDIR:-/tmp}/load-attr.XXXXXX")"
trap 'rm -f "$procs_tmp"' EXIT

if [ -n "$PS_FIXTURE" ]; then
  [ -r "$PS_FIXTURE" ] || { echo "load-attribution: fixture not readable: $PS_FIXTURE" >&2; exit 2; }
  cp "$PS_FIXTURE" "$procs_tmp"
elif command -v ps >/dev/null 2>&1; then
  ps -eo pid=,pcpu=,etimes=,args= > "$procs_tmp" 2>/dev/null \
    || { echo "load-attribution: ps failed" >&2; exit 2; }
else
  echo "load-attribution: no ps on PATH and no --ps-fixture" >&2
  exit 2
fi

self_pid=$$
heavy_rows=""
heavy_count=0

while read -r pid pcpu etimes args; do
  [ -n "${pid:-}" ] || continue
  case "$pid" in *[!0-9]*) continue ;; esac
  [ "$pid" = "$self_pid" ] && continue
  case "$args" in
    *"load-attribution.sh"*) continue ;;   # never grade our own probe
    *grep*'HEAVY_RE'*) continue ;;
  esac
  # Integer compare on %CPU without bc/awk edge cases: strip the decimal part.
  cpu_int="${pcpu%%.*}"
  cpu_int="${cpu_int:-0}"
  [ "$cpu_int" -ge "$HEAVY_CPU_MIN" ] || continue
  printf '%s\n' "$args" | grep -Eq "$HEAVY_RE" || continue

  kind="other"
  case "$args" in
    *pytest*|*py.test*|*"go test"*|*vitest*|*jest*) kind="test" ;;
    *"go build"*|*cargo*|*rustc*|*tsc*|*webpack*|*esbuild*|*"next build"*) kind="build" ;;
    *"xz "*|*"xz -"*|*pigz*|*gzip*|*zstd*|*"tar -"*) kind="compress" ;;
    *"docker build"*|*buildx*|*"compose build"*) kind="image" ;;
    *"uv sync"*|*"pip install"*|*npm*ci*|*npm*install*|*pnpm*|*yarn*install*) kind="install" ;;
    *make*|*cmake*|*ninja*) kind="build" ;;
  esac

  cwd=""
  if [ -n "$PROC_ROOT" ] && [ -d "$PROC_ROOT/$pid" ]; then
    cwd="$(readlink "$PROC_ROOT/$pid/cwd" 2>/dev/null || true)"
  fi
  # Digest, not the full command line: log lines and probe output are
  # replayable surfaces, and a full argv can carry a token (the fleet has been
  # bitten by that before).
  digest=""
  if command -v sha256sum >/dev/null 2>&1; then
    if [ -r "$PROC_ROOT/$pid/cmdline" ]; then
      digest="$(sha256sum < "$PROC_ROOT/$pid/cmdline" 2>/dev/null | cut -c1-8)"
    else
      digest="$(printf '%s' "$args" | sha256sum | cut -c1-8)"
    fi
  fi

  heavy_rows="${heavy_rows}${pid}|${kind}|${cpu_int}|${etimes}|${cwd:-}|${digest}|${args}"$'\n'
  heavy_count=$((heavy_count + 1))
done < "$procs_tmp"

# --- lane attribution -----------------------------------------------------

# cwd -> lane: longest workdir prefix wins, so a worktree nested inside a
# project still attributes to its own project row rather than the parent.
projects="$(api_json /api/v1/projects)"
attr_ok=0
if [ -n "$projects" ] && command -v python3 >/dev/null 2>&1; then
  attr_ok=1
fi

attribute() { # attribute <cwd> -> lane name or empty
  [ "$attr_ok" -eq 1 ] || return 0
  [ -n "${1:-}" ] || return 0
  printf '%s' "$projects" | CWDP="$1" python3 -c '
import json, os, sys
cwd = os.environ["CWDP"].rstrip("/")
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit
rows = d.get("projects") or d.get("data") or []
best_wd, best_name = "", ""
for p in rows:
    wd = (p.get("workdir") or "").rstrip("/")
    if not wd:
        continue
    # Longest matching prefix wins, so a worktree nested inside a project
    # attributes to its own row instead of the parent.
    if (cwd == wd or cwd.startswith(wd + "/")) and len(wd) > len(best_wd):
        best_wd, best_name = wd, p.get("name", "")
print(best_name)
' 2>/dev/null
}

# --- report ---------------------------------------------------------------

gate_num="$GATE"
gate_state="unknown"
if [ -n "$gate_num" ] && [ -n "$load1" ] && command -v python3 >/dev/null 2>&1; then
  gate_state="$(python3 -c '
import sys
try:
    l=float(sys.argv[1]); g=float(sys.argv[2])
except Exception:
    print("unknown"); raise SystemExit
print("shut" if l>=g else "open")
' "$load1" "$gate_num")"
fi

verdict="ok"
if [ "$heavy_count" -gt "$LIMIT" ]; then
  verdict="violation"
elif [ "$gate_state" = "shut" ] && [ "$heavy_count" -gt 0 ]; then
  verdict="violation"
fi

if [ "$JSON" -eq 1 ]; then
  printf '%s\n' "$heavy_rows" | VERDICT="$verdict" LOAD1="${load1:-}" GATE="${gate_num:-}" \
    LIMIT="$LIMIT" COUNT="$heavy_count" ATTR="$attr_ok" GATE_STATE="$gate_state" \
    python3 -c '
import json,os,sys
rows=[]
for line in sys.stdin.read().splitlines():
    if not line.strip(): continue
    pid,kind,cpu,etimes,cwd,digest,args=line.split("|",6)
    rows.append({"pid":int(pid),"kind":kind,"cpu_pct":int(cpu),"elapsed_s":int(etimes) if etimes.isdigit() else 0,
                 "cwd":cwd,"cmd_digest":digest,"cmd":args[:160]})
print(json.dumps({
  "load1": float(os.environ["LOAD1"]) if os.environ["LOAD1"] else None,
  "gate": float(os.environ["GATE"]) if os.environ["GATE"] else None,
  "gate_state": os.environ["GATE_STATE"],
  "policy_limit": int(os.environ["LIMIT"]),
  "heavy_jobs": rows,
  "count": int(os.environ["COUNT"]),
  "lane_attribution": os.environ["ATTR"]=="1",
  "verdict": os.environ["VERDICT"],
}, indent=2))
'
else
  say "host load1 : ${load1:-unknown}   gate: ${gate_num:-unknown}   gate state: $gate_state"
  if [ "$heavy_count" -eq 0 ]; then
    say "heavy jobs : 0 (none over ${HEAVY_CPU_MIN}% CPU)"
  else
    say "heavy jobs : $heavy_count (policy limit $LIMIT per host — docs/load-attribution.md)"
    say ""
    printf '%s' "$heavy_rows" | head -n "$TOP" | while IFS='|' read -r pid kind cpu etimes cwd digest args; do
      [ -n "$pid" ] || continue
      lane="$(attribute "$cwd")"
      [ -n "$lane" ] || lane="(unattributed)"
      printf '  pid %-7s %-8s cpu=%-3s up=%-8s lane=%-24s cwd=%s\n' \
        "$pid" "$kind" "$cpu" "${etimes:-?}s" "$lane" "${cwd:-(unknown)}"
      printf '           cmd=%s… %s\n' "${digest:-no-digest}" "$(printf '%s' "$args" | cut -c1-90)"
    done
  fi
  say ""
  case "$verdict" in
    ok) say "verdict    : OK" ;;
    violation)
      if [ "$heavy_count" -gt "$LIMIT" ]; then
        say "verdict    : VIOLATION — $heavy_count concurrent heavy jobs, policy allows $LIMIT"
      else
        say "verdict    : VIOLATION — the load gate is SHUT and ${heavy_count} heavy job(s) named above are the holders"
      fi ;;
  esac
  if [ "$attr_ok" -eq 0 ]; then
    say "note       : lane attribution unavailable (scheduler API at $API did not answer /api/v1/projects)"
  fi
fi

[ "$verdict" = "violation" ] && exit 1
[ -z "$gate_num" ] && exit 2
exit 0
