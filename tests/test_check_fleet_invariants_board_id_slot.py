"""SCHED-GAP-207 regression fixtures for the board id-slot gate (check 9b).

Drives ``ops/check-fleet-invariants.py`` check #9b (class ``board-id-slot``)
in-process, so the 29%-of-open-rows id-collision class cannot silently
regrow behind a gate that never looks at ids:

  * an id holding TWO open rows (the QA-CONSENSUS-1 slot shape) must exit 1
    with the id named on one ``VIOLATION board-id-slot`` line;
  * the same id re-filed AFTER the previous row was closed must exit 0 —
    the legitimate recurring cycle (close-then-refile);
  * ids that appear multiple times but hold at most ONE open row are
    history, not slots (exit 0);
  * distinct ids sharing similar content stay out of the id-slot class
    (that is check 9's job, not this one).

The fixtures run with ``--board-only`` — the CI mode — so no live DB or
fleet.toml is touched, and the fixture boards are written to ``tmp_path``.

Run:  python3 -m pytest tests/test_check_fleet_invariants_board_id_slot.py -v
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
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _row(rid: str, status: str = "pending", title: str = "a finding", **overrides) -> dict:
    row = {
        "id": rid,
        "title": title,
        "status": status,
        "priority": "P2",
        "updated_at": "2026-09-22T00:00:00Z",
        "foreman_note": None,
    }
    row.update(overrides)
    return row


def _run_gate(tmp_path: Path, rows: list[dict]) -> tuple[int, str]:
    """Write *rows* as a JSONL board and run the gate in --board-only mode."""
    board = tmp_path / "tasks.jsonl"
    with open(board, "w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row) + "\n")
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "no-scheduler.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(board),
            "--board-only",
        ])
    return rc, buf.getvalue()


def test_id_slot_fires_when_two_open_rows_share_one_id(tmp_path):
    """Sad path (the measured class): the same id filed twice while the
    previous copy is still open — a recurring SLOT — must exit 1, class named,
    the id and the open count on the violation line."""
    rows = [
        _row("QA-CONSENSUS-1", title="port pool exhausted [cycle 1]"),
        _row("QA-CONSENSUS-1", title="port pool exhausted [cycle 2]",
             updated_at="2026-09-21T00:00:00Z"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-id-slot QA-CONSENSUS-1" in out, out
    assert "2 OPEN rows share this id" in out, out
    assert "FAIL" in out, out
    # The fixture has two DIFFERENT titles, so exact-content dedupe (check 9)
    # must NOT fire — this is exactly the class that slipped past every
    # existing dedupe tool (each cycle re-words the same condition).
    other = [l for l in out.splitlines()
             if l.startswith("VIOLATION ") and not l.startswith("VIOLATION board-id-slot")]
    assert other == [], f"unexpected non-id-slot violations: {other}"


def test_id_slot_allows_refile_after_previous_row_closed(tmp_path):
    """Happy path (the legitimate cycle): the same id re-filed once the
    previous row is complete — one open row under an id with closed history —
    must exit 0. This is the closure protocol working as designed."""
    rows = [
        _row("QA-CONSENSUS-1", status="complete",
             title="port pool exhausted [cycle 1]"),
        _row("QA-CONSENSUS-1", title="port pool exhausted [cycle 2]"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "board-id-slot" not in out, out
    assert "PASS" in out, out


def test_id_slot_ignores_closed_history_under_one_id(tmp_path):
    """Three closed generations under one id with no open row at all:
    history, not a slot — the gate must stay silent and exit 0."""
    rows = [
        _row("DF-CRIER-1", status="complete"),
        _row("DF-CRIER-1", status="complete"),
        _row("DF-CRIER-1", status="complete"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "board-id-slot" not in out, out


def test_id_slot_fires_three_open_rows_under_one_id(tmp_path):
    """The QA-HIVEMIND-WORK-1 shape: 39 open rows under one id. Three here is
    enough to prove the count scales past the pair and reports the real N."""
    rows = [
        _row("QA-HIVEMIND-WORK-1", title="worker lane stuck [cycle 1]"),
        _row("QA-HIVEMIND-WORK-1", title="worker lane stuck [cycle 2]",
             updated_at="2026-09-21T00:00:00Z"),
        _row("QA-HIVEMIND-WORK-1", title="worker lane stuck [cycle 3]",
             updated_at="2026-09-20T00:00:00Z"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-id-slot QA-HIVEMIND-WORK-1" in out, out
    assert "3 OPEN rows share this id" in out, out


def test_id_slot_distinct_ids_do_not_collide(tmp_path):
    """Distinct ids are findings even when titles are similar — the id-slot
    class fires only on id reuse, never on content similarity (check 9's)."""
    rows = [
        _row("QA-ALPHA-1", title="port pool exhausted"),
        _row("QA-ALPHA-2", title="port pool exhausted (re-worded)"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "board-id-slot" not in out, out


def test_id_slot_offvocabulary_status_counts_as_open(tmp_path):
    """A row whose status is off-vocabulary ('todo') is open to every reader
    (board_freshness.go openStatuses) — two rows under one id, one pending and
    one todo, are two OPEN rows sharing a slot and must fire."""
    rows = [
        _row("PM-007", status="pending"),
        _row("PM-007", status="todo"),
    ]

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-id-slot PM-007" in out, out


def test_new_check_class_is_registered():
    """The daily report builds its summary from CHECK_CLASSES — the new class
    must be part of the tuple or the violation is counted nowhere."""
    assert "board-id-slot" in gate.CHECK_CLASSES, gate.CHECK_CLASSES