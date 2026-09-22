#!/bin/bash
# pm-standin-tick.sh — scheduler custom-command tick for the DAGGER PM stand-in
# (Bane cutover 2026-09-01). Spawned by coding-hermes-scheduler as a project
# command (same mechanism as scheduler-foreman-tick.sh) — THE SCHEDULER owns
# the timer/cooldown/trigger identity, no bespoke cron.
#
# Per-project design (Bane 2026-09-01): one scheduler row per project, named
# "<project>-pm" (e.g. "helios-pm"). The row name IS the target: this script
# derives PM_TARGET by stripping the -pm suffix and the pipeline proposes ONLY
# for that project. Cooldowns are per-row (86400 daily; 3/5/7-day variants
# just use bigger cooldown_s values on that row).
#
#   - runs the pm.ts dagger pipeline with REAL_MODE=1 + PM_TARGET=<project>
#   - converts push-scratch proposals → REAL board rows (INSERT-before-event)
#   - HARD-FILTERS proposals to the target project (agent scope belt+braces)
#   - appends ledger entries (status=added)
#   - writes DuckBrain keys under the dedicated 'stand-in-pm' namespace
#
# Scheduler contract (spawn.go custom-command path):
#   cmd.Dir = project workdir; env gets CODING_HERMES_TICK / _SOURCE / _PROJECT
set -u

PROJECT="${CODING_HERMES_PROJECT:-pm-standin}"
TICK="${CODING_HERMES_TICK:-manual-$(date +%s)}"
DAGGER_HOME="/home/kara/hermes-dagger"
STANDIN="$HOME/.hermes/stand-in"

# PM_TARGET = row name minus the trailing "-pm" (helios-pm → helios).
PM_TARGET="${CODING_HERMES_PROJECT:-pm-standin}"
PM_TARGET="${PM_TARGET%-pm}"
[ "$PM_TARGET" = "${CODING_HERMES_PROJECT:-pm-standin}" ] && PM_TARGET="${CODING_HERMES_PM_TARGET:-}"
export PM_TARGET
export PM_PROJECT="$PM_TARGET"   # router profile resolve for the agent head

export LEDGER="$STANDIN/ledger.json"
export INITIATIVES="$STANDIN/initiatives.json"
export DAGGER_PROPOSALS_PATH="$STANDIN/dagger-proposals.jsonl"
export DAGGER_PROPOSALS_SCRATCH="$STANDIN/dagger-proposals-scratch.jsonl"
export VERIFICATIONS_PATH="$STANDIN/proposal-verifications.jsonl"
export PROPOSALS_HISTORY_PATH="$DAGGER_PROPOSALS_PATH"
export ESCALATIONS_PATH="$STANDIN/escalations.jsonl"
export ESCALATIONS_SCRATCH_PATH="$STANDIN/escalations-scratch.jsonl"
export PUSH_SCRATCH_PATH="$STANDIN/push-scratch-$PM_TARGET.jsonl"
export DIGEST_BUNDLE_PATH="$STANDIN/digest-input.json"
export PM_NAMESPACE="stand-in-pm"     # dedicated DuckBrain namespace (Bane)
export REAL_MODE="${REAL_MODE:-1}"    # deploy mode: proposals become REAL board rows

KEY=$(grep -oE '^GATEWAY_API_KEY=.*' ~/.hermes/.env 2>/dev/null | head -1 | cut -d= -f2-)
[ -z "$KEY" ] && KEY=$(grep -oE '^API_SERVER_KEY=.*' ~/.hermes/.env 2>/dev/null | head -1 | cut -d= -f2-)
[ -z "$KEY" ] && { echo "PM-STANDIN: no GATEWAY_API_KEY/API_SERVER_KEY in ~/.hermes/.env"; exit 1; }

# Target sanity: resolve the BASE project's REAL workdir from the scheduler
# DB (names can differ from paths — ai-plays-poke → ~/ai_plays_poke), then
# require it to exist. Unknown project → skip the run (misnamed row).
PM_WORKDIR=""
if [ -n "$PM_TARGET" ] && [ -f "$HOME/.hermes/coding-hermes/scheduler.db" ]; then
  PM_WORKDIR=$(sqlite3 "$HOME/.hermes/coding-hermes/scheduler.db" \
    "SELECT workdir FROM projects WHERE name='$PM_TARGET' LIMIT 1;" 2>/dev/null | head -1)
