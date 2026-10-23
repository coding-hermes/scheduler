"""SCHED-GAP-1757 regression fixtures for the board-cleanup mass-close guard.

The incident these pin: a foreman cleanup closed 43 rows in bulk with
``status="stale"`` and the blanket reason "pending with zero acceptance
criteria" — a non-vocabulary status (invisible work, red CI) applied on a false
premise (42 of the 43 rows carried criteria). The guard in
``ops/board_cleanup_guard.py`` is the mechanical half of the doctrine "a gap
gets acceptance criteria, not a silent close".

Both arms are pinned, in-process and through the CLI:

  * REFUSED — a mass close that selects a row with no acceptance criteria, and
    a close written to a status outside the board vocabulary;
  * ALLOWED — a mass close where every selected row carries criteria, and a
    single-row close carrying completion evidence.

Run:  python3 -m pytest tests/test_board_cleanup_guard.py -v
"""
from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GUARD_PATH = REPO_ROOT / "ops" / "board_cleanup_guard.py"


def _load_guard():
    """Import ops/board_cleanup_guard.py by path (kept importable, not installable)."""
    spec = importlib.util.spec_from_file_location("board_cleanup_guard", GUARD_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GUARD_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


guard = _load_guard()


def _write_board(tmp_path: Path, rows: list[dict]) -> Path:
    """Write a line-wise JSONL board fixture."""
    tmp_path.mkdir(parents=True, exist_ok=True)
    board = tmp_path / "tasks.jsonl"
    board.write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")
    return board


def _run_cli(*args: str) -> subprocess.CompletedProcess:
    """Run the guard CLI exactly as a cleanup helper would."""
    return subprocess.run(
        [sys.executable, str(GUARD_PATH), *args],
        capture_output=True,
        text=True,
        check=False,
    )


# The 43-row incident, shrunk to its shape: criteria-bearing trackers mixed with
# the one row that carried no criteria (REL-GUARD-001 in the real board).
INCIDENT_ROWS = [
    {"id": "SEC-002", "status": "pending", "acceptance": "six credentials rotated"},
    {"id": "SCHED-GAP-162", "status": "pending", "acceptance": "build_sha == HEAD"},
    {
        "id": "REL-GUARD-001",
        "status": "pending",
        "title": "tag protection on v* refs — needs an owner ruling",
    },
]


def test_mass_close_of_criteria_less_rows_is_refused(tmp_path):
    """The incident replayed: a bulk stale-close is refused, never written."""
    board = _write_board(tmp_path, INCIDENT_ROWS)

    proc = _run_cli("--board", str(board), "--status", "stale")

    assert proc.returncode == 3, proc.stdout + proc.stderr
    assert "REFUSED" in proc.stdout
    # The illegal status is the first refusal a stale-close hits.
    assert "refused-status-not-in-vocabulary" in proc.stdout
    assert "pending, in_progress, complete, duplicate" in proc.stdout


def test_mass_close_legal_status_still_refuses_criteria_less_row(tmp_path):
    """Same mass close to a LEGAL status — the criteria rule alone refuses it."""
    board = _write_board(tmp_path, INCIDENT_ROWS)

    proc = _run_cli(
        "--board", str(board), "--status", "complete",
        "--ids", "SEC-002,SCHED-GAP-162,REL-GUARD-001",
    )

    assert proc.returncode == 3, proc.stdout + proc.stderr
    assert "refused-mass-close-of-criteria-less-rows" in proc.stdout
    assert "REL-GUARD-001" in proc.stdout
    # The criteria-bearing rows are not blamed for the refusal.
    assert "criteria-less: REL-GUARD-001" in proc.stdout


def test_mass_close_of_criteria_bearing_rows_is_allowed(tmp_path):
    """The positive arm: a bulk close where every row carries criteria proceeds."""
    board = _write_board(tmp_path, INCIDENT_ROWS)

    proc = _run_cli(
        "--board", str(board), "--status", "complete",
        "--ids-file", str(_ids_file(tmp_path, ["SEC-002", "SCHED-GAP-162"])),
    )

    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "GUARD: ALLOWED" in proc.stdout


def test_single_close_needs_criteria_or_completion_evidence(tmp_path):
    """A one-row close is allowed only when the row names what was done."""
    bare = _write_board(tmp_path / "bare", [{"id": "X-1", "status": "pending"}])
    proc = _run_cli("--board", str(bare), "--status", "complete", "--ids", "X-1")
    assert proc.returncode == 3, proc.stdout + proc.stderr
    assert "refused-single-close-without-criteria-or-evidence" in proc.stdout

    evidenced = _write_board(
        tmp_path / "evidenced",
        [{"id": "X-1", "status": "pending", "commit_hash": "abc1234",
          "worker_summary": "shipped the fix"}],
    )
    proc = _run_cli("--board", str(evidenced), "--status", "complete", "--ids", "X-1")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "GUARD: ALLOWED" in proc.stdout


def test_acceptance_criteria_list_counts_as_criteria(tmp_path):
    """Boards store criteria as a string OR a list; an empty list is not criteria."""
    rows = [
        {"id": "LIST-OK", "status": "pending",
         "acceptance_criteria": ["a test passes", "the route answers 200"]},
        {"id": "LIST-EMPTY", "status": "pending", "acceptance_criteria": []},
    ]
    allowed = guard.evaluate(rows, ids=["LIST-OK"], target_status="complete")
    assert allowed.allowed, allowed.code

    refused = guard.evaluate(rows, ids=["LIST-OK", "LIST-EMPTY"],
                             target_status="complete")
    assert refused.code == guard.REFUSED_MASS_CRITERIA_LESS
    assert refused.criteria_less == ["LIST-EMPTY"]
    assert refused.exit_code == 3


def test_guard_reads_a_real_board_without_touching_it(tmp_path):
    """End-to-end on a board file: verdict JSON, exit code, file byte-identical."""
    board = _write_board(tmp_path, INCIDENT_ROWS)
    before = board.read_bytes()

    proc = _run_cli("--board", str(board), "--status", "complete", "--json")

    assert proc.returncode == 3, proc.stdout + proc.stderr
    verdict = json.loads(proc.stdout)
    assert verdict["allowed"] is False
    assert verdict["criteria_less"] == ["REL-GUARD-001"]
    assert verdict["exit_code"] == 3
    assert board.read_bytes() == before, "the guard must be read-only"


def _ids_file(tmp_path: Path, ids: list[str]) -> Path:
    """Write an ``--ids-file`` fixture (one id per line)."""
    path = tmp_path / "ids.txt"
    path.write_text("\n".join(ids) + "\n", encoding="utf-8")
    return path
