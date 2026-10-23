"""SCHED-GAP-128 regression fixtures for check 13 (class ``fleet-infra-dup``).

The gate must be provably incapable of lying, so both arms are pinned:

  * a project board carrying an OPEN copy of a pattern the shared-infra board
    already carries exits 1 and names the offending row id and the carrier;
  * the same fleet with only project-local rows (a repo's own stale binary, a
    closed copy) exits 0 — the classifier's discriminator is what the gate
    leans on, and a gate that demanded those rows be deleted would destroy
    real project work;
  * with no flags in ``--board-only`` (CI) mode the check SKIPS with a
    documented INFO instead of failing for absent fleet boards.

Fixtures are written to ``tmp_path``; no live DB, TOML or board is touched
(the whole run is ``--board-only`` plus explicit fixture paths).

Run:  python3 -m pytest tests/test_check_fleet_invariants_fleet_infra_dup.py -v
"""
from __future__ import annotations

import importlib.util
import io
import json
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"


def _load_gate():
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()

CARRIER = {
    "id": "FLEET-INFRA-1", "status": "pending", "priority": "P1",
    "origin": "fleet-infra-collapse",
    "classification": "fleet-infra:port_pool",
    "title": "[fleet-infra:port_pool] consolidated from 3 project board row(s)",
}
DUPLICATE = {
    "id": "QA-CONSENSUS-1-R8", "status": "pending", "priority": "P1",
    "title": "[P1] run_battery FAIL — bunkerd port-pool exhaustion on bunker-las-03",
}
LOCAL_STALE = {
    "id": "GAP-056", "status": "pending", "priority": "P1",
    "title": "Deployed binaries stale vs HEAD: repo ./dagger and ~/.local/bin/dagger",
}
CLOSED_DUP = dict(DUPLICATE, id="QA-OLD-1", status="complete")


def _board(path: Path, rows: list[dict]) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    return str(path)


def _run(args: list[str]) -> tuple[int, dict]:
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main(args)
    return rc, json.loads(buf.getvalue())


def _fixture(tmp_path: Path, project_rows: list[dict]):
    infra = _board(tmp_path / "infra" / "tasks.jsonl", [CARRIER])
    project = _board(tmp_path / "consensus" / "tasks.jsonl", project_rows)
    scheduler = _board(tmp_path / "sched" / "tasks.jsonl",
                       [{"id": "SCHED-1", "status": "pending", "title": "scheduler work"}])
    return infra, project, scheduler


def _args(infra, project, scheduler):
    return ["--board", scheduler, "--board-only", "--json",
            "--infra-board", infra, "--fleet-boards", project]


def test_open_duplicate_fires_and_names_both_rows(tmp_path):
    infra, project, scheduler = _fixture(tmp_path, [DUPLICATE, LOCAL_STALE])
    rc, result = _run(_args(infra, project, scheduler))
    assert rc == 1
    hits = [v for v in result["violations"] if v["class"] == "fleet-infra-dup"]
    assert len(hits) == 1
    assert "QA-CONSENSUS-1-R8" in hits[0]["subject"]
    assert "FLEET-INFRA-1" in hits[0]["detail"]
    assert "fleet-infra:port_pool" in hits[0]["detail"]
    # The project-local stale-binary row is NOT swept in.
    assert all("GAP-056" not in v["subject"] for v in result["violations"])


def test_clean_fleet_passes(tmp_path):
    infra, project, scheduler = _fixture(tmp_path, [LOCAL_STALE, CLOSED_DUP])
    rc, result = _run(_args(infra, project, scheduler))
    assert rc == 0
    assert [v for v in result["violations"] if v["class"] == "fleet-infra-dup"] == []
    info = [i for i in result["info"] if i["class"] == "fleet-infra-dup"]
    assert info and "carrier pattern(s) open" in info[0]["detail"]


def test_duplicate_of_a_pattern_with_no_carrier_does_not_fire(tmp_path):
    # The infra board carries only port_pool; the project reports stale_binary.
    infra, project, scheduler = _fixture(tmp_path, [
        dict(DUPLICATE, id="QA-X-1", title="Fix not deployed — bunker-las-02 binary predates QA-BUNKER-4"),
    ])
    rc, result = _run(_args(infra, project, scheduler))
    assert rc == 0


def test_board_only_without_flags_skips_with_a_documented_info(tmp_path):
    _, _, scheduler = _fixture(tmp_path, [DUPLICATE])
    rc, result = _run(["--board", scheduler, "--board-only", "--json"])
    assert rc == 0
    info = [i for i in result["info"] if i["class"] == "fleet-infra-dup"]
    assert info and info[0]["subject"] == "skip"
    assert "CI has no fleet boards" in info[0]["detail"]


def test_class_is_registered_in_the_check_table():
    assert "fleet-infra-dup" in gate.CHECK_CLASSES
    rc, result = _run(["--board", "/nonexistent.jsonl", "--board-only", "--json"])
    assert rc == 0
    assert any(c["class"] == "fleet-infra-dup" for c in result["checks"])
