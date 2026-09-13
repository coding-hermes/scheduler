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

# ---- generate the COMPLETE digest bundle (same generator as dagger-daily.sh) ----
python3 - "$LEDGER" "$INITIATIVES" "$DIGEST_BUNDLE_PATH" <<'PYEOF'
import json, sys, datetime
ledger_path, init_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    led = json.load(open(ledger_path))
except Exception as e:
    print(json.dumps({"error": f"ledger read failed: {e}", "digest_valid": False}))
    sys.exit(0)
items = led.get("items", led if isinstance(led, list) else [])
counts = {}
for it in items:
    counts[it.get("status", "?")] = counts.get(it.get("status", "?"), 0) + 1
now = datetime.datetime.now(datetime.timezone.utc)
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
    "digest_valid": True
}
json.dump(bundle, open(out_path, "w"), indent=1)
print(f"digest bundle: {len(items)} items, counts={counts}, stuck={len(stuck)}")
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

export DAGGER_ENV="LEDGER_PATH=$LEDGER,INITIATIVES_PATH=$INITIATIVES,DAGGER_PROPOSALS_SCRATCH=$DAGGER_PROPOSALS_SCRATCH,VERIFICATIONS_PATH=$VERIFICATIONS_PATH,PROPOSALS_HISTORY_PATH=$PROPOSALS_HISTORY_PATH,ESCALATIONS_PATH=$ESCALATIONS_PATH,ESCALATIONS_SCRATCH_PATH=$ESCALATIONS_SCRATCH_PATH,PUSH_SCRATCH_PATH=$PUSH_SCRATCH_PATH,DIGEST_BUNDLE_PATH=$DIGEST_BUNDLE_PATH,REAL_MODE=$REAL_MODE,PM_NAMESPACE=$PM_NAMESPACE,PM_TARGET=$PM_TARGET"
env GATEWAY_API_KEY="$KEY" ./dagger run examples/coding-hermes/pm.ts --server "http://127.0.0.1:$PORT" \
  --log-file "$STANDIN/pm-standin.jsonl" --log-level info >/tmp/pm-standin-run.log 2>&1
RUN_RC=$?

# Scratch → history accumulation (shell owns the append).
if [ -s "$DAGGER_PROPOSALS_SCRATCH" ]; then
  grep -v '^File not found' "$DAGGER_PROPOSALS_SCRATCH" >> "$DAGGER_PROPOSALS_PATH"
fi
if [ -s "$ESCALATIONS_SCRATCH_PATH" ]; then
  grep -v '^File not found' "$ESCALATIONS_SCRATCH_PATH" >> "$ESCALATIONS_PATH"
fi

# GAP-020: run log is truth (branch-skipped nodes exit 1 even on success).
if grep -q "Failed: 0" /tmp/pm-standin-run.log 2>/dev/null; then
  RUN_RC=0
fi

# ---- DEPLOY: push-scratch → REAL board rows + ledger entries ----
# HARD FILTER: when PM_TARGET is set, only proposals for that project pass.
PUSH_OUT_FILE=/tmp/pm-standin-push.out
rm -f "$PUSH_OUT_FILE"
if [ -s "$PUSH_SCRATCH_PATH" ] && [ "$REAL_MODE" = "1" ]; then
  PM_TARGET="$PM_TARGET" python3 - "$PUSH_SCRATCH_PATH" > "$PUSH_OUT_FILE" <<'PYEOF'
import json, sys, os, datetime
scratch_path = sys.argv[1]
target = os.environ.get("PM_TARGET", "")
home = os.path.expanduser("~")
led_path = os.path.join(home, ".hermes/stand-in/ledger.json")
pushed = []
dropped_scope = 0
try:
    lines = [l for l in open(scratch_path) if l.strip().startswith("{")]
    if not lines:
        print("PUSHED:[]")
        sys.exit(0)
    batch = json.loads(lines[-1])  # newest batch only
    for pr in batch.get("proposals", []):
        proj = pr.get("project", "")
        title = pr.get("title", "")
        prio = pr.get("priority", "P2")
        gap = pr.get("gap", "")
        if target and proj != target:
            dropped_scope += 1
            continue
        board = os.path.join(home, proj, ".coding-hermes/board/tasks.jsonl")
        if not (proj and title and os.path.isdir(os.path.join(home, proj)) and os.path.isfile(board)):
            continue
        # --- INSERT task row BEFORE the event (dispatch order doctrine) ---
        n = 0
        for l in open(board):
            if l.strip():
                try:
                    r = json.loads(l)
                    suffix = str(r.get("id", "")).split("-")[-1]
                    n = max(n, int(suffix) if suffix.isdigit() else 0)
                except Exception:
                    pass
        task_id = "PM-" + str(n + 1).zfill(3)
        row = {
            "id": task_id,
            "title": title[:150], "priority": prio,
            "status": "pending", "origin": "stand-in-pm-dagger",
            "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "reasoning": f"gap: {gap[:200]}"
        }
        with open(board, "a") as f:
            f.write(json.dumps(row) + "\n")
        ev_path = os.path.join(home, proj, ".coding-hermes/board/events.jsonl")
        ev = {"id": task_id, "kind": "task_added", "source": "stand-in-pm-dagger",
              "detail": title[:120], "ts": datetime.datetime.now(datetime.timezone.utc).isoformat()}
        with open(ev_path, "a") as f:
            f.write(json.dumps(ev) + "\n")
        # ledger entry (status=added)
        try:
            led = json.load(open(led_path))
            if not isinstance(led, dict):
                led = {"items": led}
        except Exception:
            led = {"items": []}
        items = led.get("items", [])
        if not isinstance(items, list):
            items = []
        items.append({"id": proj + ":" + task_id, "project": proj, "title": title[:120],
                      "status": "added", "priority": prio,
                      "added_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                      "origin": "dagger-stand-in"})
        led["items"] = items
        json.dump(led, open(led_path, "w"), indent=1)
        pushed.append(proj + ":" + task_id + " " + title[:60])
    print("PUSHED:" + json.dumps(pushed))
    if dropped_scope:
        print("DROPPED_SCOPE:" + str(dropped_scope))
except Exception as e:
    print("PUSH-ERROR:" + str(e))
PYEOF
  # archive the consumed batch so it never re-pushes
  cat "$PUSH_SCRATCH_PATH" >> "$PUSH_SCRATCH_PATH.consumed" 2>/dev/null
  : > "$PUSH_SCRATCH_PATH"
fi
PUSHED=0
PUSH_DETAIL=""
if [ -s "$PUSH_OUT_FILE" ]; then
  PUSH_DETAIL=$(grep -oE 'PUSHED:\[.*\]' "$PUSH_OUT_FILE" | head -1 | cut -d: -f2-)
  [ -z "$PUSH_DETAIL" ] && PUSH_DETAIL=$(grep 'PUSH-ERROR' "$PUSH_OUT_FILE" | head -1)
  if [ -n "$PUSH_DETAIL" ]; then
    NQ=$(echo "$PUSH_DETAIL" | grep -o '"' | wc -l)
    PUSHED=$((NQ / 4))
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
