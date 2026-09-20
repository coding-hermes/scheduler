"""SCHED-GAP-147 regression fixtures for checks 5b/5c/5d (classes ``adaptive``,
``boards``, ``coverage``).

Drives ``ops/check-fleet-invariants.py`` in-process against a seeded fixture
DB with real temp workdirs, so all three classes are fixture-reachable:

  * adaptive — an ENABLED satellite-shaped lane (``<name>-qa`` etc.) with
    ``adaptive_cooldown`` armed fires; disarmed passes; the SAME name with
    adaptive armed but DISABLED is exempt (the regex sits inside the
    enabled-gate);
  * boards — an enabled satellite whose workdir LACKS the
    ``.coding-hermes/board`` link while its target project exists fires; the
    same layout WITH the link passes; a satellite whose target project row is
    absent is out of scope for 5c by design (that shape is check 6's);
  * coverage — a coding-hermes primary missing one satellite lane fires ONE
    violation per missing suffix; a full satellite set passes; a primary
    outside the ``coding-hermes`` namespace is not a coverage subject.

Run:  python3 -m pytest tests/test_check_fleet_invariants_adaptive_boards_coverage.py -v
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


def _make_db(path: Path, rows: list[dict], workdirs: dict[str, Path]) -> Path:
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, adaptive_cooldown INTEGER, command TEXT, prompt TEXT,"
                " workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, 1, 'cooldown')", (ns,))
    # The check-6 targets fallback queries ticks for recent activity when a
    # satellite's base is not a fleet project — give it an empty table so a
    # fixture satellite never crashes the gate on a missing table.
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    for r in rows:
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, adaptive_cooldown,"
            " command, prompt, workdir, namespace_id) VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)",
            (r["name"], r.get("enabled", 1), r.get("cooldown_s", 43200),
             r.get("cooldown_floor_s", 43200), r.get("adaptive_cooldown", 0),
             r.get("command", SUPPORTED_COMMAND), str(workdirs[r["name"]]),
             r.get("namespace_id", "coding-hermes")))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, rows: list[dict],
              with_board_for: tuple[str, ...] = ()) -> tuple[int, str]:
    """Seed *rows*; every named workdir exists; only names in *with_board_for*
    get the ``.coding-hermes/board`` link the 5c boards check looks for."""
    workdirs = {r["name"]: (tmp_path / r["name"]) for r in rows}
    for wd in workdirs.values():
        wd.mkdir(exist_ok=True)
    for name in with_board_for:
        _board_dir(tmp_path, name)
    db = _make_db(tmp_path / "scheduler.db", rows, workdirs)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _primary_with_full_satellites(primary: str, **sat_overrides) -> list[dict]:
    """A primary plus its four satellites, every satellite on its family pin,
    adaptive disarmed — the coverage-clean shape."""
    rows = [{"name": primary, "cooldown_s": 43200, "cooldown_floor_s": 43200}]
    for suffix in FAMILIES:
        pin = gate.SATELLITE_FAMILY_PINS[suffix]
        row = {"name": f"{primary}-{suffix}", "cooldown_s": pin, "cooldown_floor_s": pin,
               "namespace_id": NS_FOR_SUFFIX[suffix]}
        row.update(sat_overrides)
        rows.append(row)
    return rows


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── adaptive ──────────────────────────────────────────────────────────────

def test_adaptive_armed_satellite_fires(tmp_path):
    rc, out = _run_gate(
        tmp_path,
        [{"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
          "namespace_id": "fixture-ns"},  # ns outside coverage scope: this module tests 5b/5c
         {"name": "fixture-primary-qa", "cooldown_s": 43200, "cooldown_floor_s": 43200,
          "adaptive_cooldown": 1, "namespace_id": "qa"}],
        with_board_for=("fixture-primary-qa",),
    )

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "adaptive") == [
        "VIOLATION adaptive fixture-primary-qa: satellite lane is adaptive-armed — "
        "satellites must never arm adaptive cooldown"], out
    assert _other_violations(out, "adaptive") == [], f"unexpected extra violations:\n{out}"


def test_adaptive_armed_but_disabled_lane_is_exempt(tmp_path):
    rc, out = _run_gate(
        tmp_path,
        [{"name": "fixture-primary-qa", "cooldown_s": 43200, "cooldown_floor_s": 43200,
          "adaptive_cooldown": 1, "namespace_id": "qa", "enabled": 0}],
        with_board_for=("fixture-primary-qa",),
    )

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_adaptive_disarmed_satellite_passes(tmp_path):
    rc, out = _run_gate(
        tmp_path,
        [{"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
          "namespace_id": "fixture-ns"},
         {"name": "fixture-primary-qa", "cooldown_s": 43200, "cooldown_floor_s": 43200,
          "adaptive_cooldown": 0, "namespace_id": "qa"}],
        with_board_for=("fixture-primary-qa",),
    )

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── boards (5c) ───────────────────────────────────────────────────────────

def test_boards_missing_link_fires(tmp_path):
    """Satellite workdir without the board link while its target project
    exists → the 5c boards violation names the satellite."""
    rows = [
        {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
         "namespace_id": "fixture-ns"},  # ns outside coverage scope: this module tests 5c
        {"name": "fixture-primary-qa", "cooldown_s": 43200, "cooldown_floor_s": 43200,
         "namespace_id": "qa"},
    ]
    # NB: no with_board_for — neither workdir has the board link, but only the
    # SATELLITE shape is reported (the primary is not a 5c subject).

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "boards") == [
        "VIOLATION boards fixture-primary-qa: board does not resolve in "
        f"'{tmp_path / 'fixture-primary-qa'}' while its target 'fixture-primary' exists"], out
    assert _other_violations(out, "boards") == [], f"unexpected extra violations:\n{out}"


def test_boards_link_present_passes(tmp_path):
    rows = [
        {"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
         "namespace_id": "fixture-ns"},
        {"name": "fixture-primary-qa", "cooldown_s": 43200, "cooldown_floor_s": 43200,
         "namespace_id": "qa"},
    ]

    rc, out = _run_gate(tmp_path, rows, with_board_for=("fixture-primary-qa",))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── coverage (5d) ─────────────────────────────────────────────────────────

def test_coverage_missing_satellites_fire_per_suffix(tmp_path):
    rows = _primary_with_full_satellites("fixture-primary")
    rows = [r for r in rows if r["name"] not in ("fixture-primary-qa", "fixture-primary-sync")]

    rc, out = _run_gate(tmp_path, rows, with_board_for=tuple(r["name"] for r in rows))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "coverage")
    assert len(got) == 2, got
    assert "VIOLATION coverage fixture-primary-qa: missing or disabled" in got[0], got
    assert "VIOLATION coverage fixture-primary-sync: missing or disabled" in got[1], got
    assert _other_violations(out, "coverage") == [], f"unexpected extra violations:\n{out}"


def test_coverage_full_satellite_set_passes(tmp_path):
    rows = _primary_with_full_satellites("fixture-primary")

    rc, out = _run_gate(tmp_path, rows, with_board_for=tuple(r["name"] for r in rows))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_coverage_disabled_satellite_counts_as_missing(tmp_path):
    rows = _primary_with_full_satellites("fixture-primary")
    for r in rows:
        if r["name"] == "fixture-primary-dogfood":
            r["enabled"] = 0

    rc, out = _run_gate(tmp_path, rows, with_board_for=tuple(r["name"] for r in rows))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "coverage")
    assert got == ["VIOLATION coverage fixture-primary-dogfood: missing or disabled — every "
                   "coding-hermes primary needs an enabled dogfood lane (primary 'fixture-primary' "
                   "lives in the 'coding-hermes' namespace)"], got
    assert _other_violations(out, "coverage") == [], f"unexpected extra violations:\n{out}"


def test_coverage_non_foreman_namespace_primary_not_a_subject(tmp_path):
    """A primary whose namespace_id is not ``coding-hermes`` is outside the
    coverage gate's scope (the checker's primary discriminator)."""
    rows = [{"name": "fixture-primary", "cooldown_s": 43200, "cooldown_floor_s": 43200,
             "namespace_id": "some-other-ns"}]

    rc, out = _run_gate(tmp_path, rows, with_board_for=("fixture-primary",))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
