"""SCHED-GAP-1661 regression fixtures for check 13 (class
``cooldown-pin-pacing``).

cooldown_pin_s is a promise: the lane must not re-tick more often than the
pin (the pin-first effective cooldown the engine now enforces). This battery
pins the OBSERVED half: a lane's median inter-tick gap over the last 7 days
must not sit under HALF of its effective base (cooldown_pin_s when set and
positive, else cooldown_s — mirroring the engine's resolution order).

Arms proven:

  * seeded violations fire (exit 1 naming the lane): the GAP-1661 incident
    shape (pin 604800, cooldown_s 86400, ~2h observed gaps — the pin was
    ignored live), the no-pin shape (base 86400, ~12h gaps), and a
    pin-0 row whose base falls through to cooldown_s;
  * conforming shapes exit 0: a pinned lane honoring its pin (~5d gaps),
    the exact boundary (median == base/2 is NOT under half), fewer than 3
    completed ticks (no median), cooldown_s=0 (dynamic interval,
    unmeasurable), a DISABLED lane, and a lane named in
    COOLDOWN_PIN_PACING_EXCEPTIONS carrying a violating shape (the
    documented exception path);
  * the schema-predates-columns skip is pinned: a fixture DB without
    cooldown_pin_s / ticks timestamps produces the documented skip INFO
    line, never a violation — a bare checkout's battery stays green;
  * --board-only never reads the DB, so the class cannot run in CI.

Every fixture lane gets a real workdir (check 5) and lives in a complete
namespace roster (checks 1/2) so any extra ``VIOLATION`` line means the
fixture — not the gate — regressed.

Run:  python3 -m pytest tests/test_check_fleet_invariants_cooldown_pin_pacing.py -v
"""
from __future__ import annotations

import datetime as dt
import importlib.util
import io
import sqlite3
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()

SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
# The five perf-family lanes the exception list names at introduction (the
# brief's four perf lanes + warpfs-perf, pinned the same day by the same
# fleet-cooldown-policy wave). Pinned here so silently dropping one breaks
# the fixture, not the live fleet.
PERF_EXCEPTION_LANES = (
    "duckbrain-perf",
    "ai-plays-poke-perf",
    "bunker-perf",
    "coding-hermes-scheduler-perf",
    "warpfs-perf",
)


def _iso(hours_ago: float) -> str:
    """UTC ISO stamp *hours_ago* before now (the ticks.spawned_at shape)."""
    return (dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=hours_ago)).isoformat()


