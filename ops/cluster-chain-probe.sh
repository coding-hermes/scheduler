#!/usr/bin/env bash
#
# cluster-chain-probe.sh — measure each hop of the distributed code factory.
#
# WHY THIS EXISTS
#   The distributed-code-factory goal (~/.hermes/goals/remote-coding-system.md)
#   describes a chain — a task is routed to a model, dispatched by the
#   scheduler, carried over the crier bus, executed inside a bunker agent and
#   landed through a connector — and names six done-gates (G1-G6) for it. None
#   of those gates had an executable measurement: every claim about the chain
#   was prose. This probe is that measurement. One arm per hop, one JSON object
#   per arm, an exit code that can say NO.
#
# WHAT A PASS MEANS
#   A PASS names what it measured (`measured` field): the model+provider the
#   router answered with, the crier version string, the scheduler's build_sha
#   and its drift against the checked-out tree, the bunker server and agent
#   count. "ok" is never a measurement. A FAIL names the hop and the reason
#   (`reason` field) with a stable machine-readable prefix.
#
# READ-ONLY BY CONSTRUCTION
#   Every arm issues a GET, or an explicitly list/status CLI verb. Nothing here
#   restarts, deploys, writes, signals or mutates a service, an agent or a
#   board. The probe never reads a credential and never sends one: where a
#   surface requires auth (crier /status, crier /agents) the probe deliberately
#   sends NO Authorization header and records the refusal as the measurement.
#   It can therefore be run against the live fleet at any time.
#
# USAGE
#   ops/cluster-chain-probe.sh [ARM] [options]
#
#     ARM  router | proxy | crier | scheduler | bunker | connector
#          | all | arms | help
#
#     `all` runs every arm in order (router, proxy, crier, scheduler, bunker,
#     connector) and ends with a summary object + one human-readable line.
#     `arms` prints the arm names and whether each one gates the exit code.
#
# OPTIONS
#   --repo PATH            checkout the live build_sha is compared against
#                          (default: this script's own git top level)
#   --against REF          the DEPLOY LINEAGE the running build must contain
#                          (default: origin/main when that ref resolves here,
#                          else HEAD). Comparing against a feature branch's HEAD
#                          reports "drift" for a correctly deployed daemon.
#   --project NAME         router project to resolve
#                          (default: coding-hermes-scheduler)
#   --timeout SECS         bound on every external command (default 15)
#   --scheduler-url URL    default http://127.0.0.1:9090
#   --crier-url URL        default http://127.0.0.1:${CRIER_PORT:-8767}
#   --router-url URL       default http://127.0.0.1:9092 (documented fleet API)
#   --proxy-url URL        default http://127.0.0.1:9391 (the classified proxy;
#                          env ROUTER_PROXY_URL, the name the router repo uses)
#   --proxy-repo PATH      checkout the proxy's loaded commit is compared
#                          against (default ~/task-router)
#   --proxy-required       the proxy HOP is owned by this host, so an
#                          unmeasurable proxy is a FAIL, not a report
#   --bunker-bin PATH      default `bunker` on PATH, then ~/.local/bin/bunker
#   --router-spawn PATH    default ~/.hermes/scripts/router_spawn.py
#   --offline              skip the connector arm's `git ls-remote` (no network)
#   --no-router-http       skip the router arm's documented HTTP sub-check
#   -h | --help
#
# ENVIRONMENT (same knobs, for cron/wrappers)
#   CLUSTER_CHAIN_REPO, CLUSTER_CHAIN_PROJECT, CLUSTER_CHAIN_TIMEOUT,
#   CLUSTER_CHAIN_OFFLINE, CLUSTER_CHAIN_PROXY_REQUIRED, SCHEDULER_URL,
#   CRIER_URL, CRIER_PORT, ROUTER_URL, ROUTER_PROXY_URL,
#   ROUTER_PROXY_REPO, BUNKER_BIN, ROUTER_SPAWN
#
# EXIT CODES
#   0  every GATING arm that ran PASSed (report-only arms never gate)
#   1  at least one gating arm FAILed
#   2  usage error
#
# WHAT THIS PROBE DOES NOT COVER — see docs/cluster-chain-probe.md, which is
# the authority. In short: it proves each hop ANSWERS, not that a task travelled
# end to end. It never runs a tick, never spawns a worker, never delivers a
# message, never executes inside an agent, and it does not read the crier bus
# through authentication.

set -uo pipefail

PROBE=cluster-chain-probe
CONNECTOR_BRANCH_REF=refs/heads/wt/SCHED-HARNESS-003-port

TIMEOUT="${CLUSTER_CHAIN_TIMEOUT:-15}"
SCHEDULER_URL="${SCHEDULER_URL:-http://127.0.0.1:9090}"
CRIER_URL="${CRIER_URL:-http://127.0.0.1:${CRIER_PORT:-8767}}"
ROUTER_URL="${ROUTER_URL:-http://127.0.0.1:9092}"
PROXY_URL="${ROUTER_PROXY_URL:-http://127.0.0.1:9391}"
PROXY_REPO="${ROUTER_PROXY_REPO:-}"
PROXY_REQUIRED="${CLUSTER_CHAIN_PROXY_REQUIRED:-0}"
PROJECT="${CLUSTER_CHAIN_PROJECT:-coding-hermes-scheduler}"
REPO="${CLUSTER_CHAIN_REPO:-}"
AGAINST="${CLUSTER_CHAIN_AGAINST:-}"
BUNKER_BIN_PATH="${BUNKER_BIN:-}"
ROUTER_SPAWN_PATH="${ROUTER_SPAWN:-}"
OFFLINE="${CLUSTER_CHAIN_OFFLINE:-0}"
ROUTER_HTTP=1

