"""SCHED-GAP-147 regression fixture for the board legacy-status class
(``board-legacy-status``, the second half of check 8).

Rows carrying a LEGACY closed spelling (``done`` / ``completed`` / ``closed``)
are reported under their own class so the PM cycle can sweep them in the same
tick — distinct from ``board-vocab`` (parked spellings like ``todo``, which
nobody dispatches AND nobody sweeps). Both arms proven: a ``done`` row exits 1
naming the row and the required replacement spelling; a row already on
``complete`` passes.

Run:  python3 -m pytest tests/test_check_fleet_invariants_board_legacy_status.py -v
"""
from __future__ import annotations

import importlib.util
import io
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


def _run_gate(tmp_path: Path, lines: list[str]) -> tuple[int, str]:
    """Write *lines* as a JSONL board and run the gate in --board-only mode."""
    board = tmp_path / "tasks.jsonl"
    with open(board, "w", encoding="utf-8") as fh:
        for line in lines:
            fh.write(line + "\n")
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "no-scheduler.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(board),
            "--board-only",
        ])
    return rc, buf.getvalue()


def test_legacy_status_constants_are_the_documented_set():
    """The legacy-closed set and its required replacement stay pinned."""
    assert gate.BOARD_LEGACY_CLOSED_STATUSES == ("done", "completed", "closed")
    assert "complete" in gate.BOARD_ALLOWED_STATUSES


def test_done_row_fires_board_legacy_status(tmp_path):
    """A done row exits 1 on the board-legacy-status class (NOT board-vocab)
    and names 'complete' as the required spelling."""
    rc, out = _run_gate(tmp_path, ['{"id": "OLD-9", "status": "done", "title": "closed the old way"}'])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-legacy-status OLD-9: status=done (use 'complete')" in out, out
    assert "VIOLATION board-vocab OLD-9" not in out, out


def test_completed_row_fires_too(tmp_path):
    rc, out = _run_gate(tmp_path, ['{"id": "OLD-10", "status": "completed"}'])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-legacy-status OLD-10: status=completed (use 'complete')" in out, out


def test_complete_row_passes(tmp_path):
    """The conforming arm: the current vocabulary's closed spelling is clean."""
    rc, out = _run_gate(tmp_path, ['{"id": "NEW-1", "status": "complete", "title": "closed"}'])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
