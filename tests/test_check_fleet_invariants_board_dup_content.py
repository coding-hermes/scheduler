"""SCHED-GAP-172 regression fixtures for the board content-duplicate gate.

Drives ``ops/check-fleet-invariants.py`` check #9 (class
``board-content-dup``) in-process, so the gate cannot silently degrade into a
no-op pass:

  * two pending rows whose non-volatile content matches must exit 1 with both
    row ids printed on one ``VIOLATION board-content-dup`` line (the
    FND-001/FND-002 class the foreman swept by hand in tick #596);
  * a clean board, and a board whose only same-fingerprint rows are closed,
    must exit 0;
  * rows differing ONLY in volatile bookkeeping fields (``updated_at``,
    ``foreman_note``, …) still share a fingerprint and still fire.

The fixtures run with ``--board-only`` — the CI mode — so no live DB or
fleet.toml is touched, and the fixture boards are written to ``tmp_path``.

Run:  python3 -m pytest tests/test_check_fleet_invariants_board_dup_content.py -v
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


def _row(rid: str, status: str = "pending", **overrides) -> dict:
    """A minimal board row; keyword overrides patch any field."""
    row = {
        "id": rid,
        "title": "Board validator passes byte-identical duplicate rows",
        "reasoning": "identical body of the finding",
        "status": status,
        "priority": "P2",
        "labels": ["dedupe"],
        "updated_at": "2026-09-19T00:00:00Z",
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


def test_content_dup_fails_when_two_pending_rows_share_fingerprint(tmp_path):
    """Sad path: two open rows with identical content must exit 1, class named,
    both row ids printed on the single per-group violation line."""
    rows = [
        _row("DUP-1", foreman_note="filed by the pm cycle"),
        _row("DUP-2", foreman_note="re-filed from the daily report"),
    ]
    # Premise: the rows really do collide once volatile fields are ignored.
    assert gate.compute_content_fingerprint(rows[0]) == gate.compute_content_fingerprint(rows[1])

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-content-dup" in out, out
    assert "[DUP-1,DUP-2]" in out, out  # one line per offending group, all ids
    assert "FAIL" in out, out
    # The fixture is clean by construction, so the content-dup finding must be
    # the only one — any extra class means the fixture (not the gate) regressed.
    other = [l for l in out.splitlines()
             if l.startswith("VIOLATION ") and not l.startswith("VIOLATION board-content-dup")]
    assert other == [], f"unexpected non-content-dup violations: {other}"


def test_content_dup_passes_when_no_duplicate(tmp_path):
    """Happy path: distinct rows are not flagged and the gate exits 0."""
    rows = [
        _row("FND-1", title="First distinct finding",
             reasoning="body one"),
        _row("FND-2", title="Second distinct finding",
             reasoning="body two"),
        _row("SEC-1", title="Third distinct finding", status="complete",
             reasoning="body three"),
    ]
    # Premise: no two of the three rows share a fingerprint.
    fps = [gate.compute_content_fingerprint(r) for r in rows]
    assert len(set(fps)) == 3, fps

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "board-content-dup" not in out, out
    assert "PASS" in out, out


def test_content_dup_ignores_closed_rows(tmp_path):
    """Exemption: an all-closed fingerprint group is historical closure noise,
    not an open duplicate — the gate must stay silent and exit 0."""
    rows = [
        _row("OLD-1", status="complete"),
        _row("OLD-2", status="complete", foreman_note="closed on a later tick"),
    ]
    # Premise: the two closed rows really do share a fingerprint, so this test
    # proves the exemption rather than an accidental mismatch.
    assert gate.compute_content_fingerprint(rows[0]) == gate.compute_content_fingerprint(rows[1])

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "PASS" in out, out


def test_content_dup_ignores_volatile_field_diffs(tmp_path):
    """Two pending rows differing only in ``updated_at`` and ``foreman_note``
    are the same finding — both fields are volatile, so the fingerprint must
    match and the gate must still fire."""
    rows = [
        _row("DUP-A", updated_at="2026-09-18T10:00:00Z",
             foreman_note="note from yesterday's tick"),
        _row("DUP-B", updated_at="2026-09-19T19:00:00Z",
             foreman_note="note from today's tick"),
    ]
    # Premise: the rows differ ONLY in volatile fields, hence same fingerprint.
    assert gate.compute_content_fingerprint(rows[0]) == gate.compute_content_fingerprint(rows[1])

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert "VIOLATION board-content-dup" in out, out
    assert "[DUP-A,DUP-B]" in out, out


def test_content_dup_closed_twin_of_pending_row_does_not_collide(tmp_path):
    """Status is CONTENT, not volatile: a closed (complete) twin of a pending
    row has a DIFFERENT fingerprint, so the pair neither fires nor needs the
    exemption — the all-closed exemption (AC3) and the open-member trigger
    (AC2) both operate on same-status groups under the mandated fingerprint.
    Pinned so nobody 'fixes' the fingerprint into flagging closure twins."""
    rows = [
        _row("DUP-1", status="complete"),
        _row("DUP-2", status="pending"),
    ]
    # Premise: the status difference alone changes the fingerprint.
    assert gate.compute_content_fingerprint(rows[0]) != gate.compute_content_fingerprint(rows[1])

    rc, out = _run_gate(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_new_check_class_is_registered():
    """The daily report builds its summary from CHECK_CLASSES — the new class
    must be part of the tuple or the violation is counted nowhere."""
    assert "board-content-dup" in gate.CHECK_CLASSES, gate.CHECK_CLASSES