GATING_ARMS=(router proxy crier scheduler bunker)
REPORT_ARMS=(connector)
ALL_ARMS=(router proxy crier scheduler bunker connector)

declare -A VERDICT=() MEASURED=() REASON=()
ARMS_RUN=()
ARM_START_MS=0

TMP="$(mktemp -d "${TMPDIR:-/tmp}/cluster-chain-probe.XXXXXX")" || {
  echo "cluster-chain-probe: cannot create a temp dir" >&2; exit 2; }
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# ─── small helpers ──────────────────────────────────────────────────────────

usage() { sed -n '2,/^# WHAT THIS PROBE DOES NOT COVER/p' "$0" | sed 's/^# \{0,1\}//'; }

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

now_ms() { echo $(( $(date +%s%N) / 1000000 )); }

# jstr STRING -> a JSON string literal (including the quotes).
jstr() {
  local s="${1-}"
  s="$(printf '%s' "$s" | tr -d '\000-\010\013\014\016-\037')"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '"%s"' "$s"
}

# jget FILE DOTTED.PATH -> the value, or empty when the file/key is absent.
jget() {
  local f="$1" p="$2"
  [ -s "$f" ] || return 0
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg p "$p" '
      (try (getpath($p | split("."))) catch null) as $v
      | if $v == null then ""
        elif ($v | type) == "boolean" then ($v | tostring)
        elif ($v | type) == "object" or ($v | type) == "array" then ($v | tojson)
        else $v end' "$f" 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$f" "$p" <<'PY' 2>/dev/null
import json, sys
try:
    cur = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
for key in sys.argv[2].split('.'):
    if isinstance(cur, dict) and key in cur:
        cur = cur[key]
    else:
        sys.exit(0)
if cur is None:
    print('')
elif isinstance(cur, bool):
    print('true' if cur else 'false')
elif isinstance(cur, (dict, list)):
    print(json.dumps(cur))
else:
    print(cur)
PY
  fi
}

# tail_n FILE N -> the file's content, newlines folded, truncated to N chars.
tail_n() {
  tr -d '\r' <"$1" 2>/dev/null | tr '\n' ' ' | cut -c1-"${2:-200}"
}

# http_get URL OUTFILE -> prints the HTTP status code ("000" when it never
# connected). Body lands in OUTFILE, curl's own error in $TMP/curl.err.
http_get() {
  local url="$1" out="$2"
  : >"$TMP/curl.err"
  : >"$out"
  curl -sS --connect-timeout 3 --max-time "$TIMEOUT" \
    -o "$out" -w '%{http_code}' "$url" 2>"$TMP/curl.err" || true
}

curl_fail_reason() {
  local r
  r="$(tail_n "$TMP/curl.err" 120)"
  [ -n "$r" ] || r="no response"
  printf '%s' "$r" | sed 's/"/'"'"'/g'
}

have_curl() {
  command -v curl >/dev/null 2>&1
}

# ─── arm result emitters (one JSON object per arm) ──────────────────────────

emit_one() {
  local a="$1" elapsed
  elapsed=$(( $(now_ms) - ARM_START_MS ))
  printf '{"arm":%s,"verdict":%s,"measured":%s,"reason":%s,"elapsed_ms":%d,"ts":%s}\n' \
    "$(jstr "$a")" "$(jstr "${VERDICT[$a]:-FAIL}")" "$(jstr "${MEASURED[$a]:-}")" \
    "$(jstr "${REASON[$a]:-}")" "$elapsed" "$(jstr "$(ts)")"
}

arm_pass() { VERDICT[$1]=PASS; MEASURED[$1]="${2-}"; REASON[$1]="${3-}"; emit_one "$1"; }
arm_fail() { VERDICT[$1]=FAIL; MEASURED[$1]="${2-}"; REASON[$1]="${3-}"; emit_one "$1"; }
arm_report() { VERDICT[$1]=REPORT; MEASURED[$1]="${2-}"; REASON[$1]="${3-}"; emit_one "$1"; }

run_arm() {
  local a="$1"
  ARMS_RUN+=("$a")
  ARM_START_MS="$(now_ms)"
  "arm_$a" || true
  if [ -z "${VERDICT[$a]:-}" ]; then
    arm_fail "$a" "" "PROBE_INTERNAL: arm '$a' returned without emitting a verdict"
  fi
}

