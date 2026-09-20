"""SCHED-GAP-147 regression fixtures for checks 1-2 (classes ``caps`` and
``admission``).

Drives ``ops/check-fleet-invariants.py`` in-process against a seeded fixture
DB so neither class can degrade into a green no-op:

  * caps — the foreman namespace must sit at its guaranteed cap (8); every
    satellite namespace must be capped at exactly 1; a missing satellite
    namespace is itself a caps violation;
  * admission — every namespace carries an admission_mode; ``tasks`` ONLY on
    ``coding-hermes``; every satellite namespace on ``cooldown``.

Both arms are proven per class: a seeded violation exits 1 naming the exact
subject, the conforming fleet exits 0. The fixture DB carries NO project rows
— every project-level check (cooldown/executors/workdirs/adaptive/boards/
coverage/family-floor/targets) is clean by construction on an empty fleet, so
any extra ``VIOLATION`` line means the fixture (not the gate) regressed.

Run:  python3 -m pytest tests/test_check_fleet_invariants_caps_and_admission.py -v
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


def _seed_ns_db(path: Path, foreman: tuple[int, str] = (8, "tasks"),
                sat_caps: dict[str, int] | None = None,
                sat_modes: dict[str, str] | None = None,
                drop: tuple[str, ...] = ()) -> Path:
    """Mint a namespaces-only scheduler DB (no projects → every project-level
    check is vacuously clean). *foreman* is (max_concurrent, admission_mode);
    per-satellite overrides come via *sat_caps* / *sat_modes*; names in
    *drop* get no row at all (the missing-namespace shape)."""
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " command TEXT, prompt TEXT, workdir TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', ?, ?)", foreman)
    for ns in SATELLITE_NS:
        if ns in drop:
            continue
        con.execute("INSERT INTO namespaces VALUES (?, ?, ?)",
                    (ns, (sat_caps or {}).get(ns, 1), (sat_modes or {}).get(ns, "cooldown")))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, **seed_kwargs) -> tuple[int, str]:
    db = _seed_ns_db(tmp_path / "scheduler.db", **seed_kwargs)
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


# ── constants pin ─────────────────────────────────────────────────────────

def test_caps_constants_are_the_documented_shape():
    """The foreman cap is 8 and the satellite-namespace roster is the six
    families; drifting either constant silently re-levels the whole fleet."""
    assert gate.FOREMAN_NS == "coding-hermes"
    assert gate.FOREMAN_CAP_EXPECTED == 8, gate.FOREMAN_CAP_EXPECTED
    assert gate.SATELLITE_NS == SATELLITE_NS, gate.SATELLITE_NS


# ── caps: seeded violations must fire ─────────────────────────────────────

def test_foreman_cap_below_8_fires(tmp_path):
    rc, out = _run_gate(tmp_path, foreman=(4, "tasks"))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "caps")
    assert lines == ["VIOLATION caps coding-hermes: max_concurrent=4 expected 8 "
                     "(the foremen's guaranteed room)"], lines
    assert _other_violations(out, "caps") == [], f"unexpected extra violations:\n{out}"


def test_satellite_cap_above_1_fires(tmp_path):
    rc, out = _run_gate(tmp_path, sat_caps={"qa": 3})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "caps")
    assert len(lines) == 1 and lines[0].startswith("VIOLATION caps qa: max_concurrent=3 expected 1"), lines
    assert _other_violations(out, "caps") == [], f"unexpected extra violations:\n{out}"


def test_missing_satellite_namespace_fires(tmp_path):
    rc, out = _run_gate(tmp_path, drop=("doc-writer",))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "caps")
    assert lines == ["VIOLATION caps doc-writer: namespace missing"], lines
    assert _other_violations(out, "caps") == [], f"unexpected extra violations:\n{out}"


# ── admission: seeded violations must fire ────────────────────────────────

def test_admission_defects_all_fire(tmp_path):
    """Three admission shapes at once: foreman off ``tasks``, a satellite on
    ``tasks``, and a satellite with NO mode. Each must get its own line."""
    rc, out = _run_gate(tmp_path, foreman=(8, "cooldown"),
                        sat_modes={"qa": "tasks", "pm": ""})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert ("VIOLATION admission coding-hermes: mode=cooldown — the foremen namespace "
            "must be tasks (fast with work, timer when perpetual-only)") in out, out
    assert ("VIOLATION admission qa: mode=tasks — satellite namespaces must be "
            "timer-paced (cooldown)") in out, out
    assert "VIOLATION admission pm: no admission_mode set" in out, out
    assert len(_violations(out, "admission")) == 3, out
    assert _other_violations(out, "admission") == [], f"unexpected extra violations:\n{out}"


# ── clean fleet must pass ─────────────────────────────────────────────────

def test_clean_caps_and_admission_pass(tmp_path):
    rc, out = _run_gate(tmp_path)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "PASS" in out, out
