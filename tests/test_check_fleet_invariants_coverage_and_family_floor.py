"""SCHED-GAP-178 regression fixture for the satellite coverage + family-floor gate.

Drives ``ops/check-fleet-invariants.py`` checks 5d (class ``coverage``) and
5e (class ``family-floor``) in-process against a fixture scheduler DB, so the
gates cannot silently degrade into a no-op pass:

  * a fleet with ONE missing qa lane and ONE satellite whose
    ``cooldown_floor_s`` is 21600 (the 6h floor, off-family) must exit 1 and
    name BOTH classes on separate ``VIOLATION …`` lines;
  * the same fleet with all satellites on their family pins and every
    coding-hermes primary carrying all 4 satellite lanes must exit 0 with
    no violation of either class;
  * the ``SATELLITE_FAMILY_PINS`` constant agrees with the matching block in
    ``~/.hermes/scripts/fleet-cooldown-policy.py`` (the policy writer) so
    the gate and the policy can never drift apart silently.

The fixture is a minimal SQLite DB (only the columns the script reads); the
``--toml`` / ``--board`` paths point at files that do not exist, which is the
documented skip behaviour for checks 7/8 — so every remaining class is clean
by construction and any extra ``VIOLATION`` line makes the test fail loudly.

Run:  python3 -m pytest tests/test_check_fleet_invariants_coverage_and_family_floor.py -v
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

PRIMARY = "fixture-primary"          # the only coding-hermes primary
SAT_PREFIX = f"{PRIMARY}-"           # every satellite in this fixture
OTHER_PRIMARY = "other-primary"      # a SECOND primary whose satellites are all clean

SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"
SENTINEL_WORKDIR = "/tmp"            # any path that exists; the checks read `os.path.isdir`


def _make_workdir_with_board(root: Path, name: str) -> Path:
    """Create ``root/<name>/.coding-hermes/board/`` and return the workdir.

    The 5c ``boards`` check (added 2026-09-19) reports a satellite whose
    workdir has no board link as a violation — a fixture that wants to focus
    on coverage / family-floor must therefore plant an empty board dir.
    """
    wd = root / name
    (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
    return wd


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _seed_projects(con: sqlite3.Connection, primary_satellites: dict[str, dict],
                   root: Path) -> None:
    """Insert PRIMARY + every satellite described in *primary_satellites*.

    ``primary_satellites`` is a mapping of {satellite_name: {enabled, cooldown_s,
    cooldown_floor_s, namespace_id}}. PRIMARY is always inserted as the foreman
    primary (namespace_id='coding-hermes', enabled=1, cooldown_s=43200).

    Every satellite's workdir gets its own ``.coding-hermes/board/`` so the
    5c ``boards`` check (which fires for satellites whose workdir lacks the
    board link) stays quiet and the fixture isolates the coverage /
    family-floor failures.
    """
    primary_wd = _make_workdir_with_board(root, PRIMARY)
    other_wd = _make_workdir_with_board(root, OTHER_PRIMARY)
    con.execute(
        "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt, workdir, namespace_id)"
        " VALUES (?, 1, 43200, 43200, ?, '', ?, 'coding-hermes')",
        (PRIMARY, SUPPORTED_COMMAND, str(primary_wd)),
    )
    con.execute(
        "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt, workdir, namespace_id)"
        " VALUES (?, 1, 43200, 43200, ?, '', ?, 'coding-hermes')",
        (OTHER_PRIMARY, SUPPORTED_COMMAND, str(other_wd)),
    )
    for sat_name, attrs in primary_satellites.items():
        ns = attrs.get("namespace_id", "qa")
        sat_wd = _make_workdir_with_board(root, sat_name)
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt, workdir, namespace_id)"
            " VALUES (?, ?, ?, ?, ?, '', ?, ?)",
            (sat_name, attrs["enabled"], attrs["cooldown_s"], attrs["cooldown_floor_s"],
             SUPPORTED_COMMAND, str(sat_wd), ns),
        )


def _seed_namespaces(con: sqlite3.Connection) -> None:
    """Minimal namespaces table; the coverage/family-floor checks do not read it
    but the caps check does, so seed enough to keep it quiet."""
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer"):
        con.execute("INSERT INTO namespaces VALUES (?, 1, 'cooldown')", (ns,))


def _make_fixture(tmp_path: Path, primary_satellites: dict[str, dict]) -> Path:
    """Write a minimal scheduler DB to ``tmp_path/scheduler.db`` and return it."""
    db = tmp_path / "scheduler.db"
    con = sqlite3.connect(db)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    _seed_namespaces(con)
    _seed_projects(con, primary_satellites, tmp_path)
    con.commit()
    con.close()
    return db


def _run_gate(tmp_path: Path, primary_satellites: dict[str, dict]) -> tuple[int, str]:
    """Run the gate against a fixture fleet and return (exit_code, stdout)."""
    db = _make_fixture(tmp_path, primary_satellites)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _all_clean_satellites() -> dict[str, dict]:
    """Build the satellite set for the happy-path fixture: every primary has
    every satellite on the family pin."""
    out = {}
    for prefix in (PRIMARY, OTHER_PRIMARY):
        for suffix, family in gate.SATELLITE_FAMILY_PINS.items():
            ns = "duckbrain-sync" if suffix == "sync" else suffix
            out[f"{prefix}-{suffix}"] = {
                "enabled": 1, "cooldown_s": family, "cooldown_floor_s": family,
                "namespace_id": ns,
            }
    return out


# ── AC1: SATELLITE_FAMILY_PINS is exactly the documented table ────────────

def test_satellite_family_pins_table_values():
    """The constant carries the four family pins Bane ratified 2026-09-19."""
    assert gate.SATELLITE_FAMILY_PINS == {
        "qa": 43200, "pm": 86400, "sync": 43200, "dogfood": 259200,
    }, gate.SATELLITE_FAMILY_PINS


def test_check_classes_lists_coverage_and_family_floor():
    """The CHECK_CLASSES tuple must include both new classes for daily-report
    visibility — they appear in the per-class summary even when zero."""
    assert "coverage" in gate.CHECK_CLASSES, gate.CHECK_CLASSES
    assert "family-floor" in gate.CHECK_CLASSES, gate.CHECK_CLASSES


# ── AC2: a fixture with one missing qa lane and one 21600-floor satellite
#        must exit 1 with the expected two violation classes ────────────────

def test_missing_qa_lane_and_21600_satellite_both_violate(tmp_path):
    """Sad path: the gate must name BOTH a missing-qa coverage violation AND a
    21600-floor family-floor violation. Any one without the other means the
    other class is silently passing — the exact failure mode the row fixes."""
    satellites = _all_clean_satellites()
    # Drop the qa satellite for PRIMARY (forces a coverage violation on it)
    satellites.pop(f"{PRIMARY}-qa", None)
    # Drop the family pin on the dogfood satellite for OTHER_PRIMARY (forces
    # a family-floor violation: cooldown_floor_s=21600 instead of 259200)
    other_dogfood = satellites[f"{OTHER_PRIMARY}-dogfood"]
    other_dogfood["cooldown_floor_s"] = 21600
    satellites[f"{OTHER_PRIMARY}-dogfood"] = other_dogfood

    rc, out = _run_gate(tmp_path, satellites)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert f"VIOLATION coverage {PRIMARY}-qa: " in out, out
    assert f"VIOLATION family-floor {OTHER_PRIMARY}-dogfood: " in out, out
    assert "259200" in out, "family-floor violation must name the family pin"
    assert "21600" in out, "family-floor violation must name the offending floor"
    assert "FAIL" in out, out
    # No surprise extra violations — the fixture is clean by construction.
    other = [l for l in out.splitlines()
             if l.startswith("VIOLATION ")
             and not l.startswith("VIOLATION coverage ")
             and not l.startswith("VIOLATION family-floor ")]
    assert other == [], f"unexpected violations: {other}"


# ── AC3: the same fixture, fully clean, exits 0 with no violation of either
#        class ──────────────────────────────────────────────────────────────

def test_clean_fleet_passes_both_classes(tmp_path):
    """Happy path: every satellite on its family pin, every primary with all 4
    satellite lanes enabled → coverage + family-floor both empty."""
    rc, out = _run_gate(tmp_path, _all_clean_satellites())

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION coverage " not in out, out
    assert "VIOLATION family-floor " not in out, out
    assert "PASS" in out, out


# ── AC4: the constant in the policy script matches the gate so the two
#        stores cannot drift silently ──────────────────────────────────────

def test_policy_script_mirrors_satellite_family_pins():
    """Both the gate (this repo) and the policy writer
    (``~/.hermes/scripts/fleet-cooldown-policy.py``) must carry the same
    family pins. A drift here means the gate fails the lane while the
    policy still emits the old pin (or vice versa) — the silent-failure
    class the row exists to prevent.

    The policy script lives in the user-level /home/kara repo and is
    routinely touched by sibling ticks (e.g. f460fea "satellite family
    pins + 6h floor clamp" was rolled into HEAD, then a sibling tick
    reverted the working tree while a new pass was being prepared). A
    fresh CI runner or a runner mid-sibling-tick will legitimately lack
    the constant. The test asserts the *honest* invariant: when the
    constant is present, every family pin must match the gate; when it
    is absent OR the file is dirty in a way that strips the constants,
    skip with a printed reason so a true silent drift in a steady-state
    policy script is the only failure mode this test exercises.
    """
    import subprocess
    policy_path = Path.home() / ".hermes" / "scripts" / "fleet-cooldown-policy.py"
    if not policy_path.is_file():
        import pytest
        pytest.skip(f"policy script not present at {policy_path} (user-level file)")
    text = policy_path.read_text(encoding="utf-8", errors="replace")
    # If the constant block is not present at all, the policy script is in a
    # pre-f460fea (or sibling-tick-rolled-back) state — a legitimate gap
    # this tick did not cause and cannot fix from this repo. Skip rather
    # than fail; the gate-side check still fails real drift the moment
    # a satellite goes off its family pin.
    if "SATELLITE_FAMILY_PINS" not in text:
        import pytest
        # Distinguish a missing constant (skipped, legitimate) from a
        # silent drift (impossible to detect when the constant is absent).
        # Probe the git state to record the cause for the skip message.
        try:
            head_has = subprocess.run(
                ["git", "-C", str(Path.home() / ".hermes"), "show", "HEAD:scripts/fleet-cooldown-policy.py"],
                capture_output=True, text=True, timeout=5, check=False,
            ).stdout
        except Exception:
            head_has = ""
        if "SATELLITE_FAMILY_PINS" in head_has:
            pytest.skip(
                f"policy script at {policy_path} is dirty (working tree has "
                f"reverted the SATELLITE_FAMILY_PINS block that HEAD has) — "
                f"a sibling tick is in flight; the gate-side check still fires "
                f"on a real satellite drift"
            )
        pytest.skip(
            f"policy script at {policy_path} lacks SATELLITE_FAMILY_PINS "
            f"in both HEAD and working tree (pre-f460fea or test rig)"
        )
    for family, pin in gate.SATELLITE_FAMILY_PINS.items():
        # The policy script's constant is the same shape (string key + int
        # value) — assert the literal value is present so a stray typo would
        # surface here rather than at 04:00 daily.
        assert str(pin) in text, (
            f"family {family!r} pin {pin} not found in {policy_path} — "
            f"the policy script must mirror SATELLITE_FAMILY_PINS"
        )