def _seed_db(path: Path, lanes: list[dict], ticks: list[tuple[str, str, str]] | None = None,
             schema: str = "modern") -> Path:
    """Mint a scheduler DB fixture.

    *lanes*: dicts with name, cooldown_s, cooldown_pin_s (optional),
    enabled (default 1), admission_mode (default '' = inherit the
    namespace's cooldown), updated_at (default far in the past, so the
    era-aware window lets all seeded ticks through). Modern schema carries
    cooldown_pin_s/updated_at/admission_mode; the "old" schema omits them
    (and the ticks table) so check 13 documents its skip.
    *ticks*: (project_name, status, spawned_at) rows.
    """
    con = sqlite3.connect(path)
    if schema == "modern":
        con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER,"
                    " cooldown_s INTEGER, cooldown_pin_s INTEGER, command TEXT,"
                    " workdir TEXT, namespace_id TEXT, updated_at TEXT,"
                    " admission_mode TEXT)")
        con.execute("CREATE TABLE ticks (project_name TEXT, status TEXT, spawned_at TEXT)")
    else:
        con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER,"
                    " cooldown_s INTEGER, command TEXT, workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER,"
                " admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY[ns]))
    # SCHED-GAP-1604: a -perf fixture lane must sit in the `perf` family
    # namespace or the membership class cross-fires; seed any family namespace
    # not already in SATELLITE_NS (idempotently — doc-writer is in both).
    for ns, cap in (("perf", 1), ("doc-writer", 1), ("releases", 9)):
        con.execute("INSERT OR IGNORE INTO namespaces VALUES (?, ?, 'cooldown')", (ns, cap))
    old = "2026-01-01T00:00:00Z"  # far outside any window — all ticks in-era
    for ln in lanes:
        wd = path.parent / ln["name"]
        wd.mkdir(parents=True, exist_ok=True)
        m = gate.SATELLITE_FAMILY_RE.match(ln["name"])
        # role-matched -> family namespace; otherwise a namespace outside the
        # foremen scope (coverage only polices coding-hermes primaries).
        ns = gate.SATELLITE_FAMILY_NAMESPACES[m.group(2)] if m else "backup"
        if schema == "modern":
            con.execute("INSERT INTO projects VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        (ln["name"], ln.get("enabled", 1), ln["cooldown_s"],
                         ln.get("cooldown_pin_s"), "", str(wd), ns,
                         ln.get("updated_at", old), ln.get("admission_mode", "")))
        else:
            con.execute("INSERT INTO projects VALUES (?, ?, ?, ?, ?, ?)",
                        (ln["name"], ln.get("enabled", 1), ln["cooldown_s"],
                         "", str(wd), ns))
    for row in ticks or []:
        con.execute("INSERT INTO ticks VALUES (?, ?, ?)", row)
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, lanes: list[dict],
              ticks: list[tuple[str, str, str]] | None = None,
              schema: str = "modern") -> tuple[int, str]:
    tmp_path.mkdir(parents=True, exist_ok=True)
    db = _seed_db(tmp_path / "scheduler.db", lanes, ticks=ticks, schema=schema)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines()
            if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


def _completed_ticks(lane: str, hours_ago_list: list[float]) -> list[tuple[str, str, str]]:
    return [(lane, "completed", _iso(h)) for h in hours_ago_list]


# ── constants pin ─────────────────────────────────────────────────────────

def test_constants_are_the_documented_shape():
    """The class is registered in CHECK_CLASSES (the daily report builds its
    summary from that tuple), the window is 7 days, and the exception list
    carries the perf-family lanes with a non-empty reason each."""
    assert gate.COOLDOWN_PIN_PACING_CLASS == "cooldown-pin-pacing"
    assert gate.COOLDOWN_PIN_PACING_WINDOW_DAYS == 7
    assert "cooldown-pin-pacing" in gate.CHECK_CLASSES, gate.CHECK_CLASSES
    for lane in PERF_EXCEPTION_LANES:
        assert lane in gate.COOLDOWN_PIN_PACING_EXCEPTIONS, (
            f"{lane} missing from COOLDOWN_PIN_PACING_EXCEPTIONS")
        reason = gate.COOLDOWN_PIN_PACING_EXCEPTIONS[lane]
        assert isinstance(reason, str) and reason.strip(), (lane, reason)


# ── seeded violations must fire ───────────────────────────────────────────

def test_incident_shape_pin_ignored_fires(tmp_path):
    """THE SCHED-GAP-1661 incident shape: pin 604800 on the row but the lane
    re-ticked every ~2h (the pin was ignored live). Base resolves to the PIN
    (84h threshold); 2h median fires, exit 1, detail names the pin."""
    lane = "fixture-pinned-fast"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": 604800},
    ], ticks=_completed_ticks(lane, [2, 4, 6, 8]))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "cooldown-pin-pacing")
    assert len(lines) == 1, lines
    assert lane in lines[0], lines
    assert "168.0h" in lines[0] and "2.0h" in lines[0], lines
    assert "pin set" in lines[0], lines
    assert _other_violations(out, "cooldown-pin-pacing") == [], \
        f"unexpected extra violations:\n{out}"


def test_no_pin_cooldown_s_base_fires(tmp_path):
    """No pin: the base falls through to cooldown_s — a lane re-ticking at
    half its cooldown_s still fires (the checker is not pin-only)."""
    lane = "fixture-unpinned-fast"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": None},
    ], ticks=_completed_ticks(lane, [10, 20, 30]))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "cooldown-pin-pacing")
    assert len(lines) == 1, lines
    assert lane in lines[0] and "24.0h" in lines[0] and "10.0h" in lines[0], lines
    assert "pin not set" in lines[0], lines


