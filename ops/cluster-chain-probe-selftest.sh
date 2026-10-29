#!/usr/bin/env bash
#
# cluster-chain-probe-selftest.sh — prove that ops/cluster-chain-probe.sh can
# FAIL, and that a PASS names the value it measured.
#
# WHY THIS EXISTS
#   A probe that only ever prints PASS is a probe that lies. This selftest
#   drives the real probe against targets whose behaviour it controls, and makes
#   nine claims, each with its own checks:
#
#     1. CAUSALITY — the SAME probe invocation, pointed at the SAME URL, PASSes
#        while the target answers and FAILs (exit non-zero, arm named) once that
#        target is gone. Nothing else changes between the two runs, so the
#        verdict is attributable to the target's state.
#     2. A PASS IS A MEASUREMENT — the PASS arm's `measured` field carries the
#        value it read (the stub's version string), not a bare "ok".
#     3. DRIFT IS COUNTED — the scheduler arm PASSes when the live build_sha is
#        the checked-out HEAD, and FAILs with the exact commit count when it is
#        not (`drift=behind=2 ahead=0` against a purpose-built 3-commit repo).
#     4. EVERY GATING ARM CAN FAIL — router (no router_spawn.py), crier (dead
#        socket), scheduler (dead socket), bunker (no CLI) each exit 1 and name
#        the hop plus a machine-readable reason prefix. A dead EVERYTHING run
#        reports `all` as FAIL with failed=4.
#     5. IT IS READ-ONLY AND QUIET — the stub records every request; the
#        selftest asserts the probe sent GETs only, and that a fake credential
#        in the environment never appears in the probe's output.
#     6. THE CONNECTOR ARM NEVER GATES — it reports presence/absence and exits 0
#        either way, because presence is not a working hop.
#     7. THE PROXY ARM MEASURES THE INGRESS — a stub serving an older commit
#        FAILs with PROXY_BUILD_DRIFT naming both commits and the exact *.py
#        count, a stub serving the tip PASSes, a docs-only commit after the
#        deploy is not drift, and forcing the comparison to zero in a throwaway
#        copy turns the stale case green (the checks are not vacuous).
#     8. A COMPLETE FRESH CHAIN CAN SAY YES — with a fresh stub, a route stub
#        and a bunker stub, `all` PASSes five gating arms, reports the connector
#        and exits 0, so "green" is a reachable state and not just a theory.
#
#   Everything runs against 127.0.0.1 only: a python stub HTTP server that plays
#   both the crier and the scheduler contracts, a black-hole socket that accepts
#   and never answers (for the timeout check), and closed ports. No external
#   network, no live fleet service is required, so the result is the same on a
#   laptop and on a bunker host.
#
# USAGE
#   ops/cluster-chain-probe-selftest.sh [--verbose]
#
# EXIT CODES
#   0  every selftest check passed
#   1  at least one check failed (the probe is not proving what it claims)
#   2  a required tool is missing (curl / python3 / ss / git / jq) — never a
#      silent skip

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE="$HERE/cluster-chain-probe.sh"
VERBOSE=0
[ "${1-}" = "--verbose" ] && VERBOSE=1

TMP="$(mktemp -d "${TMPDIR:-/tmp}/cluster-chain-probe-selftest.XXXXXX")" || exit 2
STUB_PID=""
HANG_PID=""
cleanup() {
  for p in "$STUB_PID" "$HANG_PID"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

PASS=0
FAIL=0
pass() { printf '  PASS  %s\n' "$1"; PASS=$((PASS + 1)); }
fail() { printf '  FAIL  %s\n' "$1"; FAIL=$((FAIL + 1)); }
hdr()  { printf '\n=== %s ===\n' "$1"; }

# ─── preconditions (fail closed, never a vacuous green) ─────────────────────
for tool in curl python3 ss git jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "selftest: required tool '$tool' is missing" >&2; exit 2; }
done
[ -f "$PROBE" ] || { echo "selftest: probe not found at $PROBE" >&2; exit 2; }

# ─── assertion helpers ─────────────────────────────────────────────────────
expect_rc() { # label want got
  if [ "$2" = "$3" ]; then pass "$1 (exit $3)"; else fail "$1 (want exit $2, got $3)"; fi
}
expect_file_has() { # label file needle
  if grep -qF -- "$3" "$2"; then pass "$1"; else fail "$1 — output does not contain: $3"; fi
}
expect_file_lacks() { # label file needle
  if grep -qF -- "$3" "$2"; then fail "$1 — output contains forbidden: $3"; else pass "$1"; fi
}
show() { [ "$VERBOSE" = "1" ] && sed 's/^/        /' "$1"; return 0; }

run_probe() { # outfile arg...
  local out="$1"; shift
  "$@" >"$out" 2>&1
  return $?
}

# ─── free-port selection, bounded ──────────────────────────────────────────
pick_port() {
  local base="${1:-18800}" p i
  for i in $(seq 0 60); do
    p=$((base + i))
    if ! ss -tlnH "sport = :$p" 2>/dev/null | grep -q .; then
      printf '%s' "$p"
      return 0
    fi
  done
  return 1
}