# ─── arm: router ────────────────────────────────────────────────────────────
# Measures: does the Task Router resolve a route for a named profile and name
# the model AND provider, and does it refuse an invalid request with a NAMED
# code (a refusal without a code cannot be told from a crash)?
arm_router() {
  local script py out rc ref_out code model provider hop price profile err
  local hcode http_model http_provider hcode_ref http_ref_body
  local agree measured

  script="$ROUTER_SPAWN_PATH"
  if [ -z "$script" ]; then
    if [ -f "$HOME/.hermes/scripts/router_spawn.py" ]; then
      script="$HOME/.hermes/scripts/router_spawn.py"
    elif [ -f "$HOME/task-router/scripts/router_spawn.py" ]; then
      script="$HOME/task-router/scripts/router_spawn.py"
    fi
  fi
  if [ -z "$script" ] || [ ! -f "$script" ]; then
    arm_fail router "" "ROUTER_SCRIPT_ABSENT: no router_spawn.py found (looked at \$ROUTER_SPAWN, ~/.hermes/scripts/router_spawn.py, ~/task-router/scripts/router_spawn.py) — the CLI hop cannot be measured"
    return 1
  fi

  py=""
  local cand
  for cand in "$HOME/.hermes/venvs/board/bin/python3" "$(command -v python3 2>/dev/null)"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then py="$cand"; break; fi
  done
  if [ -z "$py" ]; then
    arm_fail router "" "ROUTER_PYTHON_ABSENT: no interpreter for router_spawn.py (~/.hermes/venvs/board/bin/python3 and python3 both missing)"
    return 1
  fi

  out="$TMP/router-resolve.json"
  timeout "$TIMEOUT" "$py" "$script" "$PROJECT" --format json >"$out" 2>"$TMP/router.err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    arm_fail router "" "ROUTER_CLI_EXIT_${rc}: router_spawn.py exited ${rc} for project ${PROJECT}: $(tail_n "$TMP/router.err" 160)"
    return 1
  fi

  err="$(jget "$out" error)"
  if [ -n "$err" ]; then
    arm_fail router "error=${err}" "ROUTER_REFUSED_HAPPY_PATH: router_spawn.py answered '${err}' for project ${PROJECT}"
    return 1
  fi

  model="$(jget "$out" head.model)"
  provider="$(jget "$out" head.provider)"
  hop="$(jget "$out" head.hop)"
  price="$(jget "$out" head.effective_price)"
  profile="$(jget "$out" profile)"
  if [ -z "$model" ] || [ -z "$provider" ]; then
    arm_fail router "model='${model}' provider='${provider}' profile='${profile}'" \
      "ROUTE_UNNAMED: resolve for project ${PROJECT} returned no model+provider pair (model='${model}', provider='${provider}')"
    return 1
  fi

  # Refusal sub-check (CLI): an invalid requirement must come back with a NAMED code.
  ref_out="$TMP/router-refusal.json"
  timeout "$TIMEOUT" "$py" "$script" "$PROJECT" --profile-req cluster_probe_unknown_category=3 \
    --format json >"$ref_out" 2>/dev/null
  rc=$?
  code="$(jget "$ref_out" code)"
  if [ -z "$code" ]; then
    arm_fail router "model=${model} provider=${provider} cli_refusal_code=''" \
      "ROUTER_REFUSAL_UNNAMED: an invalid requirement (exit ${rc}) produced no named 'code' — a refusal that cannot be named cannot be distinguished from a crash"
    return 1
  fi

  measured="cli: model=${model} provider=${provider} profile=${profile} hop=${hop} effective_price=${price} refusal_code=${code}"

  # HTTP sub-check: the documented fleet API (docs/health-plane.md lists
  # 127.0.0.1:9092 with GET /resolve). Skipped only when explicitly asked.
  if [ "$ROUTER_HTTP" = "1" ]; then
    if ! have_curl; then
      arm_fail router "$measured" "ROUTER_HTTP_UNMEASURED: curl is absent, so the documented :9092 surface was not probed"
      return 1
    fi
    hcode="$(http_get "${ROUTER_URL}/resolve?project=${PROJECT}" "$TMP/router-http.json")"
    if [ "$hcode" != "200" ]; then
      arm_fail router "$measured http=${ROUTER_URL}/resolve status=${hcode}" \
        "ROUTER_HTTP_UNREACHABLE: GET ${ROUTER_URL}/resolve?project=${PROJECT} answered ${hcode} ($(curl_fail_reason)); the documented fleet API is part of this hop"
      return 1
    fi
    http_model="$(jget "$TMP/router-http.json" head.model)"
    http_provider="$(jget "$TMP/router-http.json" head.provider)"
    if [ -z "$http_model" ] || [ -z "$http_provider" ]; then
      arm_fail router "$measured http_status=200 http_model='' http_provider=''" \
        "ROUTER_HTTP_UNNAMED: the HTTP resolve answered 200 without naming a model+provider pair"
      return 1
    fi
    hcode_ref="$(http_get "${ROUTER_URL}/resolve?project=__cluster_probe_no_such_project__" "$TMP/router-http-refusal.json")"
    if [ "$hcode_ref" != "400" ]; then
      http_ref_body="$(tail_n "$TMP/router-http-refusal.json" 80)"
      arm_fail router "$measured http_status=200 refusal_http_status=${hcode_ref}" \
        "ROUTER_HTTP_REFUSAL_UNNAMED: an unknown project returned ${hcode_ref} instead of a 400 refusal (body: ${http_ref_body})"
      return 1
    fi
    agree="no"
    [ "$http_model" = "$model" ] && [ "$http_provider" = "$provider" ] && agree="yes"
    measured="${measured} | http(${ROUTER_URL}): model=${http_model} provider=${http_provider} unknown_project_status=${hcode_ref} surfaces_agree=${agree}"
  else
    measured="${measured} | http: skipped (--no-router-http)"
  fi

  arm_pass router "$measured" "route resolved and refusal named on both surfaces"
  return 0
}

# ─── arm: proxy (the classified model-serving ingress) ──────────────────────
# Measures: does the ingress answer, which commit did the RUNNING PROCESS load,
# and is that commit behind the deploy lineage on paths that reach a running
# Python process (*.py)?
#
# WHY THIS ARM EXISTS: the ingress is the one hop whose staleness is invisible
# by construction. /health has carried an honest `code` block since TR-141 and a
# watchdog has consumed it since TR-255, yet the proxy served a build loaded
# from an older commit with nothing on the chain's own measurement saying so —
# measured 2026-10-29 19:35Z: loaded_commit=13ef7482e1d7, repo_commit=73d4795b0dc9,
# stale=true, while this probe reported every hop it looked at green.
#
# GATING RULE: it FAILs on drift it can measure and REPORTS when it cannot (no
# listener, no loaded commit, no checkout to compare against) — a host that does
# not run the ingress must not read a missing hop as a broken one. Where the hop
# IS owned, --proxy-required (CLUSTER_CHAIN_PROXY_REQUIRED=1) turns those
# unmeasurable cases into FAILs so the arm cannot become decoration.
proxy_unmeasurable() { # measured reason
  if [ "$PROXY_REQUIRED" = "1" ]; then
    arm_fail proxy "$1" "$2"
    return 1
  fi
  arm_report proxy "$1" \
    "REPORT_ONLY: ${2} — the ingress is not owned by every host; set --proxy-required (CLUSTER_CHAIN_PROXY_REQUIRED=1) where it is (exit code unaffected)"
  return 0
}

