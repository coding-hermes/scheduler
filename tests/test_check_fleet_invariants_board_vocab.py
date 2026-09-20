"""RELEASE-007 regression fixtures for the board vocabulary gate (check 8).

Drives ``ops/check-fleet-invariants.py`` check #8 (class ``board-vocab``)
in-process, pinning both halves of the vocabulary decision:

  * ``in_progress`` is a LEGITIMATE writer status — the live-lane claim the
    foreman wave writes when it dispatches a row — so a fixture carrying an
    ``in_progress`` row must exit 0 with no ``board-vocab`` violation;
  * genuinely PARKED spellings stay off-vocabulary: a ``todo`` row must exit 1
    with the row named on a ``VIOLATION board-vocab`` line.

The fixtures run with ``--board-only`` — the CI mode — so no live DB or
fleet.toml is touched, and the fixture boards are written to ``tmp_path``.

Run:  python3 -m pytest tests/test_check_fleet_invariants_board_vocab.py -v
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


def test_board_vocab_in_progress_is_allowed(tmp_path):
    """Positive pin (RELEASE-007): the foreman wave's dispatch marker must not
    fail the gate — one in_progress row exits 0 with no violation naming it."""
    rows = [
        '{"id": "TRBL-026", "status": "in_progress", "title": "live lane holds this"}',
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "TRBL-026" not in out, out
    assert "PASS" in out, out


def test_board_vocab_todo_still_fails(tmp_path):
    """Sad path: parked spellings stay off-vocabulary — one todo row exits 1
    with the row named on the board-vocab violation line."""
    rows = [
        '{"id": "TRBL-027", "status": "todo", "title": "parked by an old writer"}',
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-vocab TRBL-027: status=todo" in out, out
    assert "FAIL" in out, out


def test_board_vocab_mixed_fixture(tmp_path):
    """One in_progress row and one todo row on the SAME board: the gate must
    flag only the todo row and stay silent about the in_progress one."""
    rows = [
        '{"id": "FND-1", "status": "in_progress"}',
        '{"id": "FND-2", "status": "todo"}',
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-vocab FND-2: status=todo" in out, out
    violations = [l for l in out.splitlines() if l.startswith("VIOLATION ")]
    assert len(violations) == 1, f"expected exactly one violation, got: {violations}"
    assert "FND-1" not in out, out