def test_zero_pin_falls_through_and_fires(tmp_path):
    """A pin column of 0 is NO pin (the engine treats non-positive as
    absent): the base must fall through to cooldown_s, not disable the
    check — a 0-pin lane pacing at half its cooldown_s fires."""
    lane = "fixture-zero-pin"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": 0},
    ], ticks=_completed_ticks(lane, [10, 20, 30]))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "cooldown-pin-pacing")
    assert len(lines) == 1 and lane in lines[0], lines


# ── conforming shapes must NOT fire ───────────────────────────────────────

def test_pinned_lane_honoring_pin_is_clean(tmp_path):
    """A pinned lane whose observed gaps honor the pin: cooldown_s=3600
    under a 21600 (6h) pin, observed gaps of 6h >= the 3h half-threshold —
    exit 0, and the INFO census counts one measured lane. (A lane honoring
    the 7-day perf pin cannot show 3 in-window ticks at all — that arm is
    the documented fewer-than-3 skip, pinned separately.)"""
    lane = "fixture-pinned-slow"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 21600, "cooldown_pin_s": 43200},
    ], ticks=_completed_ticks(lane, [2, 14, 26]))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert ("INFO cooldown-pin-pacing pacing: 1 cooldown-mode lane(s) measured, "
            "0 tasks-mode lane(s) skipped (waiver), "
            "0 named exception(s)") in out, out


def test_boundary_median_exactly_half_is_clean(tmp_path):
    """median == base/2 is NOT under half — the strict-< boundary holds
    (24h gaps against a 48h threshold)."""
    lane = "fixture-boundary"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 172800, "cooldown_pin_s": None},
    ], ticks=_completed_ticks(lane, [24, 48, 72]))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_fewer_than_three_ticks_is_silently_unmeasurable(tmp_path):
    """One gap is not a cadence: fewer than 3 in-era completed ticks means
    no median — never a violation, and no per-lane line (the census in the
    INFO summary is the only trace)."""
    lane = "fixture-two-ticks"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": 604800},
    ], ticks=_completed_ticks(lane, [2, 50]))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert f"INFO cooldown-pin-pacing {lane}" not in out, out
    assert "INFO cooldown-pin-pacing pacing: 0 cooldown-mode lane(s) measured" in out, out


def test_zero_cooldown_s_dynamic_interval_skips_with_info(tmp_path):
    """cooldown_s=0 means the priority-derived dynamic interval, which the
    ticks table cannot observe — a NULL WITH A REASON (skip INFO), never a
    silent pass and never a pacing violation. (The sub-6h floor of check 3
    fires on the same row — that is check 3's fact about cooldown_s=0, not
    this class's; the assertion is scoped to cooldown-pin-pacing.)"""
    lane = "fixture-dynamic"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 0, "cooldown_pin_s": None},
    ], ticks=_completed_ticks(lane, [1, 2, 3]))

    assert rc == 1, f"gate exited {rc}, expected 1 via check 3's sub-6h floor (stdout:\n{out})"
    assert "VIOLATION cooldown-pin-pacing" not in out, out
    assert ("VIOLATION cooldown " + lane) in out, out  # check 3 owns that fact
    assert ("INFO cooldown-pin-pacing fixture-dynamic: no measurable base "
            "(cooldown_s=0, dynamic interval) — skipped") in out, out