arm_proxy() {
  local hcode status commit loaded repo_commit stale_flag mode
  local p_repo p_ref p_head p_short p_behind p_ahead p_cb p_ca
  local measured drift flag_agrees

  if ! have_curl; then
    proxy_unmeasurable "" "PROXY_UNMEASURED: curl is absent, so the ingress was not probed"
    return $?
  fi

  hcode="$(http_get "${PROXY_URL}/health" "$TMP/proxy-health.json")"
  if [ "$hcode" != "200" ]; then
    proxy_unmeasurable "url=${PROXY_URL} health_status=${hcode}" \
      "PROXY_UNREACHABLE: GET ${PROXY_URL}/health answered ${hcode} ($(curl_fail_reason))"
    return $?
  fi

  status="$(jget "$TMP/proxy-health.json" status)"
  if [ -n "$status" ] && [ "$status" != "ok" ]; then
    arm_fail proxy "url=${PROXY_URL} health_status=200 body_status='${status}'" \
      "PROXY_UNHEALTHY: GET ${PROXY_URL}/health answered 200 with status='${status}' — an ingress that names itself unhealthy is failing whatever its build is"
    return 1
  fi

  commit="$(jget "$TMP/proxy-health.json" commit)"
  loaded="$(jget "$TMP/proxy-health.json" code.loaded_commit)"
  [ -n "$commit" ] || commit="$loaded"
  repo_commit="$(jget "$TMP/proxy-health.json" code.repo_commit)"
  stale_flag="$(jget "$TMP/proxy-health.json" code.stale)"
  mode="$(jget "$TMP/proxy-health.json" mode)"
  if [ -z "$commit" ]; then
    proxy_unmeasurable "url=${PROXY_URL} health=ok commit='' loaded_commit='${loaded}'" \
      "PROXY_NO_COMMIT: /health answered 200 without a loaded commit (neither 'commit' nor 'code.loaded_commit') — the serving build cannot be attributed"
    return $?
  fi

  p_repo="$PROXY_REPO"
  [ -n "$p_repo" ] || p_repo="$HOME/task-router"
  if [ ! -d "$p_repo" ]; then
    proxy_unmeasurable "url=${PROXY_URL} commit=${commit} proxy_repo='${p_repo}'" \
      "PROXY_DRIFT_UNMEASURED: no checkout to compare the loaded commit against (proxy_repo='${p_repo}')"
    return $?
  fi
  if git -C "$p_repo" rev-parse --verify --quiet origin/main >/dev/null 2>&1; then
    p_ref="origin/main"
  else
    p_ref="HEAD"
  fi
  p_head="$(git -C "$p_repo" rev-parse --verify --quiet "${p_ref}^{commit}" 2>/dev/null || true)"
  if [ -z "$p_head" ]; then
    proxy_unmeasurable "url=${PROXY_URL} commit=${commit} proxy_repo=${p_repo}" \
      "PROXY_DRIFT_UNMEASURED: ${p_repo} has no resolvable ${p_ref}"
    return $?
  fi
  p_short="${p_head:0:8}"
  measured="url=${PROXY_URL} health=ok mode=${mode:-unknown} loaded_commit=${loaded:-$commit} commit=${commit} repo_commit_at_load=${repo_commit:-unknown} stale_flag=${stale_flag:-null} proxy_repo=${p_repo} against=${p_ref} head=${p_short}"

  if [ "${p_head:0:${#commit}}" = "$commit" ]; then
    flag_agrees="yes"
    [ "$stale_flag" = "true" ] && flag_agrees="no"
    measured="${measured} drift=0 flag_agrees=${flag_agrees}"
    arm_pass proxy "$measured" "the serving ingress loaded the commit ${p_ref} points at"
    return 0
  fi

  if git -C "$p_repo" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    # Count against the RESOLVED ref, never the literal string HEAD (the
    # scheduler arm's lesson, kept here so both arms describe the lineage).
    p_behind="$(git -C "$p_repo" rev-list --count "${commit}..${p_head}" 2>/dev/null || echo '?')"
    p_ahead="$(git -C "$p_repo" rev-list --count "${p_head}..${commit}" 2>/dev/null || echo '?')"
    # COMPILED drift for a Python service is *.py: the interpreter imported the
    # files that were on disk when it started, so a commit touching no Python
    # source cannot be in the running process by definition. Counting board or
    # docs commits after a deploy as drift is the noise this filter removes;
    # both raw counts are still printed so nothing is hidden.
    p_cb="$(git -C "$p_repo" rev-list --count "${commit}..${p_head}" -- '*.py' 2>/dev/null || echo '?')"
    p_ca="$(git -C "$p_repo" rev-list --count "${p_head}..${commit}" -- '*.py' 2>/dev/null || echo '?')"
    drift="behind=${p_behind} ahead=${p_ahead} compiled_behind=${p_cb} compiled_ahead=${p_ca}"
    flag_agrees="yes"
    if [ "$stale_flag" = "true" ] && [ "$p_cb" = "0" ] && [ "$p_ca" = "0" ]; then
      flag_agrees="no"
    elif [ "$stale_flag" = "false" ] && { [ "$p_cb" != "0" ] || [ "$p_ca" != "0" ]; }; then
      # A stale build whose own flag says fresh is a lying flag, not a pass.
      flag_agrees="no"
    fi
    measured="${measured} drift=${drift} flag_agrees=${flag_agrees}"
    if [ "$p_cb" = "0" ] && [ "$p_ca" = "0" ]; then
      arm_pass proxy "$measured" \
        "the loaded build matches the deploy lineage for every commit that reaches a running Python process"
      return 0
    fi
    arm_fail proxy "$measured" \
      "PROXY_BUILD_DRIFT: the ingress at ${PROXY_URL} loaded ${commit}, which is ${p_behind} commit(s) behind ${p_ref} (${p_head}) and ${p_ahead} ahead of it; ${p_cb} of the behind-commits touch *.py, so the responder is serving code the deploy lineage has already replaced — redeploy the ingress (sync + restart); this probe will not"
    return 1
  fi
  measured="${measured} drift=unknown-commit"
  arm_fail proxy "$measured" \
    "PROXY_BUILD_UNKNOWN_COMMIT: the ingress reports commit ${commit}, which is not a commit in ${p_repo}, so the serving build cannot be attributed to ${p_ref}"
  return 1
}

