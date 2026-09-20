"""SCHED-GAP-147 regression fixtures for checks 3-5 (classes ``cooldown``,
``executors``, ``workdirs``).

Drives ``ops/check-fleet-invariants.py`` in-process against a seeded fixture
DB; each class is proven in BOTH arms:

  * cooldown — an enabled lane below the 21600s (6h) floor fires; a lane at a
    documented tier value (qa-audit 24h / release-engineer 7d) is exempt; a
    tier lane drifted OFF its documented value fires; a DISABLED lane is
    exempt from the floor;
  * executors — an enabled lane whose ``command`` carries a retired driver
    script fires with the script named; the same content carrying the
    ``RETIRED`` / ``do NOT run`` marker is exempt; a supported executor
    passes;
  * workdirs — an enabled lane whose workdir is empty or nonexistent fires;
    a real directory passes; a DISABLED lane with a garbage workdir is
    exempt.

The cooldown/executors fixtures seed ONE enabled lane on a real temp workdir
with a supported executor at 43200s — clean by construction — so every extra
``VIOLATION`` line means the fixture (not the gate) regressed. The workdirs
arms pass an EMPTY project set plus full namespaces so no other class can
fire.

Run:  python3 -m pytest tests/test_check_fleet_invariants_cooldown_executors_workdirs.py -v
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

SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")


def _seed_namespaces(con: sqlite3.Connection) -> None:
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, 1, 'cooldown')", (ns,))


def _make_db(path: Path, rows: list[dict]) -> Path:
    """Mint a scheduler DB with the given project rows (and clean namespaces).

    Row keys: name, enabled, cooldown_s, command, workdir, namespace_id,
    prompt. Unlisted keys default to the fleet-clean values."""
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    _seed_namespaces(con)
    for r in rows:
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt,"
            " workdir, namespace_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (r["name"], r.get("enabled", 1), r.get("cooldown_s", 43200),
             r.get("cooldown_floor_s", 43200), r.get("command", SUPPORTED_COMMAND),
             r.get("prompt", ""), r.get("workdir", ""), r.get("namespace_id", "coding-hermes")))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, rows: list[dict]) -> tuple[int, str]:
    db = _make_db(tmp_path / "scheduler.db", rows)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _clean_lane(tmp_path: Path, **overrides) -> dict:
    """One enabled lane on a REAL temp workdir (workdirs check quiet) with a
    supported executor (executors quiet), 43200s (floor quiet), in the
    coding-hermes namespace BUT named so the coverage check skips it: not a
    satellite, and its namespace_id is not 'coding-hermes' (namespace_id is
    the checker's primary discriminator, so a non-fleet namespace id keeps
    coverage out of these fixtures' scope — family-floor, adaptive and boards
    skip it because the name carries no satellite suffix)."""
    row = {"name": LANE, "workdir": str(tmp_path), "namespace_id": "fixture-ns"}
    row.update(overrides)
    return row


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── cooldown ──────────────────────────────────────────────────────────────

def test_cooldown_below_floor_fires(tmp_path):
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, cooldown_s=3600)])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "cooldown") == [
        "VIOLATION cooldown fixture-lane: cooldown_s=3600 below the 21600s (6h) floor — "
        "sub-6h pins are retired"], out
    assert _other_violations(out, "cooldown") == [], f"unexpected extra violations:\n{out}"


def test_cooldown_tier_lane_at_documented_value_is_exempt(tmp_path):
    """qa-audit at its documented 24h tier must NOT fire (floor exempt)."""
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, name="qa-audit", cooldown_s=86400)])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_cooldown_tier_lane_drifted_off_value_fires(tmp_path):
    """release-engineer moved off its documented 7d pin is still a violation."""
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, name="release-engineer", cooldown_s=86400)])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "cooldown") == [
        "VIOLATION cooldown release-engineer: cooldown_s=86400 != documented tier 604800"], out
    assert _other_violations(out, "cooldown") == [], f"unexpected extra violations:\n{out}"


def test_cooldown_disabled_lane_is_exempt(tmp_path):
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, enabled=0, cooldown_s=600)])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── executors ─────────────────────────────────────────────────────────────

def test_retired_driver_in_command_fires(tmp_path):
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, command=RETIRED_COMMAND)])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "executors") == [
        "VIOLATION executors fixture-lane: instructs the retired driver pm-standin-tick.sh — "
        "lanes run their skill"], out
    assert _other_violations(out, "executors") == [], f"unexpected extra violations:\n{out}"


def test_retired_driver_in_prompt_also_fires(tmp_path):
    rc, out = _run_gate(tmp_path, [_clean_lane(tmp_path, prompt="run via qa-scheduler-tick.sh nightly")])


    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "executors") == [
        "VIOLATION executors fixture-lane: instructs the retired driver qa-scheduler-tick.sh — "
        "lanes run their skill"], out
    assert _other_violations(out, "executors") == [], f"unexpected extra violations:\n{out}"


def test_retired_driver_marked_retired_is_exempt(tmp_path):
    """The documented exemption: same driver named but explicitly marked."""
    rc, out = _run_gate(tmp_path, [_clean_lane(
        tmp_path, command=f"echo note: {RETIRED_COMMAND} is RETIRED — do not use")])


    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── workdirs ──────────────────────────────────────────────────────────────

def test_workdir_missing_fires(tmp_path):
    rc, out = _run_gate(tmp_path, [{"name": LANE, "enabled": 1, "workdir": "",
                                    "namespace_id": "fixture-ns"}])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "workdirs") == [
        "VIOLATION workdirs fixture-lane: workdir missing: ''"], out
    assert _other_violations(out, "workdirs") == [], f"unexpected extra violations:\n{out}"


def test_workdir_nonexistent_path_fires(tmp_path):
    missing = str(tmp_path / "does-not-exist")
    rc, out = _run_gate(tmp_path, [{"name": LANE, "enabled": 1, "workdir": missing,
                                    "namespace_id": "fixture-ns"}])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "workdirs") == [
        f"VIOLATION workdirs fixture-lane: workdir missing: '{missing}'"], out
    assert _other_violations(out, "workdirs") == [], f"unexpected extra violations:\n{out}"


def test_workdir_real_dir_passes(tmp_path):
    real = tmp_path / "lane-home"
    real.mkdir()
    rc, out = _run_gate(tmp_path, [{"name": LANE, "enabled": 1, "workdir": str(real),
                                    "namespace_id": "fixture-ns"}])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_workdir_disabled_lane_exempt(tmp_path):
    rc, out = _run_gate(tmp_path, [{"name": LANE, "enabled": 0, "workdir": "/definitely/not/here",
                                    "namespace_id": "fixture-ns"}])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
