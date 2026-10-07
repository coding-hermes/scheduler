"""SCHED-GAP-1604 regression fixtures for the namespace-membership check
(class ``namespace-membership``, check 2b in ops/check-fleet-invariants.py).

Drives the gate in-process against a seeded fixture DB so the family
namespace convention cannot degrade into a green no-op:

  * a satellite lane named ``<base>-<role>`` must sit in its ROLE's shared
    family namespace (qa/pm/dogfood/duckbrain-sync/releases/review/docs/perf);
    a private per-project namespace (the measured auger outlier — five
    satellites parked in a private ``auger`` namespace) is a violation;
  * a primary lane (no role suffix) is NOT policed — the family convention
    governs satellites only;
  * the gate's own live-fleet census (2026-10-06) must stay clean — a
    baseline drift means the fleet regressed, not the fixture;
  * the namespace map and the role-suffix pin table cannot drift apart
    silently (every role in SATELLITE_FAMILY_PINS is policeable).

Both arms are proven: a seeded violation exits 1 naming the exact lane, a
conforming fleet exits 0.

Run:  python3 -m pytest tests/test_check_fleet_invariants_namespace_membership.py -v
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
SATELLITE_CAP_POLICY = {
    "qa": 9,
    "pm": 9,
    "dogfood": 9,
    "releases": 9,
    "duckbrain-sync": 12,
    "doc-writer": 1,
}


SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"


def _make_workdir_with_board(root: Path, name: str) -> Path:
    """Create ``root/<name>/.coding-hermes/board/`` and return the workdir.

    The 5c ``boards`` check reports a satellite whose workdir has no board
    link as a violation, so the fixture must plant an empty board dir. A
    ``*-sync`` lane additionally needs the check-10 orientation facts (a
    README naming its namespace + a findable consumption contract), so this
    helper plants them — otherwise sibling classes, not the class under
    test, turn the fixture red.
    """
    wd = root / name
    (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
    if name.endswith("-sync"):
        base = name[:-len("-sync")]
        (wd / "README.md").write_text(
            f"# {name} — lane workdir\n\nTarget namespace: `{base}`.\n"
            f"Contract: skill `{base}-sync-data`.\n", encoding="utf-8")
    return wd


def _seed_db(path: Path, projects: list[tuple[str, str, int]]) -> Path:
    """Mint a scheduler DB with the given (name, namespace_id, enabled) rows.

    Namespaces are seeded conforming (foreman 8/tasks; satellites at the
    SCHED-GAP-215 policy caps on cooldown) so checks 1-2 are clean; each
    project gets a conforming workdir (board dir + sync orientation facts)
    and a `ticks` table exists (empty) so the sibling checks that read it
    stay quiet — any extra VIOLATION line means the fixture (not the gate)
    regressed.
    """
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, SATELLITE_CAP_POLICY[ns]))
    for name, ns, enabled in projects:
        wd = _make_workdir_with_board(path.parent, name)
        role = name.rsplit("-", 1)[-1] if name.rsplit("-", 1)[-1] in gate.SATELLITE_FAMILY_PINS else None
        cd = gate.SATELLITE_FAMILY_PINS.get(role, 21600) if role else 43200
        con.execute("INSERT INTO projects (name, namespace_id, enabled, cooldown_s, command, prompt, workdir)"
                    " VALUES (?, ?, ?, ?, ?, '', ?)",
                    (name, ns, enabled, cd, SUPPORTED_COMMAND, str(wd)))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, projects: list[tuple[str, str, int]]) -> tuple[int, str]:
    db = _seed_db(tmp_path / "scheduler.db", projects)
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


def _subject(l: str) -> str:
    """The subject token of a ``VIOLATION <class> <subject>:`` line."""
    return l.split()[2].rstrip(":")


# ── the measured auger shape is a violation ────────────────────────────────

def test_auger_outlier_shape_is_flagged(tmp_path):
    """The live shape the board row measured: five auger satellites parked in
    a private `auger` namespace. Each must be named."""
    rc, out = _run_gate(tmp_path, [
        ("auger", "auger", 1),          # a primary is NOT policed...
        ("auger-qa", "auger", 1),       # ...but every satellite is
        ("auger-pm", "auger", 1),
        ("auger-dogfood", "auger", 1),
        ("auger-sync", "auger", 1),
        ("auger-releng", "auger", 1),
    ])
    assert rc == 1
    v = _violations(out, "namespace-membership")
    flagged = {_subject(l) for l in v}
    assert flagged == {"auger-qa", "auger-pm", "auger-dogfood", "auger-sync", "auger-releng"}
    assert not _other_violations(out, "namespace-membership")


def test_primary_lane_in_private_namespace_is_not_policed(tmp_path):
    """The convention governs SATELLITE lanes; a primary may sit anywhere its
    operator points it (auger sat in a private namespace pre-1604 — a primary
    in the coding-hermes namespace WITH satellites is coverage's business,
    not membership's, so this fixture keeps its primary OUT of coding-hermes
    and carries no satellites at all)."""
    rc, out = _run_gate(tmp_path, [
        ("auger", "auger", 1),
        ("lonetree", "backup", 1),
    ])
    assert rc == 0, out
    assert _violations(out, "namespace-membership") == []


def test_conforming_family_passes(tmp_path):
    """Every satellite in its role's shared family namespace → exit 0, no
    membership violations (mirrors the normalized live fleet)."""
    conforming = [
        ("9router", "coding-hermes", 1),
        ("9router-qa", "qa", 1),
        ("9router-pm", "pm", 1),
        ("9router-sync", "duckbrain-sync", 1),
        ("9router-dogfood", "dogfood", 1),
        ("9router-releng", "releases", 1),
        ("9router-review", "review", 1),
        ("9router-docs", "docs", 1),
        ("9router-readme", "docs", 1),
        ("9router-perf", "perf", 1),
    ]
    rc, out = _run_gate(tmp_path, conforming)
    assert rc == 0, out
    assert _violations(out, "namespace-membership") == []
    assert not _other_violations(out)


def test_auger_normalized_shape_passes(tmp_path):
    """The FIXED live fleet: auger's satellites in the family namespaces."""
    rc, out = _run_gate(tmp_path, [
        ("auger", "coding-hermes", 1),
        ("auger-qa", "qa", 1),
        ("auger-pm", "pm", 1),
        ("auger-sync", "duckbrain-sync", 1),
        ("auger-dogfood", "dogfood", 1),
        ("auger-releng", "releases", 1),
    ])
    assert rc == 0, out
    assert _violations(out, "namespace-membership") == []