# ─── arm: crier ─────────────────────────────────────────────────────────────
# Measures: is the bus reachable, which build answers, and does a protected
# route REFUSE an unauthenticated caller (the probe never sends a token).
arm_crier() {
  local hcode vcode scode status version commit auth measured
  if ! have_curl; then
    arm_fail crier "" "CRIER_UNMEASURED: curl is absent"
    return 1
  fi

  hcode="$(http_get "${CRIER_URL}/health" "$TMP/crier-health.json")"
  if [ "$hcode" != "200" ]; then
    arm_fail crier "url=${CRIER_URL} health_status=${hcode}" \
      "CRIER_UNREACHABLE: GET ${CRIER_URL}/health answered ${hcode} ($(curl_fail_reason))"
    return 1
  fi
  status="$(jget "$TMP/crier-health.json" status)"
  if [ "$status" != "ok" ]; then
    arm_fail crier "url=${CRIER_URL} health_status=200 body_status='${status}'" \
      "CRIER_UNHEALTHY: /health answered 200 with status='${status}' (expected 'ok')"
    return 1
  fi

  vcode="$(http_get "${CRIER_URL}/version" "$TMP/crier-version.json")"
  if [ "$vcode" != "200" ]; then
    arm_fail crier "url=${CRIER_URL} health=ok version_status=${vcode}" \
      "CRIER_VERSION_UNAVAILABLE: GET ${CRIER_URL}/version answered ${vcode} ($(curl_fail_reason)) — reachable but not attributable to a build"
    return 1
  fi
  version="$(jget "$TMP/crier-version.json" version)"
  commit="$(jget "$TMP/crier-version.json" commit)"
  if [ -z "$version" ]; then
    arm_fail crier "url=${CRIER_URL} health=ok version=''" \
      "CRIER_VERSION_UNNAMED: /version answered 200 without a version string"
    return 1
  fi

  # Auth posture. No token is sent: the refusal IS the measurement.
  scode="$(http_get "${CRIER_URL}/status" "$TMP/crier-status.json")"
  case "$scode" in
    401) auth="refused:401 $(jget "$TMP/crier-status.json" error)" ;;
    403) auth="refused:403" ;;
    200) auth="disabled:/status answered unauthenticated" ;;
    *)   auth="inconclusive:/status ${scode}" ;;
  esac

  measured="url=${CRIER_URL} health=ok version=${version} commit=${commit} auth=${auth}"
  arm_pass crier "$measured" "bus reachable and build-attributable; no credential sent"
  return 0
}

