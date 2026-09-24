"""SCHED-GAP-094 regression fixtures for check 10 (class ``sync-orientation``).

Drives ``ops/check-fleet-invariants.py`` in-process against a seeded fixture
fleet so the sync-lane orientation gate cannot degrade into a green no-op.
A ``*-sync`` lane's workdir is a SHELL, not a repo: the only place a
picker/dogfood agent (or the next tick's agent) can learn what the lane is,
which DuckBrain namespace it targets and how to consume it is the workdir's
README plus the companion ``<base>-sync-data`` skill. Read that contract from
the checker's numbered Checks list (entry 10) — it is the rule asserted here.

Arms:

  * seeded   — an enabled lane whose workdir carries no README at all exits 1
               and names the lane on ``VIOLATION sync-orientation <lane>:``;
               the same lane with a zero-byte README stub fires identically
               (a stub orients nobody);
  * conforming — README declaring the namespace + the companion data skill
               present exits 0 with no violation of this class;
  * exempt   — the SAME defective lane with ``enabled=0`` exits 0, exactly
               like check 5's exemption;
  * skip     — with the skills root absent (a CI runner, a test rig) a lane
               that only looked defective because of the missing skill is
               clean; and a lane whose WORKDIR is absent produces check 5's
               ``workdirs`` line but NOT a second ``sync-orientation`` line
               (one fact never earns two violations).

The independence of the three facts is pinned too: a README with prose about
"the namespace" but no target token fires, and a README that names its
namespace without a findable consumption contract fires — so repairing one
fact cannot silently satisfy the others.

Hermetic by construction, same style as the sibling modules: a temp SQLite DB,
temp workdirs and a temp skills root (``--skills-root``), so nothing here reads
or writes ``~/.hermes`` state.

Run:  python3 -m pytest tests/test_check_fleet_invariants_sync_orientation.py -v
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
SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
# The lane and its target project. The primary deliberately lives in a
# namespace OUTSIDE the coverage gate's scope (only `coding-hermes` primaries
# are covered), so this module tests check 10 and nothing else; the base IS a
# fleet project row, which is what keeps checks 5c/6 quiet without a ticks row.
PRIMARY = "fixture-orientation-target"
LANE = f"{PRIMARY}-sync"
# Every candidate path check 6 derives for a project-less base must be absent —
# the base here is a real row, so the premise is belt-and-braces.
for cand in (f"/home/kara/{PRIMARY}", str(Path.home() / ".hermes" / PRIMARY)):
    assert not Path(cand).exists(), f"fixture premise broken: {cand} exists on this machine"

# A README that satisfies both the namespace fact and the pointer fact.
CONFORMING_README = (
    f"# {LANE} — lane workdir\n\n"
    f"Scratch space for the sync lane. Target namespace: `{PRIMARY}`\n"
    "Consumption contract: skill `context-sync-duckbrain`; markers `/sync/last-run`.\n"
)


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _make_db(path: Path, rows: list[dict], workdirs: dict[str, Path]) -> Path:
    """Minimal fixture DB: exactly the columns the checker reads."""
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, adaptive_cooldown INTEGER, command TEXT, prompt TEXT,"
                " workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER,"
                " admission_mode TEXT)")
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))
    for r in rows:
        con.execute(
            "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, adaptive_cooldown,"
            " command, prompt, workdir, namespace_id) VALUES (?, ?, ?, ?, 0, ?, '', ?, ?)",
            (r["name"], r.get("enabled", 1), r.get("cooldown_s", 43200),
             r.get("cooldown_floor_s", 43200), r.get("command", SUPPORTED_COMMAND),
             str(workdirs[r["name"]]), r.get("namespace_id", "fixture-ns")))
    con.commit()
    con.close()
    return path


def _default_rows() -> list[dict]:
    return [{"name": PRIMARY, "namespace_id": "fixture-ns"},
            {"name": LANE, "namespace_id": "duckbrain-sync"}]


def _run_gate(tmp_path: Path, rows: list[dict], *,
              readme: str | None = None,
              skills: tuple[str, ...] = (),
              empty_skills: tuple[str, ...] = (),
              skills_root_absent: bool = False,
              lane_workdir_absent: bool = False) -> tuple[int, str]:
    """Seed *rows* with real workdirs and run the gate against a temp skills root.

    *readme* is written to the LANE's workdir when given; *skills* names the
    companion ``*-sync-data`` skill directories to create under the temp skills
    root. ``skills_root_absent=True`` points ``--skills-root`` at a path that
    does not exist (the CI shape); ``lane_workdir_absent=True`` leaves the
    lane's workdir off disk so check 5 owns the missing-workdir fact.
    """
    workdirs = {r["name"]: (tmp_path / "wd" / r["name"]) for r in rows}
    for name, wd in workdirs.items():
        if lane_workdir_absent and name == LANE:
            continue
        wd.mkdir(parents=True, exist_ok=True)
        # checks 5c/6 look for the board link on a satellite whose base is a row
        (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
    if readme is not None and not lane_workdir_absent:
        (workdirs[LANE] / "README.md").write_text(readme, encoding="utf-8")
    skills_root = tmp_path / "no-skills-root" if skills_root_absent else tmp_path / "skills"
    if not skills_root_absent:
        # The root itself always exists in the non-CI arm: an empty root means
        # "no companion skill on disk", not "the runner has no fleet state".
        skills_root.mkdir(parents=True, exist_ok=True)
        for skill in skills:
            d = skills_root / "data" / skill
            d.mkdir(parents=True, exist_ok=True)
            (d / "SKILL.md").write_text("---\nname: %s\n---\n" % skill, encoding="utf-8")
        for skill in empty_skills:
            # a directory with NO SKILL.md — not a contract
            (skills_root / "data" / skill).mkdir(parents=True, exist_ok=True)

    db = _make_db(tmp_path / "scheduler.db", rows, workdirs)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
            "--skills-root", str(skills_root),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── class registration / documentation ────────────────────────────────────

def test_class_is_registered_and_documented():
    """AC1: the class is in CHECK_CLASSES and in the numbered Checks list —
    a class in neither is an incomplete change."""
    assert "sync-orientation" in gate.CHECK_CLASSES
    assert gate.SYNC_ORIENTATION_CLASS == "sync-orientation"
    doc = gate.__doc__ or ""
    assert "sync-orientation" in doc, "check 10 missing from the docstring Checks list"
    assert "VIOLATION sync-orientation" in doc, "docstring carries no example violation line"
    # The documented numbers line up: check 10 exists, and --board-only's
    # description must not claim to run it.
    assert " 10. sync-orientation" in doc


def test_namespace_rule_rejects_bare_keyword_and_accepts_whole_token():
    """The namespace fact is mechanical: a `namespace` LINE carrying the base as
    a whole hyphen-delimited word; prose alone does not pass, and neither does
    the base buried inside a longer name."""
    assert gate.sync_readme_declares_namespace(CONFORMING_README, PRIMARY)
    # a line mentioning namespace but never naming the target
    assert not gate.sync_readme_declares_namespace(
        "This lane writes to a namespace, see the skill.\n", PRIMARY)
    # the base as a SUBSTRING of a longer hyphenated token is not a declaration
    assert not gate.sync_readme_declares_namespace(
        "Target namespace: `other-fixture-orientation-target-x`\n", PRIMARY)
    # ...nor is it one inside a longer alphanumeric run
    assert not gate.sync_readme_declares_namespace(
        "Target namespace: `fixture-orientation-targets`\n", PRIMARY)
    # ...nor is a SINGLE token lifted out of a multi-token base
    assert not gate.sync_readme_declares_namespace(
        "Target namespace: `go`\n", "h3-sdk-go-foreman-sync")
    # ...but a >=2-token hyphen run of the base is the live convention
    assert gate.sync_readme_declares_namespace(
        "Target namespace: `sdk-go` (the project's own memory).\n", "h3-sdk-go-foreman-sync")
    assert not gate.sync_readme_declares_namespace(
        "Target namespace: `sdk`\n", "h3-sdk-go-foreman-sync")


# ── seeded arm ────────────────────────────────────────────────────────────

def test_missing_readme_fires(tmp_path):
    rc, out = _run_gate(tmp_path, _default_rows(), skills=(f"{PRIMARY}-sync-data",))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1, got
    assert got[0].startswith(f"VIOLATION sync-orientation {LANE}: "), got
    assert "no README.md" in got[0], got
    # the fixture is clean on every other axis: any extra class is a fixture bug
    assert _other_violations(out, "sync-orientation") == [], f"unexpected extra violations:\n{out}"


def test_zero_byte_readme_stub_fires(tmp_path):
    """A stub is not an orientation — the same lane WITH a 0-byte README fires."""
    rc, out = _run_gate(tmp_path, _default_rows(), readme="",
                        skills=(f"{PRIMARY}-sync-data",))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1 and "no README.md" in got[0], got


def test_readme_without_namespace_line_fires(tmp_path):
    """Fact (b) alone: a README that points at a consumption contract but never
    names the target namespace does not say what the lane is FOR."""
    rc, out = _run_gate(tmp_path, _default_rows(),
                        readme=f"# {LANE}\n\nMarkers: `/sync/last-run`.\n",
                        skills=(f"{PRIMARY}-sync-data",))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1 and "names no target namespace" in got[0], got


def test_readme_with_namespace_but_no_contract_fires(tmp_path):
    """Fact (c) alone: the README names the namespace but no companion skill
    exists and the README carries no pointer to one."""
    rc, out = _run_gate(tmp_path, _default_rows(),
                        readme=f"# {LANE}\n\nTarget namespace: `{PRIMARY}`.\n")

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1 and "no consumption contract findable" in got[0], got


def test_both_defects_reported_in_one_violation(tmp_path):
    """One fact never earns two lines: a lane missing the README AND the skill
    yields a single violation whose detail carries both unmet facts."""
    rc, out = _run_gate(tmp_path, _default_rows())

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1, got
    assert "no README.md" in got[0] and "no consumption contract findable" in got[0], got


# ── conforming arm ────────────────────────────────────────────────────────

def test_readme_and_companion_skill_pass(tmp_path):
    rc, out = _run_gate(tmp_path, _default_rows(), readme=CONFORMING_README,
                        skills=(f"{PRIMARY}-sync-data",))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "PASS" in out, out


def test_readme_pointer_satisfies_the_contract_without_the_skill(tmp_path):
    """The contract is findable via the README alone (the row's documented OR):
    a skill pointer in the README passes even with no companion skill on disk."""
    rc, out = _run_gate(tmp_path, _default_rows(), readme=CONFORMING_README)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_skill_dir_without_skill_md_is_not_a_contract(tmp_path):
    """An empty `<base>-sync-data/` directory is not a contract; the README must
    then carry the pointer itself."""
    rc, out = _run_gate(tmp_path / "empty-skill", _default_rows(),
                        readme=f"# {LANE}\n\nTarget namespace: `{PRIMARY}`.\n",
                        empty_skills=(f"{PRIMARY}-sync-data",))
    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "no consumption contract findable" in _violations(out, "sync-orientation")[0], out

    # same README, a real SKILL.md → clean (proves the check reads the file, not
    # the directory name)
    rc2, out2 = _run_gate(tmp_path / "real-skill", _default_rows(),
                          readme=f"# {LANE}\n\nTarget namespace: `{PRIMARY}`.\n",
                          skills=(f"{PRIMARY}-sync-data",))
    assert rc2 == 0, f"gate exited {rc2}, expected 0 (stdout:\n{out2})"
    assert "VIOLATION" not in out2, out2


# ── exempt arm ────────────────────────────────────────────────────────────

def test_disabled_defective_lane_is_exempt(tmp_path):
    rows = _default_rows()
    rows[1]["enabled"] = 0

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── skip arm ──────────────────────────────────────────────────────────────

def test_absent_skills_root_skips_the_skill_axis(tmp_path):
    """CI shape: no skills root on the runner. A lane whose only defect was the
    missing companion skill must NOT be flagged — the gate may never require
    live fleet state to pass."""
    rc, out = _run_gate(tmp_path, _default_rows(), readme=CONFORMING_README,
                        skills_root_absent=True)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_absent_skills_root_still_catches_a_lane_with_no_readme(tmp_path):
    """The skip narrows the skill axis only: fact (a) is workdir-local and must
    still fire on a runner — otherwise the class would be vacuous in CI."""
    rc, out = _run_gate(tmp_path, _default_rows(), skills_root_absent=True)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = _violations(out, "sync-orientation")
    assert len(got) == 1 and "no README.md" in got[0], got
    assert "no consumption contract findable" not in got[0], got


def test_missing_workdir_does_not_double_report(tmp_path):
    """A lane with no workdir at all is check 5's finding; check 10 stays silent
    so one fact never produces two violation lines."""
    rc, out = _run_gate(tmp_path, _default_rows(), lane_workdir_absent=True)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "workdirs"), f"check 5 should own this fact:\n{out}"
    assert _violations(out, "sync-orientation") == [], out


def test_non_sync_lane_is_out_of_scope(tmp_path):
    """The class names only `*-sync` lanes; an identical defect on another
    satellite suffix is not this check's subject."""
    rows = [{"name": PRIMARY, "namespace_id": "fixture-ns"},
            {"name": f"{PRIMARY}-qa", "namespace_id": "qa"}]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
