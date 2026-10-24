#!/usr/bin/env bash
# SCHED-GAP-16926 regression test for the post-deploy battery.
#
# This deliberately stubs only the nested pytest invocation: the checker is
# still the real checker, and the lint stage still runs its own battery. The
# outer repository pytest run exercises the fixture suite separately, while
# this shell test can safely invoke the battery without recursively invoking
# pytest/tests/test_check_invariants_battery.sh.
set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BATTERY="$REPO_ROOT/scripts/check-invariants.sh"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

STUB_BIN="$TMP_ROOT/bin"
mkdir -p "$STUB_BIN"
cat > "$STUB_BIN/python3" <<'PYTHON'
#!/usr/bin/env bash
if [ "${1:-}" = "-m" ] && [ "${2:-}" = "pytest" ]; then
    echo "stub nested pytest: PASS"
    exit 0
fi
exec /usr/bin/python3 "$@"
PYTHON
chmod +x "$STUB_BIN/python3"

run_battery() {
    local home="$1"
    local output="$2"
    set +e
    HOME="$home" PATH="$STUB_BIN:$PATH" bash "$BATTERY" >"$output" 2>&1
    TEST_RC=$?
    set -e
}

# ── absent live state: the CI/board-only arm remains usable ───────────────
ABSENT_HOME="$TMP_ROOT/absent-home"
mkdir -p "$ABSENT_HOME"
run_battery "$ABSENT_HOME" "$TMP_ROOT/absent.out"
if [ "$TEST_RC" -ne 0 ]; then
    echo "FAIL: absent-live-state battery exited $TEST_RC"
    cat "$TMP_ROOT/absent.out"
    exit 1
fi
if ! grep -q 'BATTERY: PASS (board-only; live fleet NOT checked; fleet checks not run: no local DB/TOML;' "$TMP_ROOT/absent.out"; then
    echo "FAIL: absent-live-state summary did not identify the board-only arm"
    cat "$TMP_ROOT/absent.out"
    exit 1
fi

# ── seeded live state: a live violation is not hidden by --board-only ─────
LIVE_HOME="$TMP_ROOT/live-home"
mkdir -p "$LIVE_HOME/.hermes/coding-hermes"
/usr/bin/python3 - "$LIVE_HOME" <<'PYTHON'
import sqlite3
import sys
from pathlib import Path

home = Path(sys.argv[1])
workdir = home / "fixture-project"
(workdir / ".coding-hermes" / "board").mkdir(parents=True)
db = home / ".hermes" / "coding-hermes" / "scheduler.db"
con = sqlite3.connect(db)
con.execute("""
    CREATE TABLE namespaces (
        id TEXT PRIMARY KEY,
        max_concurrent INTEGER,
        admission_mode TEXT
    )
""")
con.execute("""
    CREATE TABLE projects (
        name TEXT PRIMARY KEY,
        enabled INTEGER,
        cooldown_s INTEGER,
        cooldown_floor_s INTEGER,
        cooldown_ceiling_s INTEGER,
        adaptive_cooldown INTEGER,
        command TEXT,
        prompt TEXT,
        workdir TEXT,
        namespace_id TEXT
    )
""")
con.execute("""
    CREATE TABLE ticks (
        project_name TEXT,
        spawned_at TEXT,
        status TEXT
    )
""")
con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 16, 'tasks')")
for namespace in ('qa', 'pm', 'dogfood', 'duckbrain-sync', 'releases', 'doc-writer'):
    con.execute("INSERT INTO namespaces VALUES (?, 1, 'cooldown')", (namespace,))
# 21599 is deliberately below the 6-hour cooldown floor, so the live checker
# must emit a real `cooldown` violation and return 1.
con.execute(
    "INSERT INTO projects VALUES (?, 1, 21599, 21600, NULL, 0, ?, '', ?, 'fixture' )",
    ('fixture-project', 'fixture-command', str(workdir)),
)
con.commit()
con.close()
(home / ".hermes" / "fleet.toml").write_text("# seeded live-fleet fixture\n", encoding="utf-8")
PYTHON

run_battery "$LIVE_HOME" "$TMP_ROOT/live.out"
if [ "$TEST_RC" -ne 1 ]; then
    echo "FAIL: seeded-live-state battery exited $TEST_RC, expected 1"
    cat "$TMP_ROOT/live.out"
    exit 1
fi
if ! grep -q 'VIOLATION cooldown fixture-project:' "$TMP_ROOT/live.out"; then
    echo "FAIL: seeded live violation was not emitted by the live checker"
    cat "$TMP_ROOT/live.out"
    exit 1
fi
if ! grep -Eq 'BATTERY: FAIL \(live fleet checked; live violations=[1-9][0-9]*;' "$TMP_ROOT/live.out"; then
    echo "FAIL: seeded-live-state summary did not report live violations"
    cat "$TMP_ROOT/live.out"
    exit 1
fi

# Both arms must execute the same three stages, with numbering derived from
# STAGE_COUNT. This catches the old [1/2] header in the three-stage battery.
for output in "$TMP_ROOT/absent.out" "$TMP_ROOT/live.out"; do
    headers="$(grep -Ec '^=== \[[123]/3\]' "$output")"
    if [ "$headers" -ne 3 ]; then
        echo "FAIL: expected three numbered stage headers in $output, got $headers"
        cat "$output"
        exit 1
    fi
    if grep -q '\[1/2\]' "$output"; then
        echo "FAIL: stale [1/2] stage header in $output"
        cat "$output"
        exit 1
    fi
done

echo "PASS: SCHED-GAP-16926 absent-live-state and seeded-live-state battery arms"
exit 0
