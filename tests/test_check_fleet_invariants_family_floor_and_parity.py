"""SCHED-GAP-147 regression fixtures for checks 5e and 7 (classes
``family-floor`` and ``parity``), plus the whole-fleet PASSING fixture the
row demands.

``parity`` is the DB↔fleet.toml agreement gate (cooldown_s / cooldown_floor_s
/ cooldown_ceiling_s / board_ownership per project; admission_mode /
max_concurrent per namespace) — a pin in one store only is drift, so each arm
proves the checker can DISTINGUISH agreement from drift, not merely fire:

  * family-floor — an enabled satellite drifted off its family pin (cooldown
    OR floor alone) fires; a satellite on its family pin passes;
  * parity (projects) — a stale cooldown_s pin in fleet.toml fires; matching
    pins pass; a row ABSENT from fleet.toml is skipped by design (the toml is
    a partial mirror, not a full mirror);
  * parity (namespaces) — admission_mode / max_concurrent disagreement on a
    namespace block fires, matching blocks pass;
  * board_ownership — present in the DB schema AND populated in both stores:
    disagreement fires; agreement passes; the column ABSENT from the schema
    (older DB) is skipped, not passed.

The module ends with the row's PASSING-FLEET fixture: a full clean fleet
(namespaces, a primary with all four satellites on family pins, real
workdirs, board links, a conforming fleet.toml) must exit 0 against the
complete checker — no class fires anywhere.

Run:  python3 -m pytest tests/test_check_fleet_invariants_family_floor_and_parity.py -v
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
FAMILIES = tuple(gate.SATELLITE_FAMILY_PINS)  # qa, pm, sync, dogfood
NS_FOR_SUFFIX = {s: ("duckbrain-sync" if s == "sync" else s) for s in FAMILIES}


def _board_dir(root: Path, name: str) -> Path:
    wd = root / name
    (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
    return wd


def _make_db(path: Path, projects: list[dict], *,
             ownership_column: bool = True) -> Path:
    con = sqlite3.connect(path)
    cols = ("name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER, cooldown_floor_s INTEGER,"
            " cooldown_ceiling_s INTEGER, adaptive_cooldown INTEGER, command TEXT, prompt TEXT,"
            " workdir TEXT, namespace_id TEXT"
            + (", board_ownership TEXT" if ownership_column else ""))
    con.execute(f"CREATE TABLE projects ({cols})")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, 1, 'cooldown')", (ns,))
    for p in projects:
        names = ("name, enabled, cooldown_s, cooldown_floor_s, cooldown_ceiling_s,"
                 " adaptive_cooldown, command, prompt, workdir, namespace_id"
                 + (", board_ownership" if ownership_column else ""))
        marks = ",".join("?" * (10 + (1 if ownership_column else 0)))
        wd = path.parent / p["name"]
        (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
        row = [p["name"], p.get("enabled", 1), p.get("cooldown_s", 43200),
               p.get("cooldown_floor_s", 43200), p.get("cooldown_ceiling_s"),
               p.get("adaptive_cooldown", 0), p.get("command", SUPPORTED_COMMAND),
               p.get("prompt", ""), str(wd), p.get("namespace_id", "fixture-ns")]
        if ownership_column:
            row.append(p.get("board_ownership"))  # None → NULL (no pin) unless given
        con.execute(f"INSERT INTO projects ({names}) VALUES ({marks})", row)
    con.commit()
    con.close()
    return path


def _conforming_toml(toml_path: Path, projects: list[dict]) -> None:
    """Write a fleet.toml whose every [[projects]] / [[namespaces]] block
    AGREES with the seeded DB rows (and carries no global cap — the global
    cap parity lives in test_check_fleet_invariants_global_cap_parity.py)."""
    lines = []
    for p in projects:
        lines.append("[[projects]]")
        lines.append(f'id = "{p["name"]}"')
        lines.append(f'cooldown_s = {p.get("cooldown_s", 43200)}')
        lines.append(f'cooldown_floor_s = {p.get("cooldown_floor_s", 43200)}')
        if p.get("cooldown_ceiling_s") is not None:
            lines.append(f'cooldown_ceiling_s = {p["cooldown_ceiling_s"]}')
        if p.get("board_ownership"):
            lines.append(f'board_ownership = "{p["board_ownership"]}"')
        lines.append("")
    for ns in ("coding-hermes", *SATELLITE_NS):
        lines.append("[[namespaces]]")
        lines.append(f'id = "{ns}"')
        lines.append('admission_mode = "tasks"' if ns == "coding-hermes"
                     else 'admission_mode = "cooldown"')
        lines.append("max_concurrent = 8" if ns == "coding-hermes" else "max_concurrent = 1")
        lines.append("")
    toml_path.write_text("\n".join(lines), encoding="utf-8")


def _run_gate(tmp_path: Path, projects: list[dict], toml_text: str | None = None,
              ownership_column: bool = True) -> tuple[int, str]:
    db = _make_db(tmp_path / "scheduler.db", projects, ownership_column=ownership_column)
    toml = tmp_path / "fleet.toml"
    toml.write_text(toml_text if toml_text is not None else "", encoding="utf-8")
    if toml_text is None:
        _conforming_toml(toml, projects)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(toml),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── family-floor ──────────────────────────────────────────────────────────

def _sat(name: str, suffix: str, cd: int | None = None, fl: int | None = None) -> dict:
    pin = gate.SATELLITE_FAMILY_PINS[suffix]
    return {"name": name, "cooldown_s": cd if cd is not None else pin,
            "cooldown_floor_s": fl if fl is not None else pin,
            "namespace_id": NS_FOR_SUFFIX[suffix]}


def test_family_floor_drifted_cooldown_fires(tmp_path):
    rc, out = _run_gate(tmp_path, [
        # the satellite's target project (keeps the targets check quiet)
        {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200},
        _sat("fixture-primary-pm", "pm", cd=21600),
    ])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "family-floor") == [
        "VIOLATION family-floor fixture-primary-pm: cooldown_s=21600 / cooldown_floor_s=86400"
        " — satellite family expects 86400 for pm (see SATELLITE_FAMILY_PINS, also mirrored"
        " in fleet-cooldown-policy.py)"], out
    assert _other_violations(out, "family-floor") == [], f"unexpected extra violations:\n{out}"


def test_family_floor_drifted_floor_only_fires(tmp_path):
    """The floor ALONE drifting is enough — the pair is checked together."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200},
        _sat("fixture-primary-dogfood", "dogfood", fl=21600),
    ])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "family-floor") == [
        "VIOLATION family-floor fixture-primary-dogfood: cooldown_s=259200 / cooldown_floor_s=21600"
        " — satellite family expects 259200 for dogfood (see SATELLITE_FAMILY_PINS, also mirrored"
        " in fleet-cooldown-policy.py)"], out
    assert _other_violations(out, "family-floor") == [], f"unexpected extra violations:\n{out}"