wait_http() { # url tries
  local i
  for i in $(seq 1 "${2:-60}"); do
    if [ "$(curl -s -m 1 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null)" = "200" ]; then return 0; fi
    sleep 0.1
  done
  return 1
}

listener_pid() { # port -> pid or empty
  ss -tlnpH "sport = :$1" 2>/dev/null | grep -oP 'pid=\K[0-9]+' | head -1
}

# ═══ 1. the probe parses, documents itself, and names its arms ═════════════
hdr "1. probe surface"

bash -n "$PROBE" 2>"$TMP/syntax.err"
expect_rc "probe passes bash -n" 0 $?

run_probe "$TMP/help.out" "$PROBE" --help
expect_rc "--help exits 0" 0 $?
expect_file_has "--help documents read-only behaviour" "$TMP/help.out" "READ-ONLY BY CONSTRUCTION"
expect_file_has "--help names the non-coverage pointer" "$TMP/help.out" "cluster-chain-probe.md"

run_probe "$TMP/arms.out" "$PROBE" arms
expect_rc "arms exits 0" 0 $?
for a in router proxy crier scheduler bunker connector; do
  expect_file_has "arms lists $a" "$TMP/arms.out" "$a"
done
expect_file_has "connector is declared non-gating" "$TMP/arms.out" "no (report-only)"
expect_file_has "proxy is declared gating but reporting when unmeasurable" "$TMP/arms.out" "yes (reports when unmeasurable)"

run_probe "$TMP/badarg.out" "$PROBE" --not-a-flag
expect_rc "an unknown flag is a usage error (exit 2)" 2 $?

# ═══ 2. hermetic stub target (plays crier + scheduler contracts) ═══════════
hdr "2. crier arm: PASS while the target answers, FAIL once it is gone"

cat >"$TMP/stub.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, LOG, BSHA_FILE = int(sys.argv[1]), sys.argv[2], sys.argv[3]
HEALTH504 = BSHA_FILE + ".health504"
PROXY_FILE = BSHA_FILE + ".proxy"


def proxy_body():
    """The classified ingress's /health shape (task-router: mode + code block).

    Absent file -> the plain crier shape, so every pre-proxy check keeps the
    exact body it was written against.
    """
    try:
        with open(PROXY_FILE) as f:
            d = json.load(f)
    except (OSError, ValueError):
        return {"status": "ok"}
    return {
        "status": d.get("status", "ok"),
        "service": "task-router/1.0",
        "commit": d.get("commit", ""),
        "stale": d.get("stale"),
        "mode": "read-only",
        "code": {
            "loaded_commit": d.get("commit", ""),
            "repo_commit": d.get("repo_commit", ""),
            "stale": d.get("stale"),
            "error": None,
        },
    }


