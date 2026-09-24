"""SCHED-GAP-150 regression fixture for the retired-driver gate.

Drives ``ops/check-fleet-invariants.py`` check #4 (class ``executors``)
in-process against a fixture scheduler DB, so the gate cannot silently degrade
into a no-op pass:

  * a fleet with ONE enabled lane whose ``command`` drives a retired driver must
    exit 1 and name that lane on a ``VIOLATION executors <lane>`` line;
  * the same fleet with a supported executor must exit 0 with no violation.

The fixture is a minimal SQLite DB (only the columns the script reads) and the
``--toml`` / ``--board`` paths point at files that do not exist, which is the
documented skip behaviour for checks 7/8 — so every remaining class is clean by
construction and any extra ``VIOLATION`` line makes the test fail loudly.

Run:  python3 -m pytest tests/test_check_fleet_invariants_retired_drivers.py -v
"""
from __future__ import annotations

import importlib.util
import io
import sqlite3
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"

# A real, non-retired executor script (the foreman tick the live fleet runs).
SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"
RETIRED_COMMAND = "bash /home/kara/.hermes/scripts/pm-standin-tick.sh"
LANE = "fixture-lane"


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _seed_fleet_db(path: Path, command: str, workdir: Path) -> None:
    """Mint a minimal scheduler DB whose only enabled lane runs *command*.

    Columns: exactly the ones check-fleet-invariants.py reads. Namespace rows:
    the foreman namespace at its expected cap plus one row per satellite family,
    because check #1 reports a missing satellite namespace as a caps violation.
    """
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " command TEXT, prompt TEXT, workdir TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer"):
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))
    con.execute(
        "INSERT INTO projects (name, enabled, cooldown_s, command, prompt, workdir)"
        " VALUES (?, 1, 43200, ?, '', ?)",
        (LANE, command, str(workdir)),
    )
    con.commit()
    con.close()


def _run_gate(tmp_path: Path, command: str) -> tuple[int, str]:
    """Run the gate against a fixture fleet and return (exit_code, stdout)."""
    db = tmp_path / "scheduler.db"
    _seed_fleet_db(db, command, tmp_path)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def test_retired_drivers_tuple_lists_five_scripts():
    """AC1: the Python list carries the 5th driver, dagger-role-tick.sh."""
    assert len(gate.RETIRED_DRIVERS) == 5, gate.RETIRED_DRIVERS
    assert "dagger-role-tick.sh" in gate.RETIRED_DRIVERS
    assert gate.RETIRED_DRIVERS == (
        "pm-standin-tick.sh", "qa-scheduler-tick.sh", "sync-scheduler-tick.sh",
        "dogfood-scheduler-tick.sh", "dagger-role-tick.sh",
    )


def test_retired_driver_on_enabled_lane_is_a_violation(tmp_path):
    """Sad path: the gate must exit 1 and name the lane — not pass silently."""
    rc, out = _run_gate(tmp_path, RETIRED_COMMAND)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert f"VIOLATION executors {LANE}: " in out, out
    assert "pm-standin-tick.sh" in out, out
    assert "FAIL" in out, out
    # The fixture is clean by construction, so the executors finding must be the
    # only one — any extra class means the fixture (not the gate) regressed.
    other = [l for l in out.splitlines()
             if l.startswith("VIOLATION ") and not l.startswith("VIOLATION executors")]
    assert other == [], f"unexpected non-executors violations: {other}"


def test_clean_fleet_passes(tmp_path):
    """Happy path: a supported executor is not flagged."""
    rc, out = _run_gate(tmp_path, SUPPORTED_COMMAND)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "PASS" in out, out
