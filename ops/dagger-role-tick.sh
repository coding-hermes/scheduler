#!/bin/bash
# CANONICAL SOURCE: the repo copy at ops/dagger-role-tick.sh is authoritative;
# deploy as a byte copy to ~/.hermes/scripts/dagger-role-tick.sh. RETIRED
# scheduler driver (SCHED-GAP-150) kept for manual role runs only.
# DAGGER ROLE TICK — generic driver for the coding-hermes pipeline family.
# Runs ANY coding-hermes pipeline (qa, dogfooding, duckbrain-sync, pm, ...)
# with a fresh serve + fresh DB + free-port scan, exactly like the flagship
# drivers — so every role we run is a pipeline with consistent testing and
# far lower usage than the LLM-driven skill crons.
#
# Usage:
#   dagger-role-tick.sh <pipeline> [project] [extra_env_csv]
#
#   pipeline     — qa | dogfooding | duckbrain-sync | pm | bankai | plus-ultra
#   project      — fleet project name (default: derived from pipeline)
#   extra_env_csv— optional "K=V,K2=V2" added to DAGGER_ENV (overrides)
#
# Env contract (all optional, defaults shown):
#   REAL_MODE    — 0 (default): dry-run / report-only, no board/ns writes
#                  1: perform writes (file findings, ns keys, scratch)
#   DAGGER_BIN   — default $DAGGER_HOME/dagger
#   DAGGER_HOME  — default $HOME/Hermes-DAGger/hermes-dagger
#   ROLE_PORT    — force a port (default: free-port scan 11000-16999)
#   DELIVER      — print the report to stdout (cron delivery) — default yes
#
# Per-pipeline env wiring:
#   qa            TARGETED: QA_TARGETS=comma-list (or QA_PROJECT=single)
#                 RANDOM (default): qa_discover.py picks QA_DISCOVER_N (def 2)
#                 QA_SKIP_BUNKER=1 → fast local probe instead of JIT bunker
#   dogfooding    TARGETED: DOGFOOD_TARGETS=comma-list (or DOGFOOD_PROJECT)
#                 DISCOVERY: DOGFOOD_DISCOVER=1 (dogfood-pick.py --dry-run)
#                 LEGACY: DOGFOOD_PROJECTS comma list, DOGFOOD_FIXED=1 first
#   duckbrain-sync SYNC_PROJECT / SYNC_REPO / SYNC_NS (default: project)
#                 DUCKBRAIN_URL / DUCKBRAIN_KEY (defaults :3000, hermes)
#   pm            PM_TARGETS / PM_PROJECT = targeted gap-hunt (else fleet-wide)
#                 LEDGER_PATH / INITIATIVES_PATH / VERIFICATIONS_PATH /
#                 PROPOSALS_HISTORY_PATH / ESCALATIONS_PATH (stand-in files)
#   bankai        BANKAI_TARGETED=1 → BANKAI_PROJECT/REPO flow through as
#                 the targeted project (default when project is passed)
#   plus-ultra    bankai loop + Beyond Pass; BANKAI_PROJECT + REAL_MODE drive
set -u

PIPELINE="${1:-}"
PROJECT="${2:-}"
EXTRA_ENV="${3:-}"
[ -z "$PIPELINE" ] && { echo "dagger-role-tick: pipeline required (qa|dogfooding|duckbrain-sync|pm|bankai|plus-ultra)" >&2; exit 2; }

DAGGER_HOME="${DAGGER_HOME:-$HOME/Hermes-DAGger/hermes-dagger}"
DAGGER_BIN="${DAGGER_BIN:-$DAGGER_HOME/dagger}"
REAL_MODE="${REAL_MODE:-0}"
[ -x "$DAGGER_BIN" ] || { echo "dagger-role-tick: $DAGGER_BIN not found" >&2; exit 2; }