# ─── arm: scheduler ─────────────────────────────────────────────────────────
# Measures: health + status answer, db connected, active ticks, and whether the
# RUNNING build_sha is the checked-out HEAD (drift is a FAIL naming the count).
arm_scheduler() {
  local hcode scode status db build_sha active_ticks version clock
  local s_build_sha s_active s_projects
  local head_full head_short behind ahead drift measured
  if ! have_curl; then
    arm_fail scheduler "" "SCHEDULER_UNMEASURED: curl is absent"
    return 1
  fi

  # LIVENESS FIRST (SCHED-GAP-204-A). /api/v1/live answers from process memory and
  # never touches the DB; /api/v1/health issues DB reads on the daemon's ONE
  # serialized SQLite connection and answers 504 naming the stalled step when the
  # connection is busy (SCHED-GAP-1575-B, --api-read-timeout, default 5s). Probing
  # only the rich route therefore reports a WORKING daemon as UNREACHABLE under
  # load — measured twice on this host, most recently 34s after a clean restart.
  local lcode live_sha live_uptime degraded=0 health_note="ok"
  lcode="$(http_get "${SCHEDULER_URL}/api/v1/live" "$TMP/sched-live.json")"
  if [ "$lcode" != "200" ]; then
    arm_fail scheduler "url=${SCHEDULER_URL} live_status=${lcode}" \
      "SCHEDULER_UNREACHABLE: GET ${SCHEDULER_URL}/api/v1/live answered ${lcode} ($(curl_fail_reason)) — the DB-free liveness route did not answer, so the process is down or wedged"
    return 1
  fi
  live_sha="$(jget "$TMP/sched-live.json" build_sha)"
  live_uptime="$(jget "$TMP/sched-live.json" uptime)"
  if [ -z "$live_sha" ]; then
    arm_fail scheduler "url=${SCHEDULER_URL} live=ok build_sha=''" \
      "SCHEDULER_NO_BUILD_SHA: /api/v1/live answered 200 without a build_sha — the running build cannot be attributed to a commit"
    return 1
  fi

  hcode="$(http_get "${SCHEDULER_URL}/api/v1/health" "$TMP/sched-health.json")"
  case "$hcode" in
    200) : ;;
    503|504)
      # Degraded, not down: the process answered on the DB-free route. Record the
      # reading and carry on with the build/drift comparison, which does not need
      # the DB.
      degraded=1
      health_note="degraded:${hcode}($(jget "$TMP/sched-health.json" helper))"
      ;;
    *)
      arm_fail scheduler "url=${SCHEDULER_URL} live=ok health_status=${hcode}" \
        "SCHEDULER_HEALTH_UNEXPECTED: /api/v1/live answered 200 but /api/v1/health answered ${hcode} ($(curl_fail_reason))"
      return 1
      ;;
  esac
  if [ "$hcode" = "200" ]; then
    status="$(jget "$TMP/sched-health.json" status)"
    db="$(jget "$TMP/sched-health.json" db)"
    build_sha="$(jget "$TMP/sched-health.json" build_sha)"
    active_ticks="$(jget "$TMP/sched-health.json" active_ticks)"
    version="$(jget "$TMP/sched-health.json" version)"
    clock="$(jget "$TMP/sched-health.json" clock.mode)"
    if [ "$status" != "ok" ]; then
      arm_fail scheduler "url=${SCHEDULER_URL} live=ok body_status='${status}'" \
        "SCHEDULER_UNHEALTHY: /api/v1/health answered 200 with status='${status}'"
      return 1
    fi
    if [ "$db" != "connected" ]; then
      arm_fail scheduler "db='${db}' build_sha=${build_sha}" \
        "SCHEDULER_DB_NOT_CONNECTED: /api/v1/health reports db='${db}'"
      return 1
    fi
    if [ -z "$build_sha" ]; then
      arm_fail scheduler "db=${db} build_sha=''" \
        "SCHEDULER_NO_BUILD_SHA: /api/v1/health names no build_sha — the running build cannot be attributed to a commit"
      return 1
    fi
  else
    # Degraded: the DB-backed route is over its read budget. Everything the drift
    # comparison needs is already in hand from /api/v1/live.
    db="unknown"
    build_sha="$live_sha"
    active_ticks="?"
    version="$(jget "$TMP/sched-live.json" version)"
    clock="?"
    status="degraded"
  fi

  scode="$(http_get "${SCHEDULER_URL}/api/v1/status" "$TMP/sched-status.json")"
  if [ "$scode" != "200" ]; then
    if [ "$degraded" = "1" ]; then
      # Expected companion of a 504 health read under the same budget: recorded,
      # never a second verdict about the same condition.
      scode="degraded:${scode}"
    else
      arm_fail scheduler "build_sha=${build_sha} status_status=${scode}" \
        "SCHEDULER_STATUS_UNAVAILABLE: /api/v1/live and /api/v1/health answered but GET ${SCHEDULER_URL}/api/v1/status answered ${scode} ($(curl_fail_reason))"
      return 1
    fi
  fi
  s_build_sha="$(jget "$TMP/sched-status.json" build_sha)"
  s_active="$(jget "$TMP/sched-status.json" active_ticks)"
  s_projects="$(jget "$TMP/sched-status.json" active_projects)"

  measured="url=${SCHEDULER_URL} live=ok health=${health_note} uptime=${live_uptime} db=${db} build_sha=${build_sha} version=${version} clock=${clock} active_ticks=${active_ticks}/${s_active} active_projects=${s_projects} status_build_sha=${s_build_sha}"

  if [ -z "$REPO" ]; then
    REPO="$(git -C "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" rev-parse --show-toplevel 2>/dev/null || true)"
  fi
  if [ -z "$REPO" ] || [ ! -d "$REPO" ]; then
    arm_fail scheduler "$measured" "SCHEDULER_DRIFT_UNMEASURED: no checkout to compare build_sha against (repo='${REPO}')"
    return 1
  fi
  # The comparison ref is the DEPLOY LINEAGE, not whatever this checkout happens
  # to have checked out: run from a feature worktree, HEAD is that branch and a
  # daemon deployed from main would report drift forever.
  local against_ref="$AGAINST"
  if [ -z "$against_ref" ]; then
    if git -C "$REPO" rev-parse --verify --quiet origin/main >/dev/null 2>&1; then
      against_ref="origin/main"
    else
      against_ref="HEAD"
    fi
  fi
  head_full="$(git -C "$REPO" rev-parse --verify --quiet "${against_ref}^{commit}" 2>/dev/null || true)"
  if [ -z "$head_full" ]; then
    arm_fail scheduler "$measured repo=${REPO}" "SCHEDULER_DRIFT_UNMEASURED: ${REPO} has no resolvable ${against_ref}"
    return 1
  fi
  head_short="${head_full:0:8}"
  measured="${measured} against=${against_ref}"

  if [ "${head_full:0:${#build_sha}}" = "$build_sha" ]; then
    measured="${measured} head=${head_short} drift=0"
    arm_pass scheduler "$measured" "running build is the checked-out HEAD"
    return 0
  fi

  if git -C "$REPO" cat-file -e "${build_sha}^{commit}" 2>/dev/null; then
    # Count against the RESOLVED ref, never the literal string HEAD: with
    # --against origin/main the counts must describe the lineage, and a bare
    # "HEAD" here silently measured this checkout's branch instead.
    behind="$(git -C "$REPO" rev-list --count "${build_sha}..${head_full}" 2>/dev/null || echo '?')"
    ahead="$(git -C "$REPO" rev-list --count "${head_full}..${build_sha}" 2>/dev/null || echo '?')"
    # COMPILED drift is the number that costs anything: a commit that touches no
    # Go source, module file or embedded template is not in the binary by
    # definition, so counting it as "the fleet runs a build the lineage does not
    # contain" is noise — measured: 4 ops-script commits made a daemon deployed
    # from the previous main tip read as drifted.
    compiled_behind="$(git -C "$REPO" rev-list --count "${build_sha}..${head_full}" -- \
        '*.go' 'go.mod' 'go.sum' '*.html' '*.tmpl' 2>/dev/null || echo '?')"
    compiled_ahead="$(git -C "$REPO" rev-list --count "${head_full}..${build_sha}" -- \
        '*.go' 'go.mod' 'go.sum' '*.html' '*.tmpl' 2>/dev/null || echo '?')"
    drift="behind=${behind} ahead=${ahead} compiled_behind=${compiled_behind} compiled_ahead=${compiled_ahead}"
    if [ "$compiled_behind" = "0" ] && [ "$compiled_ahead" = "0" ]; then
      measured="${measured} head=${head_short} drift=${drift}"
      arm_pass scheduler "$measured" \
        "running build matches the deploy lineage for every commit that reaches the binary"
      return 0
    fi
    arm_fail scheduler "$measured head=${head_short} drift=${drift}" \
      "SCHEDULER_BUILD_DRIFT: the running binary was built from ${build_sha}, which is ${behind} commit(s) behind ${against_ref} (${head_short}) and ${ahead} ahead of it — the fleet is executing a build the deploy lineage does not contain"
  else
    drift="unknown-commit"
    arm_fail scheduler "$measured head=${head_short} drift=${drift}" \
      "SCHEDULER_BUILD_UNKNOWN_COMMIT: build_sha ${build_sha} is not a commit in ${REPO}, so the running build cannot be attributed to ${against_ref}"
  fi
  return 1
}