def test_family_floor_on_pin_passes(tmp_path):
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200},
        _sat("fixture-primary-qa", "qa"),
    ])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── parity: projects ──────────────────────────────────────────────────────

def test_parity_stale_project_cooldown_pin_fires(tmp_path):
    """fleet.toml pins 86400 while the DB row says 43200 — drift, both arms
    of the check exercised in one fleet (the second project agrees)."""
    projects = [
        {"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200},
        {"name": "fixture-lane-b", "cooldown_s": 43200, "cooldown_floor_s": 43200},
    ]
    toml = """
[[projects]]
id = "fixture-lane-a"
cooldown_s = 86400
cooldown_floor_s = 43200

[[projects]]
id = "fixture-lane-b"
cooldown_s = 43200
cooldown_floor_s = 43200
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "parity") == [
        "VIOLATION parity fixture-lane-a: cooldown_s: db=43200 toml=86400 — a pin in one store is drift"], out
    assert _other_violations(out, "parity") == [], f"unexpected extra violations:\n{out}"


def test_parity_agreeing_project_pins_pass(tmp_path):
    projects = [{"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200,
                 "cooldown_ceiling_s": 345600, "board_ownership": "coding-hermes"}]
    toml = """
[[projects]]
id = "fixture-lane-a"
cooldown_s = 43200
cooldown_floor_s = 43200
cooldown_ceiling_s = 345600
board_ownership = "coding-hermes"
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_parity_row_absent_from_toml_is_skipped_by_design(tmp_path):
    """An enabled project with NO [[projects]] block is not a parity subject:
    the toml is a partial mirror. Pinned so nobody turns this into a false
    'missing pin = drift' violation (loader.go treats absent keys the same
    way)."""
    projects = [{"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200}]
    rc, out = _run_gate(tmp_path, projects, toml_text="")

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION parity" not in out, out


# ── parity: namespaces ────────────────────────────────────────────────────

def test_parity_namespace_mode_and_cap_disagreement_fires(tmp_path):
    projects = [{"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200}]
    toml = """
[[namespaces]]
id = "coding-hermes"
admission_mode = "cooldown"
max_concurrent = 4

[[namespaces]]
id = "qa"
admission_mode = "cooldown"
max_concurrent = 1
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "parity")
    assert got == [
        "VIOLATION parity coding-hermes: admission_mode: db=tasks toml=cooldown",
        "VIOLATION parity coding-hermes: max_concurrent: db=8 toml=4"], got
    assert _other_violations(out, "parity") == [], f"unexpected extra violations:\n{out}"


# ── parity: board_ownership schema gates ─────────────────────────────────

def test_parity_board_ownership_disagreement_fires(tmp_path):
    projects = [{"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200,
                 "board_ownership": "coding-hermes"}]
    toml = """
[[projects]]
id = "fixture-lane-a"
cooldown_s = 43200
cooldown_floor_s = 43200
board_ownership = "other-value"
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "parity") == [
        "VIOLATION parity fixture-lane-a: board_ownership: db='coding-hermes' toml='other-value'"], out
    assert _other_violations(out, "parity") == [], f"unexpected extra violations:\n{out}"


def test_parity_board_ownership_absent_column_is_skipped_not_passed(tmp_path):
    """Older DB without the board_ownership column: the checker SKIPS the
    field (have_ownership=False). The fixture asserts the skip shape — exit 0
    with NO parity line — while the doc table records that this is a SKIP, so
    it can never masquerade as a passing parity assertion."""
    projects = [{"name": "fixture-lane-a", "cooldown_s": 43200, "cooldown_floor_s": 43200}]
    toml = """
[[projects]]
id = "fixture-lane-a"
cooldown_s = 43200
cooldown_floor_s = 43200
board_ownership = "whatever"
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml, ownership_column=False)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION parity" not in out, out


# ── the row's PASSING-FLEET fixture: the whole checker, zero violations ──

def _passing_fleet() -> list[dict]:
    primary = {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
               "namespace_id": "coding-hermes"}  # a real primary: coverage must pass on it
    rows = [primary]
    for suffix in FAMILIES:
        rows.append(_sat(f"fixture-primary-{suffix}", suffix))
    return rows


def test_passing_fleet_full_checker_exits_zero(tmp_path):
    """A full conforming fleet — namespaces at their caps and modes, a primary
    with all four satellites on family pins, real workdirs with board links,
    a fully agreeing fleet.toml — against the COMPLETE checker (all checks
    1-9 live, board absent) must exit 0 with zero violations."""
    projects = _passing_fleet()

    rc, out = _run_gate(tmp_path, projects)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "PASS — 0 violation(s)" in out, out


def test_passing_fleet_toml_drift_flips_it_red(tmp_path):
    """Negative control for the passing-fleet fixture: the SAME fleet with one
    stale toml pin must exit 1 — proving the green run above is the checker
    distinguishing, not the fixture being invisible to it."""
    projects = _passing_fleet()
    toml = """
[[projects]]
id = "fixture-primary-qa"
cooldown_s = 21600
cooldown_floor_s = 43200
"""
    rc, out = _run_gate(tmp_path, projects, toml_text=toml)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "parity") == [
        "VIOLATION parity fixture-primary-qa: cooldown_s: db=43200 toml=21600 — a pin in one store is drift"], out