fi
[ -z "$PM_WORKDIR" ] && [ -n "$PM_TARGET" ] && PM_WORKDIR="/home/kara/$PM_TARGET"
if [ -n "$PM_TARGET" ] && [ ! -d "$PM_WORKDIR" ]; then
  echo "PM-STANDIN: target '$PM_TARGET' workdir '$PM_WORKDIR' missing — skipping"
  exit 0
fi
export PM_WORKDIR

# DAGGER_TOOL_MODEL (DAGGER-130 fail-closed; bridge 422f292/1f6e8c0): raw
# tool() calls inside the pipeline REQUIRE an explicit provider/model route —
# the bridge rejects an empty one BEFORE any egress instead of billing the
# gateway PAYG default. Resolve the project's router head (the same lane the
# agent nodes bill via PM_PROJECT) and export it for the bridge's Tool().
export DAGGER_TOOL_MODEL="$(
  "${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" \
    "$HOME/.hermes/scripts/router_spawn.py" "$PM_TARGET" --format json 2>/dev/null \
  | "${BOARD_VENV_PY:-$HOME/.hermes/venvs/board/bin/python3}" -c '
import json, sys
try:
    d = json.load(sys.stdin); h = d.get("head") or {}
    if h.get("provider") and h.get("model"):
        print(h["provider"] + "/" + h["model"])
except Exception:
    pass
' 2>/dev/null
)"
if [ -z "$DAGGER_TOOL_MODEL" ]; then
  echo "PM-STANDIN: FATAL: router resolved no tool() lane for project '${PM_TARGET:-<unset>}' (DAGGER_TOOL_MODEL empty) — refusing a tick the fail-closed bridge would reject at the first tool() node (DAGGER-130)." >&2
  exit 1
fi

# ---- generate the digest bundle (GAP-049 split). Classification lives in the
# ledger reconciler (ledger_board_reconcile.py) so the digest and the
# reconciler share ONE implementation. The reconciler only ever READS the
# ledger / scheduler.db (read-only URI) / boards; it writes just the digest
# bundle. If the reconciler is unavailable the legacy conflation generator
# below is used as a labelled fallback so the tick still produces output.
python3 - "$LEDGER" "$INITIATIVES" "$DIGEST_BUNDLE_PATH" \
  "$STANDIN/ledger_board_reconcile.py" <<'PYEOF'
import json, sys, datetime
ledger_path, init_path, out_path, reconciler_path = sys.argv[1:5]
now = datetime.datetime.now(datetime.timezone.utc)

def legacy_digest():
    """Pre-GAP-049 conflation generator — fallback only (honestly labelled)."""
    try:
        led = json.load(open(ledger_path))
    except Exception as e:
        print(json.dumps({"error": f"ledger read failed: {e}", "digest_valid": False}))
        return None, None
    items = led.get("items", led if isinstance(led, list) else [])
    counts = {}
    for it in items:
        counts[it.get("status", "?")] = counts.get(it.get("status", "?"), 0) + 1
    oldest = []
    stuck = []
    for it in items:
        if it.get("status") not in ("verified", "complete", "stale"):
            at = it.get("added_at", "")
            oldest.append({"id": it.get("id"), "project": it.get("project"),
                           "title": (it.get("title") or "")[:80], "status": it.get("status"), "added_at": at})
            try:
                d = datetime.datetime.fromisoformat(at.replace("Z", "+00:00"))
                if (now - d).total_seconds() > 48 * 3600:
                    stuck.append({"id": it.get("id"), "project": it.get("project"),
                                  "title": (it.get("title") or "")[:80], "age_h": round((now - d).total_seconds() / 3600, 1)})
            except Exception:
                pass
    oldest.sort(key=lambda x: x.get("added_at", ""))
    current_added = [{"id": it.get("id"), "project": it.get("project"),
                      "title": (it.get("title") or "")[:100]} for it in items if it.get("status") == "added"]
    try:
        ini = json.load(open(init_path))
        init_status = [(i.get("id"), i.get("status"), (i.get("last_updated") or "")[:19]) for i in ini.get("initiatives", [])]
    except Exception as e:
        init_status = [("ERROR", str(e), "")]
    bundle = {
        "generated_at": now.isoformat(),
        "ledger_total": len(items),
        "counts": counts,
        "oldest_unverified": oldest[:5],
        "stuck_48h": stuck[:8],
        "stuck_count": len(stuck),
        "current_added": current_added,
        "initiatives": init_status,
        "digest_valid": True,
        "reconciliation_available": False,
        "digest_generator": "legacy-conflation (pre-GAP-049 fallback)",
    }
    json.dump(bundle, open(out_path, "w"), indent=1)
    return len(items), len(stuck)