def test_disabled_lane_is_exempt(tmp_path):
    """Disabled lanes are not dispatched — exempt, like every project-level
    check in this file."""
    lane = "fixture-disabled"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": None, "enabled": 0},
    ], ticks=_completed_ticks(lane, [1, 2, 3]))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_exception_listed_lane_with_violating_shape_is_skipped(tmp_path):
    """The documented exception path: a lane named in
    COOLDOWN_PIN_PACING_EXCEPTIONS carrying a violating shape is skipped and
    counted in the census — the four perf lanes may legitimately appear in
    the exception list while their live behavior converges."""
    lane = PERF_EXCEPTION_LANES[0]
    rc, out = _run_gate(tmp_path, [
        # On the family canonical (perf 604800, SCHED-GAP-1675): this module
        # tests check 13, so the row must not carry a family-floor violation —
        # the violating shape here is the OBSERVED cadence vs the pin.
        {"name": lane, "cooldown_s": 604800, "cooldown_pin_s": 604800},
    ], ticks=_completed_ticks(lane, [2, 4, 6]))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert ("INFO cooldown-pin-pacing pacing: 0 cooldown-mode lane(s) measured, "
            "0 tasks-mode lane(s) skipped (waiver), "
            "1 named exception(s)") in out, out


def test_pre_era_ticks_do_not_fire_after_config_change(tmp_path):
    """THE era-awareness arm: the lane's config changed recently
    (updated_at hours ago — the fleet config sync re-stamps every row), so
    the fast ticks from BEFORE the change measure the previous era and must
    not be judged against the new base. All ticks pre-era → no median → no
    violation, even though the raw 7d median would fire."""
    lane = "fixture-era-change"
    updated = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=3)).isoformat()
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 259200, "cooldown_pin_s": 259200,
         "updated_at": updated},
    ], ticks=_completed_ticks(lane, [30, 37, 44]))  # 7h gaps: fire vs the OLD 72h base

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION cooldown-pin-pacing" not in out, out
    assert "0 cooldown-mode lane(s) measured" in out, out


def test_post_era_ticks_still_fire(tmp_path):
    """The era rule must not become an amnesty: fast ticks AFTER the config
    change fire normally (updated_at in the past, ticks recent)."""
    lane = "fixture-era-fire"
    updated = (dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=6)).isoformat()
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": None,
         "updated_at": updated},
    ], ticks=_completed_ticks(lane, [6, 12, 18]))  # 6h gaps vs the 12h threshold

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "cooldown-pin-pacing")
    assert len(lines) == 1 and lane in lines[0], lines
    assert "current config era" in lines[0], lines


def test_tasks_mode_lane_skipped_by_waiver(tmp_path):
    """A tasks-admission lane re-ticking far faster than its base is the
    DOCUMENTED waiver (SCHED-GAP-124: pending board work beats cooldown) —
    its timer is not what paces it, so the class must skip it and say so in
    the census. The exact semantics this change must not touch."""
    lane = "fixture-tasks-fast"
    rc, out = _run_gate(tmp_path, [
        {"name": lane, "cooldown_s": 86400, "cooldown_pin_s": None,
         "admission_mode": "tasks"},
    ], ticks=_completed_ticks(lane, [0.5, 1.0, 1.5]))  # 30-minute gaps: fires if not skipped

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION cooldown-pin-pacing" not in out, out
    assert "1 tasks-mode lane(s) skipped (waiver)" in out, out
    assert "0 cooldown-mode lane(s) measured" in out, out


# ── the documented schema skip ────────────────────────────────────────────

def test_schema_without_pin_or_ticks_skips_with_info(tmp_path):
    """A fixture DB on the OLD schema (no cooldown_pin_s column, no ticks
    table) must produce the documented skip INFO line and NEVER a violation
    — the battery on a bare checkout stays green and the skip cannot
    masquerade as a passing assertion."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-old-schema", "cooldown_s": 86400},
    ], schema="old")

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION cooldown-pin-pacing" not in out, out
    assert ("INFO cooldown-pin-pacing schema: skipped — projects/ticks schema "
            "predates cooldown_pin_s or tick timestamps") in out, out


# ── board-only mode never runs the class ──────────────────────────────────

def test_board_only_mode_never_reads_the_db(tmp_path):
    """The CI shape: --board-only never opens the DB, so the class cannot
    run there even in principle — pinned by passing a DB path that does not
    exist and getting a clean exit anyway."""
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "definitely-not-here.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
            "--board-only",
        ])
    out = buf.getvalue()

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "cooldown-pin-pacing" not in out, out
    assert "PASS" in out, out
