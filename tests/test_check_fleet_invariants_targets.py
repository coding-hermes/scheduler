"""SCHED-GAP-147 regression fixtures for check 6 (class ``targets``).

Drives ``ops/check-fleet-invariants.py`` in-process against a seeded fixture
DB so the satellite-target gate cannot degrade into a green no-op:

  * a satellite whose target project row EXISTS but is DISABLED fires;
  * a satellite whose target row exists and is ENABLED passes;
  * a satellite whose target is neither a fleet project, nor one of the
    checker's repo candidate paths, nor an active external lane fires;
  * the documented "repo-less external data target" exemption (the
    ``targets`` INFO path): with a real workdir + a recent completed tick the
    gate prints ``INFO targets`` and stays clean — verified here with a
    REAL completed tick row, so the exemption cannot rot into a lie.

Path candidates come from the checker's own source
(``/home/kara/<base>``, ``/home/kara/<base with _>``, ``~/.hermes/<base>``);
this module picks a base name whose every candidate path is absent on any
sane machine, so both arms are CI-portable (verified on the fleet host: no
such path exists).

The seeded target row is clean on every other axis (enabled, 43200s floor
pair, supported executor, real workdir) and the primary lives in a namespace
outside the coverage gate's scope — any extra ``VIOLATION`` line means the
fixture, not the gate, regressed.

Run:  python3 -m pytest tests/test_check_fleet_invariants_targets.py -v
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

SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"
# The TARGET project (check 6's subject of scrutiny) and its satellite lane.
# Check 6 only inspects lanes named <base>-{qa,pm,dogfood,sync}, so the
# satellite carries the -qa suffix and the base is the target name.
# Every candidate path the checker derives for this base must NOT exist:
# /home/kara/fixture-target, /home/kara/fixture_target, ~/.hermes/fixture-target
PRIMARY = "fixture-target"
SAT = f"{PRIMARY}-qa"
OUT_OF_SCOPE_NS = "fixture-ns"  # primary's namespace: outside coverage's scope


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()

# Premise of the module: the chosen base name is portable — no candidate path exists.
for cand in (f"/home/kara/{PRIMARY}", f"/home/kara/{PRIMARY.replace('-', '_')}",
             str(Path.home() / ".hermes" / PRIMARY)):
    assert not Path(cand).exists(), f"fixture premise broken: {cand} exists on this machine"


def _make_db(path: Path, primary: dict | None, rows: list[dict],
             sat_ticks: list[tuple[str, str]] | None = None) -> Path:
    """Seed the fixture DB. *primary* is the target project row's attrs (or
    None to seed NO row at all — the shape that reaches check 6's fallback
    and INFO branches); *rows* the satellites; *sat_ticks* optional
    (spawned_at, status) tick tuples for the LAST satellite row (the
    INFO-path evidence)."""
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer"):
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))

    if primary is not None:
        wd = path.parent / PRIMARY
        wd.mkdir(exist_ok=True)
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt,"
            " workdir, namespace_id) VALUES (?, ?, 43200, 43200, ?, '', ?, ?)",
            (PRIMARY, primary.get("enabled", 1), SUPPORTED_COMMAND, str(wd),
             primary.get("namespace_id", OUT_OF_SCOPE_NS)))
    for r in rows:
        swd = path.parent / r["name"]
        # The satellite's workdir MUST carry the .coding-hermes/board link or
        # check 5c (boards) cross-fires — this module tests check 6 only.
        (swd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt,"
            " workdir, namespace_id) VALUES (?, ?, 43200, 43200, ?, '', ?, ?)",
            (r["name"], r.get("enabled", 1), SUPPORTED_COMMAND, str(swd),
             r.get("namespace_id", "dogfood")))
        for spawned_at, status in (sat_ticks or []):
            con.execute("INSERT INTO ticks (project_name, spawned_at, status) VALUES (?, ?, ?)",
                        (r["name"], spawned_at, status))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, primary: dict, rows: list[dict],
              sat_ticks: list[tuple[str, str]] | None = None) -> tuple[int, str]:
    db = _make_db(tmp_path / "scheduler.db", primary, rows, sat_ticks)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── seeded violations ─────────────────────────────────────────────────────

def test_target_project_disabled_fires(tmp_path):
    rc, out = _run_gate(tmp_path, {"enabled": 0}, [{"name": SAT}])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "targets") == [
        f"VIOLATION targets {SAT}: target '{PRIMARY}' is DISABLED — the lane would "
        "burn a clean-machine battery on nothing"], out
    assert _other_violations(out, "targets") == [], f"unexpected extra violations:\n{out}"


def test_target_nowhere_and_lane_inactive_fires(tmp_path):
    """No target row, no candidate path, no recent completed tick → the
    dead-weight verdict. The workdir exists but carries no completed tick, so
    the activity exemption must NOT rescue it."""
    rc, out = _run_gate(tmp_path, None, [{"name": SAT}])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "targets") == [
        f"VIOLATION targets {SAT}: target '{PRIMARY}' is neither a fleet project nor a "
        "path on disk, and the lane shows no recent activity"], out
    assert _other_violations(out, "targets") == [], f"unexpected extra violations:\n{out}"


# ── the documented INFO exemption, proven with real tick evidence ─────────

def test_external_active_lane_is_info_not_violation(tmp_path):
    """Repo-less external target + real workdir + completed tick within 7 days
    → INFO line, exit 0. Seeding the tick row in the DB (not faking the
    verdict) is what makes this proof rather than decoration."""
    recent = (dt.datetime.now() - dt.timedelta(hours=2)).isoformat()
    rc, out = _run_gate(tmp_path, None, [{"name": SAT}], sat_ticks=[(recent, "completed")])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert f"INFO targets {SAT}: external data target '{PRIMARY}'" in out, out


# ── clean fleet ───────────────────────────────────────────────────────────

def test_target_project_enabled_passes(tmp_path):
    rc, out = _run_gate(tmp_path, {"enabled": 1, "namespace_id": OUT_OF_SCOPE_NS},
                        [{"name": SAT}], sat_ticks=[("2026-09-19T00:00:00", "completed")])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