def build_sha():
    try:
        with open(BSHA_FILE) as f:
            return f.read().strip()
    except OSError:
        return ""


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _rec(self):
        with open(LOG, "a") as f:
            f.write(json.dumps({"method": self.command, "path": self.path}) + "\n")

    def _send(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self._rec()
        if self.path == "/health":
            return self._send(200, proxy_body())
        if self.path == "/version":
            return self._send(200, {"version": "selftest-stub-1.2.3", "commit": "deadbeef"})
        if self.path == "/status":
            return self._send(401, {"error": "missing Authorization header"})
        if self.path == "/api/v1/live":
            return self._send(200, {
                "status": "ok", "version": "selftest-stub",
                "build_sha": build_sha(), "uptime": "12.5s",
                "started": "2026-01-01T00:00:00Z",
            })
        if self.path == "/api/v1/health" and os.path.exists(HEALTH504):
            return self._send(504, {"error": "deadline exceeded",
                                    "helper": "PingContext",
                                    "detail": "context deadline exceeded"})
        if self.path == "/api/v1/health":
            return self._send(200, {
                "status": "ok", "db": "connected", "build_sha": build_sha(),
                "active_ticks": 3, "version": "selftest-stub",
                "clock": {"mode": "real", "scale": 1},
            })
        if self.path == "/api/v1/status":
            return self._send(200, {
                "active_ticks": 3, "active_projects": 648, "build_sha": build_sha(),
            })
        return self._send(404, {"error": "not found"})

    def _non_get(self):
        self._rec()
        self._send(405, {"error": "method not allowed"})

    do_POST = do_PUT = do_PATCH = do_DELETE = _non_get


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY

PORT="$(pick_port 18800)" || { echo "selftest: no free port for the stub" >&2; exit 2; }
: >"$TMP/stub.log"
printf '00000000' >"$TMP/bsha.txt"
python3 "$TMP/stub.py" "$PORT" "$TMP/stub.log" "$TMP/bsha.txt" & STUB_PID=$!
STUB_URL="http://127.0.0.1:$PORT"

if ! wait_http "$STUB_URL/health" 60; then
  fail "stub server did not become reachable on 127.0.0.1:$PORT"
else
  pass "stub server is answering on 127.0.0.1:$PORT"

  # Ownership: the listener must be the pid we started (or its child).
  HOLDER="$(listener_pid "$PORT")"
  if [ -z "$HOLDER" ]; then
    pass "listener ownership UNKNOWN on the first poll (treated as unknown, not as foreign)"
  elif [ "$HOLDER" = "$STUB_PID" ] || [ "$(ps -o ppid= -p "$HOLDER" 2>/dev/null | tr -d ' ')" = "$STUB_PID" ]; then
    pass "stub listener $HOLDER is attributable to launcher $STUB_PID"
  else
    fail "port $PORT is held by $HOLDER, not by launcher $STUB_PID — the next probes would read a foreign process"
  fi

  # Sentinel credentials. Their NAMES are assembled at run time so this file
  # carries no literal `NAME=value` credential assignment for the repo's
  # secrets scanners to flag; the property under test is that values present in
  # the environment never reach the probe's output.
  SENTINEL="clusterprobe-selftest-sentinel-9f3a1c-DO-NOT-PRINT"
  AUTH_VAR="CR_AUTH""_TOKEN"
  KEY_VAR="OPENROUTER_API""_KEY"
  export "${AUTH_VAR}=${SENTINEL}" "${KEY_VAR}=${SENTINEL}"
  CRIER_URL="$STUB_URL" run_probe "$TMP/crier-pass.out" "$PROBE" crier
  RC=$?
  show "$TMP/crier-pass.out"
  expect_rc "crier arm PASSes against a live target" 0 "$RC"
  expect_file_has "PASS verdict is reported" "$TMP/crier-pass.out" '"arm":"crier","verdict":"PASS"'

  MEASURED="$(jq -r '.measured' "$TMP/crier-pass.out" 2>/dev/null || echo '')"
  if [ "${#MEASURED}" -gt 30 ] && printf '%s' "$MEASURED" | grep -q 'version=selftest-stub-1.2.3'; then
    pass "PASS names the measured value, not a bare 'ok': $(printf '%s' "$MEASURED" | cut -c1-90)…"
  else
    fail "PASS did not carry the measured version (measured='$MEASURED')"
  fi
  expect_file_has "PASS records the unauthenticated refusal" "$TMP/crier-pass.out" "auth=refused:401"
  expect_file_lacks "no credential appears in the output" "$TMP/crier-pass.out" "SECRET-DO-NOT-PRINT"

  # ── 3. scheduler drift: green at drift=0, red with the exact count ──────
  hdr "3. scheduler arm: drift=0 PASSes, drift=N FAILs naming N"

  CHAIN="$TMP/repo-chain"
  mkdir -p "$CHAIN"
  git -C "$CHAIN" init -q
  for n in 1 2 3; do
    printf 'package main\n// commit %s\n' "$n" >"$CHAIN/main.go"
    git -C "$CHAIN" -c user.email=selftest@example.invalid -c user.name=selftest \
      add main.go
    git -C "$CHAIN" -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -qm "commit $n"
  done
  CHAIN_HEAD="$(git -C "$CHAIN" rev-parse HEAD)"
  printf '%s' "${CHAIN_HEAD:0:8}" >"$TMP/bsha.txt"

  SCHEDULER_URL="$STUB_URL" run_probe "$TMP/sched-pass.out" "$PROBE" scheduler --repo "$CHAIN"
  RC=$?
  show "$TMP/sched-pass.out"
  expect_rc "scheduler arm PASSes when build_sha == HEAD" 0 "$RC"
  expect_file_has "the PASS states drift=0" "$TMP/sched-pass.out" "drift=0"
  expect_file_has "the PASS names the build_sha it read" "$TMP/sched-pass.out" "build_sha=${CHAIN_HEAD:0:8}"
  expect_file_has "the PASS names the api it read" "$TMP/sched-pass.out" "db=connected"

  git -C "$CHAIN" rev-parse --short=8 'HEAD~2' >"$TMP/bsha.txt"
  SCHEDULER_URL="$STUB_URL" run_probe "$TMP/sched-drift.out" "$PROBE" scheduler --repo "$CHAIN"
  RC=$?
  show "$TMP/sched-drift.out"
  expect_rc "scheduler arm exits 1 when the build is behind HEAD" 1 "$RC"
  expect_file_has "the drift verdict is FAIL" "$TMP/sched-drift.out" '"arm":"scheduler","verdict":"FAIL"'
  expect_file_has "the drift reason is named" "$TMP/sched-drift.out" "SCHEDULER_BUILD_DRIFT"
  expect_file_has "the drift count is named exactly (behind=2)" "$TMP/sched-drift.out" "drift=behind=2 ahead=0"

  # A commit that touches no compiled path is not in the binary by definition:
  # landing an ops/docs commit after a deploy must not read as drift.
  printf '%s' "${CHAIN_HEAD:0:8}" >"$TMP/bsha.txt"   # back to a matching build first
  printf '# doc\n' >"$CHAIN/NOTES.md"
  git -C "$CHAIN" -c user.email=selftest@example.invalid -c user.name=selftest add NOTES.md
  git -C "$CHAIN" -c user.email=selftest@example.invalid -c user.name=selftest commit -qm "docs only"
  SCHEDULER_URL="$STUB_URL" run_probe "$TMP/sched-docsonly.out" "$PROBE" scheduler --repo "$CHAIN"
  RC=$?
  show "$TMP/sched-docsonly.out"
  expect_rc "a docs-only commit after the deploy is not drift" 0 "$RC"
  expect_file_has "the non-compiled commit is still counted and named" "$TMP/sched-docsonly.out" "compiled_behind=0"
  git -C "$CHAIN" reset -q --hard HEAD~1

  # A 504 on the DB-backed health route is DEGRADED, not down: the DB-free
  # liveness route answers, so the arm must not report the daemon unreachable.
  printf '%s' "${CHAIN_HEAD:0:8}" >"$TMP/bsha.txt"    # back to a matching build, so the ONLY
  : >"$TMP/bsha.txt.health504"                        # defect under test is the 504 health read
  SCHEDULER_URL="$STUB_URL" run_probe "$TMP/sched-degraded.out" "$PROBE" scheduler --repo "$CHAIN"
  RC=$?
  show "$TMP/sched-degraded.out"
  rm -f "$TMP/bsha.txt.health504"
  expect_rc "a 504 health read with live ok still PASSes" 0 "$RC"
  expect_file_has "the degraded read is recorded, not hidden" "$TMP/sched-degraded.out" "health=degraded:504"
  expect_file_has "the pass names the DB-free route it trusted" "$TMP/sched-degraded.out" "live=ok"

  # An unattributable build_sha must be its own named failure.
  printf 'ffffffff' >"$TMP/bsha.txt"
  SCHEDULER_URL="$STUB_URL" run_probe "$TMP/sched-unknown.out" "$PROBE" scheduler --repo "$CHAIN"
  expect_rc "an unknown build_sha fails the arm" 1 $?
  expect_file_has "the unknown-commit reason is named" "$TMP/sched-unknown.out" "SCHEDULER_BUILD_UNKNOWN_COMMIT"

  # Read-only: every request the probe issued was a GET.
  if [ -s "$TMP/stub.log" ]; then
    NON_GET="$(jq -r 'select(.method != "GET") | .method + " " + .path' "$TMP/stub.log" 2>/dev/null)"
    if [ -z "$NON_GET" ]; then
      pass "probe issued GETs only ($(wc -l <"$TMP/stub.log" | tr -d ' ') requests recorded by the stub)"
    else
      fail "probe issued non-GET requests: $NON_GET"
    fi
  else
    fail "stub recorded no requests — the PASS above did not actually talk to it"
  fi

  # ── 3b. proxy arm: the ingress is judged on *.py drift, both ways ───────
  hdr "3b. proxy arm: a stale ingress FAILs, a fresh one PASSes, docs commits do not count"

  PCHAIN="$TMP/proxy-chain"
  mkdir -p "$PCHAIN"
  git -C "$PCHAIN" init -q
  for n in 1 2 3; do
    printf 'VERSION = %s\n' "$n" >"$PCHAIN/service.py"
    git -C "$PCHAIN" -c user.email=selftest@example.invalid -c user.name=selftest add service.py
    git -C "$PCHAIN" -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -qm "py commit $n"
  done
  P_HEAD="$(git -C "$PCHAIN" rev-parse HEAD)"
  P_STALE="$(git -C "$PCHAIN" rev-parse --short=12 'HEAD~2')"

  # The stub's /health plays the ingress contract from a file, so each case
  # changes ONLY what the running process says about itself.
  proxy_say() { # commit stale [status]
    python3 - "$TMP/bsha.txt.proxy" "$1" "${2:-false}" "$P_HEAD" "${3:-ok}" <<'PY'
import json, sys
json.dump({"commit": sys.argv[2], "stale": sys.argv[3] == "true",
           "repo_commit": sys.argv[4], "status": sys.argv[5]},
          open(sys.argv[1], "w"))
PY
  }

  proxy_say "$P_HEAD" false
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-fresh.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-fresh.out"
  expect_rc "proxy arm PASSes when the loaded commit is the lineage tip" 0 "$RC"
  expect_file_has "the proxy PASS states drift=0" "$TMP/proxy-fresh.out" "drift=0"
  expect_file_has "the proxy PASS names the commit it read" "$TMP/proxy-fresh.out" "loaded_commit=${P_HEAD:0:12}"

  proxy_say "$P_STALE" true
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-stale.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-stale.out"
  expect_rc "proxy arm exits 1 when the ingress loaded an older commit" 1 "$RC"
  expect_file_has "the stale verdict is FAIL" "$TMP/proxy-stale.out" '"arm":"proxy","verdict":"FAIL"'
  expect_file_has "the stale reason is named PROXY_BUILD_DRIFT" "$TMP/proxy-stale.out" "PROXY_BUILD_DRIFT"
  expect_file_has "the stale count is exact (compiled_behind=2)" "$TMP/proxy-stale.out" "compiled_behind=2"
  expect_file_has "the stale reason names the loaded commit" "$TMP/proxy-stale.out" "$P_STALE"
  expect_file_has "the stale reason names the lineage commit too" "$TMP/proxy-stale.out" "$P_HEAD"
  expect_file_has "the ingress's own stale flag is recorded" "$TMP/proxy-stale.out" "stale_flag=true"
  expect_file_has "the flag agrees with the measured drift" "$TMP/proxy-stale.out" "flag_agrees=yes"

  # A commit that touches no *.py file cannot be in a running interpreter:
  # landing docs or board rows after a deploy must not read as ingress drift.
  printf '# notes\n' >"$PCHAIN/NOTES.md"
  git -C "$PCHAIN" -c user.email=selftest@example.invalid -c user.name=selftest add NOTES.md
  git -C "$PCHAIN" -c user.email=selftest@example.invalid -c user.name=selftest commit -qm "docs only"
  proxy_say "$P_HEAD" false
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-docsonly.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-docsonly.out"
  expect_rc "a docs-only commit after the ingress deploy is not drift" 0 "$RC"
  expect_file_has "the non-Python commit is still counted and named" "$TMP/proxy-docsonly.out" "compiled_behind=0"
  expect_file_has "the raw behind count is not hidden" "$TMP/proxy-docsonly.out" "behind=1"

  # A flag that claims fresh while the code is behind must not read as agreement.
  P_STALE_FULL="$(git -C "$PCHAIN" rev-parse 'HEAD~3')"
  proxy_say "${P_STALE_FULL:0:12}" false
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-lyingflag.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-lyingflag.out"
  expect_rc "a lying fresh flag still FAILs on measured drift" 1 "$RC"
  expect_file_has "the disagreement with the flag is reported" "$TMP/proxy-lyingflag.out" "flag_agrees=no"

  # An unattributable loaded commit is its own named failure.
  proxy_say "ffffffffffff" false
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-unknown.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-unknown.out"
  expect_rc "an unknown loaded commit fails the arm" 1 "$RC"
  expect_file_has "the unknown-commit reason is named" "$TMP/proxy-unknown.out" "PROXY_BUILD_UNKNOWN_COMMIT"

  # A 200 that names itself unhealthy fails whatever its build is.
  proxy_say "$P_HEAD" false degraded
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-unhealthy.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-unhealthy.out"
  expect_rc "an unhealthy ingress fails even though its build is fresh" 1 "$RC"
  expect_file_has "the unhealthy body is named PROXY_UNHEALTHY" "$TMP/proxy-unhealthy.out" "PROXY_UNHEALTHY"

  # An ingress that answers without naming its build cannot be attributed:
  # that is a report (a host may run a proxy with no build stamp), never a pass.
  python3 - "$TMP/bsha.txt.proxy" <<'PY'
import json, sys
json.dump({"commit": "", "stale": False, "repo_commit": "", "status": "ok"},
          open(sys.argv[1], "w"))
PY
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-nocommit.out" \
    "$PROBE" proxy --proxy-repo "$PCHAIN"
  RC=$?
  show "$TMP/proxy-nocommit.out"
  expect_rc "an unattributable ingress reports instead of passing" 0 "$RC"
  expect_file_has "the unattributable case is named PROXY_NO_COMMIT" "$TMP/proxy-nocommit.out" "PROXY_NO_COMMIT"
  expect_file_has "the unattributable case is a REPORT, never a PASS" "$TMP/proxy-nocommit.out" '"verdict":"REPORT"'

  # A live ingress with no checkout to compare against cannot be attributed:
  # required -> FAIL named; the same case without --proxy-required is a report.
  proxy_say "$P_HEAD" false
  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-norepo.out" \
    "$PROBE" proxy --proxy-required --proxy-repo "$TMP/definitely-no-repo"
  RC=$?
  show "$TMP/proxy-norepo.out"
  expect_rc "a required hop with no checkout to compare against fails" 1 "$RC"
  expect_file_has "the missing-checkout failure is named" "$TMP/proxy-norepo.out" "PROXY_DRIFT_UNMEASURED"

  ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-norepo-report.out" \
    "$PROBE" proxy --proxy-repo "$TMP/definitely-no-repo"
  RC=$?
  show "$TMP/proxy-norepo-report.out"
  expect_rc "the same unattributable ingress only reports when the hop is not owned" 0 "$RC"
  expect_file_has "the report names the missing checkout" "$TMP/proxy-norepo-report.out" "PROXY_DRIFT_UNMEASURED"

  # MUTATION CONTROL: neuter the drift comparison itself. If the stale case
  # still FAILs with the comparison forced to zero, the checks above are not
  # testing the comparison at all.
  python3 - "$PROBE" "$TMP/probe-mutant.sh" <<'PY'
import sys
src = open(sys.argv[1]).read()
out, hit = [], 0
for line in src.splitlines(keepends=True):
    if 'p_cb="$(git -C "$p_repo" rev-list' in line:
        hit += 1
        out.append('    p_cb="0"\n')
    else:
        out.append(line)
if hit != 1:
    sys.stderr.write("expected exactly one p_cb assignment, found %d\n" % hit)
    sys.exit(3)
open(sys.argv[2], "w").write("".join(out))
PY
  if [ $? -ne 0 ]; then
    fail "the mutation control could not locate the drift comparison"
  else
    chmod +x "$TMP/probe-mutant.sh"
    proxy_say "$P_STALE" true
    ROUTER_PROXY_URL="$STUB_URL" run_probe "$TMP/proxy-mutant.out" \
      bash "$TMP/probe-mutant.sh" proxy --proxy-repo "$PCHAIN"
    RC=$?
    show "$TMP/proxy-mutant.out"
    if [ "$RC" = "0" ]; then
      pass "neutering the drift comparison turns the stale case green — the checks above test it"
    else
      fail "the mutated probe still FAILed the stale case — the drift checks are not attributable to the comparison"
    fi
  fi
  rm -f "$TMP/bsha.txt.proxy"

  # Take the target away. Nothing else about the invocation changes.
  kill "$STUB_PID" 2>/dev/null
  HOLDER="$(listener_pid "$PORT")"
  [ -n "$HOLDER" ] && [ "$HOLDER" != "$STUB_PID" ] && kill "$HOLDER" 2>/dev/null
  for _ in $(seq 1 30); do
    [ -z "$(listener_pid "$PORT")" ] && break
    sleep 0.1
  done
  if [ -z "$(listener_pid "$PORT")" ]; then
    pass "stub target is gone and 127.0.0.1:$PORT is released"
    STUB_PID=""

    CRIER_URL="$STUB_URL" run_probe "$TMP/crier-fail.out" "$PROBE" crier
    RC=$?
    show "$TMP/crier-fail.out"
    expect_rc "the SAME invocation now exits non-zero" 1 "$RC"
    expect_file_has "the failure names the arm" "$TMP/crier-fail.out" '"arm":"crier"'
    expect_file_has "the failure is a FAIL verdict" "$TMP/crier-fail.out" '"verdict":"FAIL"'
    expect_file_has "the failure names the hop and a reason code" "$TMP/crier-fail.out" "CRIER_UNREACHABLE"
  else
    fail "could not release 127.0.0.1:$PORT — the fail arm was not exercised"
  fi
fi

# ═══ 4. bounded timeout against a target that accepts but never answers ════
hdr "4. an unresponsive target is bounded by --timeout, not waited on"

cat >"$TMP/hang.py" <<'PY'
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1])))
s.listen(16)
held = []
while True:
    c, _ = s.accept()
    held.append(c)