# ─── arm: bunker ────────────────────────────────────────────────────────────
# Measures: is the agent host reachable through its CLI, and how many agents
# does it hold. `bunker list` is a read-only verb.
arm_bunker() {
  local bin out rc total server
  bin="$BUNKER_BIN_PATH"
  if [ -z "$bin" ]; then
    bin="$(command -v bunker 2>/dev/null || true)"
  fi
  if [ -z "$bin" ] && [ -x "$HOME/.local/bin/bunker" ]; then
    bin="$HOME/.local/bin/bunker"
  fi
  if [ -z "$bin" ] || [ ! -x "$bin" ]; then
    arm_fail bunker "" "BUNKER_CLI_ABSENT: no executable bunker CLI (looked at \$BUNKER_BIN, PATH, ~/.local/bin/bunker) — the execution hop cannot be measured"
    return 1
  fi

  out="$TMP/bunker-list.txt"
  timeout "$TIMEOUT" "$bin" list >"$out" 2>"$TMP/bunker.err"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    arm_fail bunker "cli=${bin} exit=${rc}" \
      "BUNKER_CLI_EXIT_${rc}: '${bin} list' exited ${rc} ($(tail_n "$TMP/bunker.err" 160)) — the host is not reachable through its CLI"
    return 1
  fi

  total="$(sed -n 's/^Total: \([0-9][0-9]*\) agents.*/\1/p' "$out" | head -1)"
  server="$(sed -n 's/.*(server: \([^)]*\)).*/\1/p' "$out" | head -1)"
  if [ -z "$total" ]; then
    arm_fail bunker "cli=${bin} exit=0 total_line=''" \
      "BUNKER_NO_TOTAL_LINE: '${bin} list' exited 0 without a 'Total: N agents (server: …)' line, so the agent count was not measured ($(tail_n "$out" 120))"
    return 1
  fi

  arm_pass bunker "cli=${bin} server=${server:-unknown} agents=${total}" \
    "agent host answered a read-only list"
  return 0
}

# ─── arm: connector (REPORT-ONLY) ───────────────────────────────────────────
# Measures presence, never a live run: does internal/connector exist on the
# checked-out tree, and does origin carry the connector branch? This arm never
# gates the exit code — its job is to state a fact, not to claim a hop works.
arm_connector() {
  local dir files remote measured sha
  if [ -z "$REPO" ]; then
    REPO="$(git -C "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" rev-parse --show-toplevel 2>/dev/null || true)"
  fi
  if [ -z "$REPO" ] || [ ! -d "$REPO" ]; then
    arm_report connector "repo='${REPO}'" "CONNECTOR_UNMEASURED: no checkout to inspect (report-only arm, exit code unaffected)"
    return 0
  fi

  dir="absent"
  [ -d "$REPO/internal/connector" ] && dir="present"
  files="$(git -C "$REPO" ls-files internal/connector 2>/dev/null | wc -l | tr -d ' ')"
  # PRESENCE IS NOT A HOP. A package nobody imports is a library, not a leg of
  # the factory: count the files OUTSIDE internal/connector that reference it.
  # Measured 2026-10-29 on wt/SCHED-CONNECTOR-PORT: 8 tracked files, 0 importers
  # — the connector compiled and passed its hermetic run while being on no
  # execution path at all, which is exactly the distinction a bare "present"
  # hides.
  importers="$(git -C "$REPO" grep -l 'internal/connector' -- '*.go' 2>/dev/null \
    | grep -v '^internal/connector/' | wc -l | tr -d ' ')"
  case "$dir" in
    absent) wire="absent" ;;
    *) if [ "${importers:-0}" = "0" ]; then wire="unwired"; else wire="wired:${importers}"; fi ;;
  esac

  if [ "$OFFLINE" = "1" ]; then
    remote="skipped:offline"
  else
    sha="$(timeout "$TIMEOUT" git -C "$REPO" ls-remote origin "$CONNECTOR_BRANCH_REF" 2>"$TMP/lsremote.err" | awk '{print $1}' | head -1)"
    if [ -n "$sha" ]; then
      remote="present:${sha:0:8}"
    elif [ -s "$TMP/lsremote.err" ]; then
      remote="unreachable:$(tail_n "$TMP/lsremote.err" 80)"
    else
      remote="absent"
    fi
  fi

  measured="repo=${REPO} internal/connector=${dir} tracked_files=${files} wire=${wire} branch=${CONNECTOR_BRANCH_REF} origin=${remote}"
  case "$wire" in
    absent)
      reason="REPORT_ONLY: no connector package on this tree; the fifth hop does not exist here (exit code unaffected)" ;;
    unwired)
      reason="REPORT_ONLY: connector package present (${files} files) with 0 importers outside it — the hop exists as a library on no execution path (exit code unaffected)" ;;
    *)
      reason="REPORT_ONLY: connector package present and imported by ${importers} file(s) outside it; this arm still never RUNS a connector (exit code unaffected)" ;;
  esac
  arm_report connector "$measured" "$reason"
  return 0
}