try:
    import importlib.util
    spec = importlib.util.spec_from_file_location("ledger_board_reconcile", reconciler_path)
    lbr = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(lbr)
    loader = lbr.board_loader_from_resolver(lbr.ProjectResolver(lbr.SCHEDULER_DB_DEFAULT))
    bundle = lbr.digest_input(ledger_path, init_path, out_path, board_loader=loader, now=now)
    rec = bundle.get("reconciliation")
    if rec:
        cats = rec["categories"]
        print("digest bundle: {n} items, counts={c}, drift_closable={dc} "
              "board_open_work={bo} stale_drift={sd} no_id_match={nm} "
              "stuck_count={sc} (stuck = board_open_work only)".format(
                  n=bundle["ledger_total"], c=bundle["counts"],
                  dc=cats["drift_closable"], bo=cats["board_open_work"],
                  sd=cats["stale_drift"], nm=cats["no_id_match"],
                  sc=bundle["stuck_count"]))
    else:
        print("digest bundle: reconciler imported but unavailable — legacy "
              "conflation digest (stuck_count={})".format(bundle.get("stuck_count")))
except Exception as e:
    print("PM-STANDIN: GAP-049 digest unavailable ({}: {}) — legacy fallback".format(
        type(e).__name__, e))
    n, sc = legacy_digest()
    if n is not None:
        print(f"digest bundle: {n} items, stuck={sc} (legacy conflation)")
PYEOF