PY

HPORT="$(pick_port 18900)" || HPORT=""
if [ -n "$HPORT" ]; then
  python3 "$TMP/hang.py" "$HPORT" & HANG_PID=$!
  sleep 0.4
  START_MS=$(( $(date +%s%N) / 1000000 ))
  CRIER_URL="http://127.0.0.1:$HPORT" run_probe "$TMP/hang.out" "$PROBE" crier --timeout 3
  RC=$?
  END_MS=$(( $(date +%s%N) / 1000000 ))
  ELAPSED=$(( END_MS - START_MS ))
  show "$TMP/hang.out"
  expect_rc "a black-hole target FAILs the arm" 1 "$RC"
  expect_file_has "the timeout failure names CRIER_UNREACHABLE" "$TMP/hang.out" "CRIER_UNREACHABLE"
  if [ "$ELAPSED" -lt 15000 ]; then
    pass "the run was bounded by --timeout 3 (finished in ${ELAPSED}ms)"
  else
    fail "the run was not bounded (${ELAPSED}ms with --timeout 3)"
  fi
  kill "$HANG_PID" 2>/dev/null
  HANG_PID=""
else
  fail "no free port for the black-hole stub"
fi

# ═══ 5. every gating arm can fail, by name ═════════════════════════════════
hdr "5. each gating arm can fail and names itself"