# ── gateway key (v3 pipelines call agent()/llm()) ───────────────────────
# Same resolution chain as dagger-daily.sh: GATEWAY_API_KEY > HERMES_API_KEY
# > API_SERVER_KEY in ~/.hermes/.env. Exported for BOTH serve and run.
if [ -z "${GATEWAY_API_KEY:-}" ]; then
  GATEWAY_API_KEY="$(grep -oE '^GATEWAY_API_KEY=.*' "$HOME/.hermes/.env" 2>/dev/null | head -1 | cut -d= -f2-)"
  [ -z "$GATEWAY_API_KEY" ] && GATEWAY_API_KEY="$(grep -oE '^HERMES_API_KEY=.*' "$HOME/.hermes/.env" 2>/dev/null | head -1 | cut -d= -f2-)"
  [ -z "$GATEWAY_API_KEY" ] && GATEWAY_API_KEY="$(grep -oE '^API_SERVER_KEY=.*' "$HOME/.hermes/.env" 2>/dev/null | head -1 | cut -d= -f2-)"
fi
[ -n "$GATEWAY_API_KEY" ] || { echo "dagger-role-tick: no GATEWAY_API_KEY/HERMES_API_KEY/API_SERVER_KEY in ~/.hermes/.env (v3 pipelines need it for agent calls)" >&2; exit 1; }
export GATEWAY_API_KEY