# ─── summary ────────────────────────────────────────────────────────────────

is_gating() {
  local a
  for a in "${GATING_ARMS[@]}"; do [ "$a" = "$1" ] && return 0; done
  return 1
}

is_report_only() {
  local a
  for a in "${REPORT_ARMS[@]}"; do [ "$a" = "$1" ] && return 0; done
  return 1
}

run_all() {
  local a passed=0 failed=0 reported=0
  for a in "${ALL_ARMS[@]}"; do run_arm "$a"; done

  local frag="" verdict="PASS" gating_json="" human=""
  for a in "${ARMS_RUN[@]}"; do
    frag="${frag}${frag:+,}\"${a}\":{\"verdict\":\"${VERDICT[$a]}\",\"reason\":$(jstr "${REASON[$a]:-}")}"
    case "${VERDICT[$a]}" in
      PASS) passed=$((passed + 1)) ;;
      FAIL) failed=$((failed + 1)); if is_gating "$a"; then verdict="FAIL"; fi ;;
      REPORT) reported=$((reported + 1)) ;;
    esac
    human="${human}${human:+ }${a}=${VERDICT[$a]}"
    if [ "${VERDICT[$a]}" = "REPORT" ]; then human="${human}(non-gating)"; fi
    if [ "${VERDICT[$a]}" = "FAIL" ]; then human="${human}[${REASON[$a]%%:*}]"; fi
  done
  for a in "${GATING_ARMS[@]}"; do
    gating_json="${gating_json}${gating_json:+,}\"${a}\""
  done

  local overall_exit=0
  [ "$verdict" = "FAIL" ] && overall_exit=1

  printf '{"arm":"summary","verdict":%s,"passed":%d,"failed":%d,"report_only":%d,"gating":[%s],"arms":{%s},"overall_exit":%d,"ts":%s}\n' \
    "$(jstr "$verdict")" "$passed" "$failed" "$reported" \
    "$gating_json" "$frag" "$overall_exit" "$(jstr "$(ts)")"

  printf 'CLUSTER-CHAIN-PROBE: %s — %s\n' "$verdict" "$human"
  return "$overall_exit"
}

# ─── arg parsing ────────────────────────────────────────────────────────────

CMD=""
while [ $# -gt 0 ]; do
  case "$1" in
    router|proxy|crier|scheduler|bunker|connector|all|arms) CMD="$1"; shift ;;
    help|-h|--help) usage; exit 0 ;;
    --repo) REPO="${2-}"; shift 2 ;;
    --against) AGAINST="${2-}"; shift 2 ;;
    --project) PROJECT="${2-}"; shift 2 ;;
    --timeout) TIMEOUT="${2-}"; shift 2 ;;
    --scheduler-url) SCHEDULER_URL="${2-}"; shift 2 ;;
    --crier-url) CRIER_URL="${2-}"; shift 2 ;;
    --router-url) ROUTER_URL="${2-}"; shift 2 ;;
    --proxy-url) PROXY_URL="${2-}"; shift 2 ;;
    --proxy-repo) PROXY_REPO="${2-}"; shift 2 ;;
    --proxy-required) PROXY_REQUIRED=1; shift ;;
    --bunker-bin) BUNKER_BIN_PATH="${2-}"; shift 2 ;;
    --router-spawn) ROUTER_SPAWN_PATH="${2-}"; shift 2 ;;
    --offline) OFFLINE=1; shift ;;
    --no-router-http) ROUTER_HTTP=0; shift ;;
    *) echo "${PROBE}: unknown argument '$1' (try --help)" >&2; exit 2 ;;
  esac
done

case "$TIMEOUT" in
  ''|*[!0-9]*) echo "${PROBE}: --timeout must be a positive integer" >&2; exit 2 ;;
esac

case "$CMD" in
  arms)
    printf '%-12s %s\n' "ARM" "GATES"
    for a in "${ALL_ARMS[@]}"; do
      if is_gating "$a"; then
        if [ "$a" = "proxy" ]; then
          printf '%-12s %s\n' "$a" "yes (reports when unmeasurable)"
        else
          printf '%-12s %s\n' "$a" "yes"
        fi
      elif is_report_only "$a"; then printf '%-12s %s\n' "$a" "no (report-only)"
      else printf '%-12s %s\n' "$a" "no"; fi
    done
    exit 0 ;;
  all) run_all; exit $? ;;
  router|proxy|crier|scheduler|bunker|connector)
    run_arm "$CMD"
    if [ "${VERDICT[$CMD]}" = "FAIL" ] && is_gating "$CMD"; then exit 1; fi
    exit 0 ;;
  '') echo "${PROBE}: no arm given (try --help)" >&2; exit 2 ;;
esac