def test_null_namespace_is_a_violation(tmp_path):
    """namespace_id NULL (or empty) on a satellite lane is unassigned — the
    same defect one resume away from being live. Not silently skipped."""
    rc, out = _run_gate(tmp_path, [("solo-qa", None, 1)])
    assert rc == 1
    v = _violations(out, "namespace-membership")
    assert len(v) == 1 and "solo-qa" in v[0]


def test_disabled_misplaced_satellite_is_still_flagged(tmp_path):
    """A mis-placed DISABLED lane is the same defect one resume away — the
    check is deliberately independent of `enabled` (mirrors the workdir gate)."""
    rc, out = _run_gate(tmp_path, [("off-qa", "qa", 0), ("bad-pm", "off", 0)])
    v = _violations(out, "namespace-membership")
    assert rc == 1
    assert {_subject(l) for l in v} == {"bad-pm"}


# ── live-fleet census: the baseline must stay clean ────────────────────────

def test_live_fleet_namespace_membership_is_clean():
    """The check against the REAL scheduler.db (when it exists) must find no
    membership violations — the auger normalization landed 2026-10-06 and a
    violation here means the fleet drifted back, not that the gate broke.
    Skips silently on a CI runner (no live DB)."""
    db = Path.home() / ".hermes/coding-hermes/scheduler.db"
    if not db.exists():
        import pytest
        pytest.skip("no live scheduler.db on this host")
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    projects = {r["name"]: dict(r) for r in con.execute("SELECT name, namespace_id FROM projects")}
    con.close()
    import re
    ns_map = gate.SATELLITE_FAMILY_NAMESPACES
    matcher = re.compile(
        r"^(.+)-(" + "|".join(re.escape(s) for s in ns_map) + r")$")
    drift = []
    for name, p in projects.items():
        m = matcher.match(name)
        if m and (p.get("namespace_id") or "") != ns_map[m.group(2)]:
            drift.append(name)
    assert drift == [], f"live fleet namespace-membership drift: {drift}"


# ── derivability: the namespace map cannot go unpoliceable ────────────────

def test_namespace_map_covers_every_pinned_family():
    """Every role in SATELLITE_FAMILY_PINS (the cadence matrix) also has a
    namespace assignment — adding a family to one table and not the other
    would re-create the SCHED-GAP-1675 blind spot for membership."""
    assert set(gate.SATELLITE_FAMILY_PINS) <= set(gate.SATELLITE_FAMILY_NAMESPACES)


def test_membership_class_is_registered():
    """The class is in CHECK_CLASSES so the JSON summary counts it and the
    runbook's class vocabulary stays complete."""
    assert gate.NAMESPACE_MEMBERSHIP_CLASS in gate.CHECK_CLASSES