# ── per-pipeline defaults ─────────────────────────────────────────────
case "$PIPELINE" in
  qa)
    [ -z "$PROJECT" ] && PROJECT="qa"
    # v3: QA_TARGETS (comma) or QA_PROJECT (single) = targeted; empty = random
    # discovery (qa_discover.py). Pass the target through only when named.
    if [ -n "$PROJECT" ] && [ "$PROJECT" != "qa" ]; then
      ENV_BASE="QA_TARGETS=$PROJECT,QA_REPO=/home/kara/$PROJECT,REAL_MODE=$REAL_MODE"
    else
      ENV_BASE="QA_DISCOVER_N=${QA_DISCOVER_N:-2},REAL_MODE=$REAL_MODE"
    fi
    ;;
  dogfooding)
    [ -z "$PROJECT" ] && PROJECT="dogfooding"
    # v3: DOGFOOD_TARGETS (comma) = targeted; DOGFOOD_DISCOVER=1 = discovery
    # picker; DOGFOOD_PROJECTS = legacy random.
    if [ -n "$PROJECT" ] && [ "$PROJECT" != "dogfooding" ]; then
      ENV_BASE="DOGFOOD_TARGETS=$PROJECT,REAL_MODE=$REAL_MODE"
    elif [ "${DOGFOOD_DISCOVER:-0}" = "1" ]; then
      ENV_BASE="DOGFOOD_DISCOVER=1,REAL_MODE=$REAL_MODE"
    else
      ENV_BASE="DOGFOOD_PROJECTS=${DOGFOOD_PROJECTS:-crier,helios,hivemind,ring-runner,imhotep,speclang},DOGFOOD_FIXED=${DOGFOOD_FIXED:-0},REAL_MODE=$REAL_MODE"
    fi
    ;;
  duckbrain-sync)
    [ -z "$PROJECT" ] && PROJECT="duckbrain"
    # DUCKBRAIN_KEY resolved HOST-SIDE (2026-09-01): the key passes through
    # env() (Go bridge, byte-exact) instead of the LLM-mediated jq lookup —
    # the mediator corrupted even that single-line output (embedded newline →
    # net/http "invalid header field value"), which explains the historical
    # 3-of-6 401 fleet-wide. E2E rounds 4-5 evidence.
    if [ -z "${DUCKBRAIN_KEY:-}" ] && [ -f "$HOME/.duckbrain/auth.json" ]; then
      # post DB-SUPA-4: auth.json is keyHash-only; sidecar .token is primary
      KEYFILE="$HOME/.duckbrain/token-1787620243982.token"
      [ -r "$KEYFILE" ] && DUCKBRAIN_KEY="$(tr -d '[:space:]' < "$KEYFILE")"
      if [ -z "$DUCKBRAIN_KEY" ]; then DUCKBRAIN_KEY="$(jq -r '.apiKeys[] | select(.name=="token-1787620243982") | .key' "$HOME/.duckbrain/auth.json" 2>/dev/null | head -1 | tr -d '[:space:]')"; fi
    # torn auth.json reads (mid-rotation) once produced an invalid X-Api-Key header; strip + sanity-check
    case "$DUCKBRAIN_KEY" in "" | *"$"\n*) ;; esac
    if [ -n "$DUCKBRAIN_KEY" ] && [ "${#DUCKBRAIN_KEY}" -lt 16 ]; then echo "dagger-role-tick: DUCKBRAIN_KEY suspiciously short (${#DUCKBRAIN_KEY}) — refusing" >&2; DUCKBRAIN_KEY=""; fi
    fi
    ENV_BASE="SYNC_PROJECT=$PROJECT,SYNC_REPO=${SYNC_REPO:-/home/kara/$PROJECT},SYNC_NS=${SYNC_NS:-$PROJECT},DUCKBRAIN_URL=${DUCKBRAIN_URL:-http://127.0.0.1:3000},DUCKBRAIN_KEY=${DUCKBRAIN_KEY:-},REAL_MODE=$REAL_MODE"
    ;;
  pm)
    [ -z "$PROJECT" ] && PROJECT="stand-in"
    # v2: PM_TARGETS/PM_PROJECT = targeted gap-hunt; empty = fleet-wide.
    PM_SCOPE=""
    if [ -n "$PROJECT" ] && [ "$PROJECT" != "stand-in" ]; then
      PM_SCOPE="PM_TARGETS=$PROJECT,"
    elif [ -n "${PM_TARGETS:-}" ]; then
      PM_SCOPE="PM_TARGETS=$PM_TARGETS,"
    fi
    ENV_BASE="${PM_SCOPE}DIGEST_BUNDLE_PATH=${DIGEST_BUNDLE_PATH:-/home/kara/.hermes/stand-in/digest-input.json},VERIFICATIONS_PATH=${VERIFICATIONS_PATH:-/home/kara/.hermes/stand-in/proposal-verifications.jsonl},PROPOSALS_HISTORY_PATH=${PROPOSALS_HISTORY_PATH:-/home/kara/.hermes/stand-in/dagger-proposals.jsonl},ESCALATIONS_PATH=${ESCALATIONS_PATH:-/home/kara/.hermes/stand-in/escalations.jsonl},REAL_MODE=$REAL_MODE"
    ;;
  bankai|plus-ultra)
    [ -z "$PROJECT" ] && PROJECT="bankai"
    # Both drive loops are TARGETED by design: BANKAI_PROJECT names the target.
    if [ "$PIPELINE" = "bankai" ]; then
      ENV_BASE="BANKAI_PROJECT=$PROJECT,BANKAI_REPO=/home/kara/$PROJECT,BANKAI_MAX_PASSES=${BANKAI_MAX_PASSES:-3},BANKAI_REAL_MODE=$REAL_MODE"
    else
      # plus-ultra: REAL_MODE also gates the Beyond Pass writes; bankai drive
      # follows the same flag (drive+write together — the 越 contract).
      ENV_BASE="BANKAI_PROJECT=$PROJECT,BANKAI_REPO=/home/kara/$PROJECT,BANKAI_MAX_PASSES=${BANKAI_MAX_PASSES:-3},BANKAI_REAL_MODE=$REAL_MODE,REAL_MODE=$REAL_MODE,PU_MAX_PROPOSALS=${PU_MAX_PROPOSALS:-4}"
    fi
    ;;
  sub-scout)
    [ -z "$PROJECT" ] && PROJECT="sub-scout"
    ENV_BASE="SCOUT_INPUT_PATH=${SCOUT_INPUT_PATH:-/home/kara/.hermes/sub-scout/sub-scout-input.json},SCOUT_HISTORY_PATH=${SCOUT_HISTORY_PATH:-/home/kara/.hermes/sub-scout/proposals.jsonl},SCOUT_SCRATCH_PATH=${SCOUT_SCRATCH_PATH:-/home/kara/.hermes/sub-scout/proposals-scratch.jsonl},REAL_MODE=$REAL_MODE"
    ;;
  *)
    echo "dagger-role-tick: unknown pipeline '$PIPELINE'" >&2
    exit 2
    ;;