# Free port per run (no stale-serve reuse).
PORT=$(python3 - <<'PYEOF'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PYEOF
)
DB="$STANDIN/pm-standin-$(date +%s).db"
# DAGGER-133 (36e4041): every REST route incl. /health requires bearer auth.
# Per-run token, same pattern as demo/stand-in-pm-run.sh; `dagger run` picks
# it up from the env (src/runner/runner.go) to authenticate against serve.
DAGGER_API_TOKEN=${DAGGER_API_TOKEN:-$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')}
export DAGGER_API_TOKEN
rm -f /tmp/pm-standin-serve.log
cd "$DAGGER_HOME" || exit 1
env GATEWAY_API_KEY="$KEY" DAGGER_API_TOKEN="$DAGGER_API_TOKEN" ./dagger serve --addr ":$PORT" --db "$DB" --tier 2 >/tmp/pm-standin-serve.log 2>&1 &
SERVE_PID=$!
OK=0
for _ in $(seq 1 25); do
  if curl -s -m 2 -H "Authorization: Bearer $DAGGER_API_TOKEN" "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 && kill -0 "$SERVE_PID" 2>/dev/null; then
    OK=1
    break
  fi
  sleep 1
done
[ "$OK" = "1" ] || { echo "PM-STANDIN: serve failed on :$PORT"; kill -9 "$SERVE_PID" 2>/dev/null; exit 1; }

# DBKEY for the DAGGER pipeline: pass the DuckBrain key via DAGGER_ENV so
# pm.ts trace_duck_write uses env("DUCKBRAIN_KEY") instead of the fragile
# LLM-mediated tool() echo of the .token file (2026-09-16 run: mediator
# returned prose + control chars → net/http "invalid header field value for
# X-Api-Key" 502, 3 attempts, verify chain skipped). Empty key → pm.ts
# falls back to the old lookup path.
DBKEY=$(tr -d '[:space:]' < "$HOME/.duckbrain/token-1787620243982.token" 2>/dev/null || true)
export DAGGER_ENV="LEDGER_PATH=$LEDGER,INITIATIVES_PATH=$INITIATIVES,DAGGER_PROPOSALS_SCRATCH=$DAGGER_PROPOSALS_SCRATCH,VERIFICATIONS_PATH=$VERIFICATIONS_PATH,PROPOSALS_HISTORY_PATH=$PROPOSALS_HISTORY_PATH,ESCALATIONS_PATH=$ESCALATIONS_PATH,ESCALATIONS_SCRATCH_PATH=$ESCALATIONS_SCRATCH_PATH,PUSH_SCRATCH_PATH=$PUSH_SCRATCH_PATH,DIGEST_BUNDLE_PATH=$DIGEST_BUNDLE_PATH,REAL_MODE=$REAL_MODE,PM_NAMESPACE=$PM_NAMESPACE,PM_TARGET=$PM_TARGET,DUCKBRAIN_KEY=$DBKEY"
env GATEWAY_API_KEY="$KEY" ./dagger run examples/coding-hermes/pm.ts --server "http://127.0.0.1:$PORT" \
  --log-file "$STANDIN/pm-standin.jsonl" --log-level info >/tmp/pm-standin-run.log 2>&1
RUN_RC=$?

# Scratch → history accumulation (shell owns the append).
# BANE 2026-09-16 (tick coding-hermes-scheduler-pm-2026-09-16-00-45-32):
# accumulators are ARCHIVE-then-TRUNCATE — a bare append re-filed the same
# scratch batch every run (escalations.jsonl hit 145 lines / 13 unique; the
# same escalation re-appended 85x). Archive to .consumed, then truncate.
if [ -s "$DAGGER_PROPOSALS_SCRATCH" ]; then
  grep -v '^File not found' "$DAGGER_PROPOSALS_SCRATCH" >> "$DAGGER_PROPOSALS_PATH"
  cat "$DAGGER_PROPOSALS_SCRATCH" >> "$DAGGER_PROPOSALS_PATH.consumed" 2>/dev/null
  : > "$DAGGER_PROPOSALS_SCRATCH"
fi
if [ -s "$ESCALATIONS_SCRATCH_PATH" ]; then
  grep -v '^File not found' "$ESCALATIONS_SCRATCH_PATH" >> "$ESCALATIONS_PATH"
  cat "$ESCALATIONS_SCRATCH_PATH" >> "$ESCALATIONS_PATH.consumed" 2>/dev/null
  : > "$ESCALATIONS_SCRATCH_PATH"
fi

# GAP-020: run log is truth (branch-skipped nodes exit 1 even on success).
if grep -q "Failed: 0" /tmp/pm-standin-run.log 2>/dev/null; then
  RUN_RC=0
fi

# ---- DEPLOY: push-scratch → REAL board rows + ledger entries ----
# HARD FILTER: when PM_TARGET is set, only proposals for that project pass.
# SCHED-GAP-207: the leg moved from an inline heredoc to ops/pm-standin/
# push_proposals.py (deployed beside this script) so its writer rules are
# unit-testable:
# (1) UNIQUE ID PER FILED FINDING — a proposal whose title overlaps an OPEN
#     row's title is SUPPRESSED (annotated to events.jsonl as
#     refile_suppressed), never appended as a sibling under the same id;
# (2) id continuation continues after the highest suffix ever used, and the
#     mint loop skips past any occupied id — this writer can no longer
#     create an id holding two open rows (the 29%-of-open-rows id-slot
#     class).
PUSH_OUT_FILE=/tmp/pm-standin-push.out
rm -f "$PUSH_OUT_FILE"
if [ -s "$PUSH_SCRATCH_PATH" ] && [ "$REAL_MODE" = "1" ]; then
  python3 "$SCRIPT_DIR/push_proposals.py" \
    "$HOME/$PM_TARGET/.coding-hermes/board/tasks.jsonl" \
    "$HOME/$PM_TARGET/.coding-hermes/board/events.jsonl" \
    "$PUSH_SCRATCH_PATH" "$LEDGER" "$PM_TARGET" --apply > "$PUSH_OUT_FILE" 2>&1
  # archive the consumed batch so it never re-pushes (shell owns the append)
  cat "$PUSH_SCRATCH_PATH" >> "$PUSH_SCRATCH_PATH.consumed" 2>/dev/null
  : > "$PUSH_SCRATCH_PATH"
fi
PUSHED=0
PUSH_DETAIL=""
if [ -s "$PUSH_OUT_FILE" ]; then
  # SCHED-GAP-207: push_proposals.py prints a JSON result line whose "filed"
  # array carries one entry per row actually appended (the old inline leg's
  # PUSHED:[...] format — the quote-count arithmetic below counted id+title
  # pairs, 4 quotes per row).
  PUSH_DETAIL=$(grep -oE '"filed": \[.*\]' "$PUSH_OUT_FILE" | head -1 | cut -d: -f2-)
  [ -z "$PUSH_DETAIL" ] && PUSH_DETAIL=$(grep -E 'PUSH-ERROR|Traceback' "$PUSH_OUT_FILE" | head -1)
  if [ -n "$PUSH_DETAIL" ]; then
    NQ=$(echo "$PUSH_DETAIL" | grep -o '"id"' | wc -l)
    PUSHED=$NQ
  fi
fi

# ---- DuckBrain keys under the dedicated namespace (loose verify) ----
# Endpoint/auth per the duckbrain-sync dagger: POST /api/memories?namespace=<ns>
# with X-API-Key = the named all-ns token from ~/.duckbrain/auth.json.
DBKEY=""
TOKEN_FILE="$HOME/.duckbrain/token-1787620243982.token"
# post DB-SUPA-4: auth.json is keyHash-only, plaintext lives in sidecar .token
[ -r "$TOKEN_FILE" ] && DBKEY=$(tr -d '[:space:]' < "$TOKEN_FILE")
if [ -z "$DBKEY" ]; then
DBKEY=$(python3 -c "
import json, os
try:
    a = json.load(open(os.path.expanduser('~/.duckbrain/auth.json')))
    k = ''
    for it in (a.get('apiKeys') or []):
        if it.get('name') == 'token-1787620243982':
            k = it.get('key', '')
            break
    print(k or ((a.get('apiKeys') or [{}])[0].get('key', '')))
except Exception:
    print('')
")
fi
DB_WRITE=0
DB_VERIFY=0
if [ -n "$DBKEY" ]; then
  SUMMARY=$(strings /tmp/pm-standin-run.log 2>/dev/null | tail -20 | head -c 500)
  NOW=$(date -u +%FT%TZ)
  # Round-trip fingerprint: the read-back MUST contain it, else the write
  # is treated as lost (DuckBrain has a proven silent-loss class).
  FP="PMFP-$(date -u +%Y%m%dT%H%M%S)-$$"
  BODY=$(PM_TARGET="$PM_TARGET" TICK="$TICK" RUN_RC="$RUN_RC" SUMMARY="$SUMMARY" NOW="$NOW" FP="$FP" python3 -c "
import json, os
print(json.dumps({
    'key': '/pm/' + os.environ['PM_TARGET'] + '/last-run',
    'domain': 'raw_note',
    'content': json.dumps({
        'ts': os.environ['NOW'], 'tick': os.environ['TICK'],
        'run_rc': int(os.environ['RUN_RC']), 'summary': os.environ['SUMMARY'][:400],
        'fingerprint': os.environ['FP']
    }),
    'attributes': {}
}))
")
  HTTP=$(curl -s -m 15 -o /tmp/pm-standin-db.out -w "%{http_code}" -X POST "http://127.0.0.1:3000/api/memories?namespace=$PM_NAMESPACE" \
    -H "X-API-Key: $DBKEY" -H "Content-Type: application/json" \
    -d "$BODY" 2>/dev/null)
  [ "$HTTP" = "200" ] || [ "$HTTP" = "201" ] && DB_WRITE=1
  # Read-back verify (bare-fingerprint match survives JSON escaping).
  if [ "$DB_WRITE" = "1" ]; then
    ENCP=$(python3 -c "import urllib.parse,os;print(urllib.parse.quote('/pm/'+os.environ['PM_TARGET']+'/last-run',safe=''))" PM_TARGET="$PM_TARGET")
    RB=$(curl -s -m 15 "http://127.0.0.1:3000/api/memories?prefix=$ENCP&namespace=$PM_NAMESPACE&limit=3" -H "X-API-Key: $DBKEY" 2>/dev/null)
    case "$RB" in
      *"$FP"*) DB_VERIFY=1 ;;
    esac
  fi
fi

kill -9 "$SERVE_PID" 2>/dev/null
wait "$SERVE_PID" 2>/dev/null

# Human report first (proposal titles, escalation flag); mechanics to stderr.
echo "PM-STANDIN TICK $TICK — $(date -u +%Y-%m-%dT%H:%MZ)"
python3 /home/kara/.hermes/scripts/dagger-role-report.py "$DB" /tmp/pm-standin-run.log pm "${PM_TARGET:-stand-in}"
echo "project=$PROJECT target=$PM_TARGET pipeline_rc=$RUN_RC real_mode=$REAL_MODE board_rows_pushed=$PUSHED db_write=$DB_WRITE db_verify=$DB_VERIFY ns=$PM_NAMESPACE" >&2
[ -n "$PUSH_DETAIL" ] && echo "pushed: $PUSH_DETAIL" >&2
exit 0