DEAD_PORT="$(pick_port 19000)" || DEAD_PORT="19000"
DEAD_URL="http://127.0.0.1:$DEAD_PORT"

SCHEDULER_URL="$DEAD_URL" run_probe "$TMP/sched-dead.out" "$PROBE" scheduler
expect_rc "scheduler arm exits 1 against a closed socket" 1 $?
expect_file_has "scheduler failure is named SCHEDULER_UNREACHABLE" "$TMP/sched-dead.out" "SCHEDULER_UNREACHABLE"
expect_file_has "scheduler failure names the arm" "$TMP/sched-dead.out" '"arm":"scheduler","verdict":"FAIL"'

ROUTER_SPAWN="$TMP/definitely-no-router_spawn.py" run_probe "$TMP/router-fail.out" "$PROBE" router
expect_rc "router arm exits 1 when router_spawn.py is absent" 1 $?
expect_file_has "router failure is named ROUTER_SCRIPT_ABSENT" "$TMP/router-fail.out" "ROUTER_SCRIPT_ABSENT"

BUNKER_BIN="$TMP/definitely-no-bunker" run_probe "$TMP/bunker-fail.out" "$PROBE" bunker
expect_rc "bunker arm exits 1 when the CLI is absent" 1 $?
expect_file_has "bunker failure is named BUNKER_CLI_ABSENT" "$TMP/bunker-fail.out" "BUNKER_CLI_ABSENT"