esac
[ -n "$EXTRA_ENV" ] && ENV_BASE="$ENV_BASE,$EXTRA_ENV"

# DAGGER-115: carry the scheduler/role identity through DAGGER_ENV as part
# of the run context. CODING_HERMES_PROJECT is the scheduler row name;
# PROJECT is the base target after role-suffix resolution by the wrapper.
# Manual role runs keep their honest d5s/ts source fallback.
ENV_BASE="$ENV_BASE,DAGGER_ROLE=$PIPELINE,DAGGER_PROJECT=$PROJECT"
if [ -n "${CODING_HERMES_TICK:-}" ]; then
  ENV_BASE="$ENV_BASE,CODING_HERMES_TICK=$CODING_HERMES_TICK,CODING_HERMES_PROJECT=${CODING_HERMES_PROJECT:-$PROJECT},DAGGER_SCHEDULER_ROW=${CODING_HERMES_PROJECT:-$PROJECT},DAGGER_SOURCE=scheduler"
fi

# ── tool-executor model (Bane 2026-08-31) ──────────────────────────────
# Every role pipeline makes raw tool() calls (terminal/git/file); without
# DAGGER_TOOL_MODEL the bridge sends no provider field and the gateway
# bills its PAYG default. Resolve the router head for the target project
# (sync/qa roles resolve P8_SYNC-style cheap lanes via the project profile;
# non-fleet virtual projects fall back to P8_SYNC). Fail-open: empty on
# router error → bridge keeps the hermes-agent default.
if [ -z "${DAGGER_TOOL_MODEL:-}" ]; then
  ROLE_RESOLVE_TARGET="$PROJECT"
  case "$PIPELINE" in
    duckbrain-sync) ROLE_RESOLVE_TARGET="duckbrain-sync" ;;
    qa)             ROLE_RESOLVE_TARGET="qa" ;;
  esac
  ROLE_TOOL_HEAD="$(
    "${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" \
      "$HOME/.hermes/scripts/router_spawn.py" "$ROLE_RESOLVE_TARGET" --format json 2>/dev/null \
    | "${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" -c '
import json, sys
try:
    d = json.load(sys.stdin); h = d.get("head") or {}
    if h.get("provider") and h.get("model"):
        print(h["provider"] + "/" + h["model"])