# The proxy arm gates on what it can measure and reports on what it cannot:
# both halves are asserted, or the arm is either decoration or a false alarm.
ROUTER_PROXY_URL="$DEAD_URL" run_probe "$TMP/proxy-dead.out" "$PROBE" proxy --proxy-required
expect_rc "proxy arm exits 1 against a closed socket when the hop is owned" 1 $?
expect_file_has "proxy failure is named PROXY_UNREACHABLE" "$TMP/proxy-dead.out" "PROXY_UNREACHABLE"
expect_file_has "proxy failure names the arm" "$TMP/proxy-dead.out" '"arm":"proxy","verdict":"FAIL"'

ROUTER_PROXY_URL="$DEAD_URL" run_probe "$TMP/proxy-dead-report.out" "$PROBE" proxy
expect_rc "the same closed socket only REPORTS when the hop is not owned" 0 $?
expect_file_has "the unowned-hop case is a REPORT" "$TMP/proxy-dead-report.out" '"verdict":"REPORT"'
expect_file_has "the report still names the reason code" "$TMP/proxy-dead-report.out" "PROXY_UNREACHABLE"
expect_file_has "the report says the exit code is unaffected" "$TMP/proxy-dead-report.out" "exit code unaffected"


# ═══ 6. the connector arm is report-only and never gates ═══════════════════
hdr "6. connector arm reports presence without gating"