except Exception:
    pass'
  )"
  # FAIL-CLOSED for the pin (Bane 09-01): an EMPTY DAGGER_TOOL_MODEL makes
  # tool() calls go through the gateway LLM — prose-wrapped outputs,
  # hallucinated command results, and PAYG-default (main key) billing.
  # Registry miss (e.g. "qa" not seeded) → fall back to the P8_SYNC lane,
  # then to the deepseek-foreman lane. NEVER empty.
  if [ -z "$ROLE_TOOL_HEAD" ]; then
    case "$ROLE_RESOLVE_TARGET" in
      duckbrain-sync) ROLE_TOOL_HEAD="deepseek-foreman/deepseek-v4-flash" ;;
      *)              ROLE_TOOL_HEAD="$("${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" \
                            "$HOME/.hermes/scripts/router_spawn.py" duckbrain-sync --format json 2>/dev/null \
                          | "${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" -c '
import json, sys
try:
    d = json.load(sys.stdin); h = d.get("head") or {}
    if h.get("provider") and h.get("model"):
        print(h["provider"] + "/" + h["model"])
except Exception:
    pass')"
    esac
  fi
  [ -z "$ROLE_TOOL_HEAD" ] && ROLE_TOOL_HEAD="deepseek-foreman/deepseek-v4-flash"
  export DAGGER_TOOL_MODEL="$ROLE_TOOL_HEAD"
fi

# ── inbound REST bearer token (DAGGER-133, 2026-09-12) ────────────────
# DAGGER-133 made `dagger serve` fail-closed: no DAGGER_API_TOKEN → serve
# aborts instantly and every role lane dies before any node runs (fleet
# outage 09:51–17:00 local 2026-09-12, found by cron ca45e1bae39a 16:10Z).
# Serve AND the `dagger run` client both read DAGGER_API_TOKEN from the
# process env, so a fresh per-run random token exported here authenticates
# the pair. Ephemeral + localhost-only beats a static shared secret; the
# product's fail-closed default stays intact for non-wrapper serves.
DAGGER_API_TOKEN="$(openssl rand -hex 32)"
export DAGGER_API_TOKEN

# ── fresh serve + fresh DB + free-port scan ───────────────────────────
PORT="${ROLE_PORT:-}"
if [ -z "$PORT" ]; then
  for p in $(seq 11000 16999 | shuf --random-source=/dev/urandom | head -40); do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then PORT=$p; break; fi
    exec 3>&- 2>/dev/null
  done
fi
[ -n "$PORT" ] || { echo "dagger-role-tick: no free port in 11000-16999" >&2; exit 1; }

RUN_DIR="/tmp/dagger-role-$PIPELINE-$$-$(date +%s%N)"
mkdir -p "$RUN_DIR"
DB="$RUN_DIR/role.db"
RUN_LOG="$RUN_DIR/run.log"
RUN_JSONL="$RUN_DIR/run.jsonl"

cd "$DAGGER_HOME" || exit 1
"$DAGGER_BIN" serve --addr ":$PORT" --db "$DB" >"$RUN_DIR/serve.log" 2>&1 &
SRV=$!

up=0
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -K <(printf 'header = "Authorization: Bearer %s"\n' "$DAGGER_API_TOKEN") "http://127.0.0.1:$PORT/health" 2>/dev/null || echo 000)
  [ "$code" = "200" ] && { up=1; break; }
  sleep 0.5
done
if [ "$up" -ne 1 ]; then
  echo "dagger-role-tick: serve failed on :$PORT"
  tail -5 "$RUN_DIR/serve.log"
  kill -9 "$SRV" 2>/dev/null
  exit 1
fi

# ── run the pipeline ───────────────────────────────────────────────────
export DAGGER_ENV="$ENV_BASE"
"$DAGGER_BIN" run --fresh --server "http://127.0.0.1:$PORT" \
  --log-file "$RUN_JSONL" --log-level info "examples/coding-hermes/$PIPELINE.ts" >"$RUN_LOG" 2>&1
RUN_RC=$?
kill -9 "$SRV" 2>/dev/null
wait "$SRV" 2>/dev/null

# ── report (human depth, not raw node tables) ─────────────────────────
# dagger-role-report.py reads the run DB checkpoints and prints a human
# card (verdict headline, per-target findings, verified counts). The raw
# node table + full log stay in $RUN_LOG — the footer points at it.
python3 /home/kara/.hermes/scripts/dagger-role-report.py "$DB" "$RUN_LOG" "$PIPELINE" "$PROJECT" "${CODING_HERMES_TICK:-}"
if ! grep -q "Failed: 0" "$RUN_LOG" 2>/dev/null; then
  echo ""
  echo "pipeline exit $RUN_RC — run log tail:"
  tail -15 "$RUN_LOG"
fi
exit $RUN_RC