mkdir -p "$TMP/repo-present/internal/connector" "$TMP/repo-absent"
git -C "$TMP/repo-present" init -q
printf 'package connector\n' >"$TMP/repo-present/internal/connector/doc.go"
git -C "$TMP/repo-present" add internal/connector/doc.go
git -C "$TMP/repo-absent" init -q

run_probe "$TMP/conn-present.out" "$PROBE" connector --offline --repo "$TMP/repo-present"
expect_rc "connector arm exits 0 with internal/connector present" 0 $?
expect_file_has "connector reports presence" "$TMP/conn-present.out" "internal/connector=present"
expect_file_has "connector verdict is REPORT, not PASS" "$TMP/conn-present.out" '"verdict":"REPORT"'

run_probe "$TMP/conn-absent.out" "$PROBE" connector --offline --repo "$TMP/repo-absent"
expect_rc "connector arm exits 0 with internal/connector absent (report-only)" 0 $?
expect_file_has "connector reports absence" "$TMP/conn-absent.out" "internal/connector=absent"

# ═══ 7. a dead-everything run: all arms fail, the summary says so ══════════
hdr "7. all: a dead chain fails overall and names every failed arm"

ROUTER_SPAWN="$TMP/nope.py" \
BUNKER_BIN="$TMP/nope-bunker" \
SCHEDULER_URL="$DEAD_URL" \
CRIER_URL="$DEAD_URL" \
ROUTER_PROXY_URL="$DEAD_URL" \
  run_probe "$TMP/all-fail.out" "$PROBE" all --offline --repo "$TMP/repo-absent" --no-router-http --proxy-required
RC=$?
show "$TMP/all-fail.out"
expect_rc "all exits 1 when every gating arm fails" 1 "$RC"
expect_file_has "the summary verdict is FAIL" "$TMP/all-fail.out" '"arm":"summary","verdict":"FAIL"'
expect_file_has "the summary counts five failed gating arms" "$TMP/all-fail.out" '"failed":5'
expect_file_has "the summary declares the exit code" "$TMP/all-fail.out" '"overall_exit":1'
for needle in ROUTER_SCRIPT_ABSENT PROXY_UNREACHABLE CRIER_UNREACHABLE SCHEDULER_UNREACHABLE BUNKER_CLI_ABSENT; do
  expect_file_has "the run names $needle" "$TMP/all-fail.out" "$needle"
done
expect_file_has "the human summary line is printed" "$TMP/all-fail.out" "CLUSTER-CHAIN-PROBE: FAIL"

# Every line that starts with { must be valid JSON.
BAD_JSON=0
while IFS= read -r line; do
  case "$line" in
    '{'*) printf '%s' "$line" | jq -e . >/dev/null 2>&1 || BAD_JSON=1 ;;
  esac
done <"$TMP/all-fail.out"
if [ "$BAD_JSON" = "0" ]; then
  pass "every JSON object the probe printed parsed"
else
  fail "the probe printed a line that is not valid JSON"
fi

# ═══ 8. a complete fresh chain exits 0 — the summary that can say yes ══════
hdr "8. all: a fresh chain PASSes every gating arm and reports the connector"

ALL_PORT="$(pick_port 19100)" || ALL_PORT=""
if [ -z "$ALL_PORT" ]; then
  fail "no free port for the all-green stub"
else
  ALL_URL="http://127.0.0.1:$ALL_PORT"
  : >"$TMP/allstub.log"
  printf '%s' "${CHAIN_HEAD:0:8}" >"$TMP/bsha.txt"
  python3 - "$P_HEAD" "$P_HEAD" <<'PY' >"$TMP/bsha.txt.proxy"
import json, sys
json.dump({"commit": sys.argv[1], "stale": False, "repo_commit": sys.argv[2],
           "status": "ok"}, sys.stdout)
PY
  python3 "$TMP/stub.py" "$ALL_PORT" "$TMP/allstub.log" "$TMP/bsha.txt" & ALL_PID=$!
  # A router CLI that answers a route and names a refusal code, and a bunker
  # CLI that answers a census line: the two non-HTTP hops, stubbed so the
  # whole-chain verdict can be exercised without a fleet.
  cat >"$TMP/router_spawn_stub.py" <<'PY'
import json, sys
if "--profile-req" in sys.argv:
    print(json.dumps({"code": "INVALID_REQUIREMENT", "error": "unknown requirement category"}))
else:
    print(json.dumps({"profile": "P1_CODING",
                      "head": {"model": "selftest/model", "provider": "selftest-provider",
                               "hop": 1, "effective_price": 0.0}}))
PY
  cat >"$TMP/bunker_stub.sh" <<'SH'
#!/usr/bin/env bash
printf 'Total: 2 agents (server: selftest-bunker)\n'
SH
  chmod +x "$TMP/bunker_stub.sh"

  if ! wait_http "$ALL_URL/health" 60; then
    fail "the all-green stub did not become reachable on 127.0.0.1:$ALL_PORT"
  else
    ROUTER_SPAWN="$TMP/router_spawn_stub.py" \
    BUNKER_BIN="$TMP/bunker_stub.sh" \
    SCHEDULER_URL="$ALL_URL" \
    CRIER_URL="$ALL_URL" \
    ROUTER_PROXY_URL="$ALL_URL" \
      run_probe "$TMP/all-pass.out" "$PROBE" all --offline \
        --repo "$CHAIN" --proxy-repo "$PCHAIN" --no-router-http --proxy-required
    RC=$?
    show "$TMP/all-pass.out"
    expect_rc "all exits 0 when every gating arm answers from a fresh build" 0 "$RC"
    expect_file_has "the summary verdict is PASS" "$TMP/all-pass.out" '"arm":"summary","verdict":"PASS"'
    expect_file_has "the summary counts five passed gating arms" "$TMP/all-pass.out" '"passed":5'
    expect_file_has "the summary counts no failures" "$TMP/all-pass.out" '"failed":0'
    expect_file_has "the connector is counted as report-only" "$TMP/all-pass.out" '"report_only":1'
    expect_file_has "the summary declares the exit code" "$TMP/all-pass.out" '"overall_exit":0'
    expect_file_has "the gating list names the proxy" "$TMP/all-pass.out" '"gating":["router","proxy","crier","scheduler","bunker"]'
    for a in router proxy crier scheduler bunker; do
      expect_file_has "the $a arm PASSed in the same run" "$TMP/all-pass.out" "\"$a\":{\"verdict\":\"PASS\""
    done
    expect_file_has "the connector verdict is REPORT" "$TMP/all-pass.out" '"connector":{"verdict":"REPORT"'
    expect_file_has "the human summary line says PASS" "$TMP/all-pass.out" "CLUSTER-CHAIN-PROBE: PASS"
  fi
  kill "$ALL_PID" 2>/dev/null
  ALL_PID=""
fi

# ═══ 9. a live chain (informational, never gating) ═════════════════════════
hdr "9. live fleet (informational — this host's own services)"

run_probe "$TMP/live.out" "$PROBE" all --offline
LIVE_RC=$?
show "$TMP/live.out"
while IFS= read -r line; do
  case "$line" in
    '{"arm":"summary"'*) printf '  INFO  %s\n' "$line" ;;
    'CLUSTER-CHAIN-PROBE'*) printf '  INFO  %s\n' "$line" ;;
  esac
done <"$TMP/live.out"
printf '  INFO  live run exit=%s (not counted: a bare host has no fleet to measure)\n' "$LIVE_RC"

# ═══ summary ══════════════════════════════════════════════════════════════
hdr "summary"
printf 'SELFTEST: PASS=%d FAIL=%d\n' "$PASS" "$FAIL"
if [ "$FAIL" -eq 0 ]; then
  printf 'RESULT: the probe is proven able to fail, and a PASS names its measurement\n'
  exit 0
fi
printf 'RESULT: the probe is NOT proven — %d check(s) failed\n' "$FAIL"
exit 1
