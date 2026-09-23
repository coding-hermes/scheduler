"""SCHED-GAP-206 regression fixtures for the event-id gate (check 11).

Drives ``ops/check-fleet-invariants.py`` check #11 (class
``event-id-ascending``) in-process, so the defect that forced a LIVE board gate
failure — a writer minting an event id at epoch-microsecond scale (16 digits)
instead of the board's 19-digit epoch-nanosecond scale, planting a line that
sorts below the file's existing ids — fails a test here instead of
``boardctl validate`` on a live board:

  * a clean ascending 19-digit log exits 0 and reports the INFO meter;
  * the reported defect's own shape (a sub-10^18 id appended after 19-digit
    ids) exits 1 with the LINE NUMBER and the BAD ID named;
  * a 19-digit id that descends below the file's max exits 1;
  * a log with NO 19-digit id at all is entirely off-scale (every int id
    reported);
  * the live log's pre-scale prefix and its duplicate ids are TOLERATED — this
    board carries 979 pre-scale ids and 2 duplicates, and the append-only log
    cannot be renumbered, so gating them would paint CI red for history;
  * legacy shapes (no id, null, string, bool, malformed JSON) are skipped and a
    missing events file skips the class silently (the CI/test-rig shape).

The fixtures run with ``--board-only`` — the CI mode — so no live DB or
fleet.toml is touched, and the fixture boards are written to ``tmp_path``.

Run:  python3 -m pytest tests/test_check_fleet_invariants_event_ids.py -v
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

BIG = 1789977500000000031       # the live log's era base (19 digits)
MICRO = 1790080373408208        # the reported off-scale id (16 digits, epoch µs)


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _make_board(tmp_path: Path, events_lines: list[str] | None) -> Path:
    """Write a minimal tasks.jsonl (+ optional events.jsonl) and return the board."""
    board = tmp_path / "tasks.jsonl"
    board.write_text(json.dumps({"id": "FIX-1", "status": "pending",
                                 "title": "fixture row"}) + "\n", encoding="utf-8")
    if events_lines is not None:
        (tmp_path / "events.jsonl").write_text(
            "".join(line + "\n" for line in events_lines), encoding="utf-8")
    return board


def _run_gate(tmp_path: Path, events_lines: list[str] | None,
              extra: list[str] | None = None) -> tuple[int, str]:
    """Run the gate in --board-only mode against a fixture board + events log."""
    board = _make_board(tmp_path, events_lines)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "no-scheduler.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(board),
            "--board-only",
        ] + list(extra or []))
    return rc, buf.getvalue()


def _violations(out: str, cls: str = gate.EVENT_ID_CLASS) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str) -> list[str]:
    prefix = f"VIOLATION {gate.EVENT_ID_CLASS} "
    return [l for l in out.splitlines() if l.startswith("VIOLATION ") and not l.startswith(prefix)]


def _event(*lines: str) -> list[str]:
    return list(lines)


# ── happy path ────────────────────────────────────────────────────────────

def test_clean_ascending_log_exits_zero(tmp_path):
    """A log of ascending 19-digit ids (the shape every writer must mint) is
    silent: exit 0, no violation, and the INFO meter reports what it scanned."""
    rc, out = _run_gate(tmp_path, _event(
        f'{{"id": {BIG}, "event_type": "task_created"}}',
        f'{{"id": {BIG + 1}, "event_type": "audit"}}',
        f'{{"id": {BIG + 900}, "event_type": "audit"}}',
    ))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], out
    assert f"INFO {gate.EVENT_ID_CLASS}" in out, out
    assert "3 int id(s) scanned" in out, out
    assert "PASS" in out, out


# ── sad paths (the reported defect) ───────────────────────────────────────

def test_microsecond_scale_line_after_19_digit_ids_fires(tmp_path):
    """THE reported defect: two lines written at epoch-MICROSECOND scale while
    the file sat at 1789977500000000031. Both must be named by line number and
    id — this is the exact shape that FAILed ``boardctl validate`` live."""
    rc, out = _run_gate(tmp_path, _event(
        f'{{"id": {BIG}, "event_type": "task_created"}}',
        f'{{"id": {MICRO}, "event_type": "audit"}}',
        '{"id": 1790080400000000, "event_type": "task_completed"}',
    ))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out)
    assert len(lines) == 2, out
    assert lines[0] == (f"VIOLATION {gate.EVENT_ID_CLASS} 2: id={MICRO} is below the "
                        f"19-digit floor {gate.EVENT_ID_SCALE} (event ids must stay "
                        f"on the board's 19-digit scale)"), lines[0]
    assert lines[1].startswith(f"VIOLATION {gate.EVENT_ID_CLASS} 3: id=1790080400000000 "), lines[1]
    assert "FAIL" in out, out


def test_descending_19_digit_id_fires(tmp_path):
    """Order half of the rule: a 19-digit id below the file's MAX (not merely
    below the previous line) exits 1 naming both ids."""
    rc, out = _run_gate(tmp_path, _event(
        f'{{"id": {BIG}, "event_type": "audit"}}',
        f'{{"id": {BIG + 4}, "event_type": "audit"}}',
        f'{{"id": {BIG + 1}, "event_type": "audit"}}',
    ))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out) == [
        f"VIOLATION {gate.EVENT_ID_CLASS} 3: id={BIG + 1} descends below earlier id "
        f"{BIG + 4} (ids must ascend; gaps tolerated)"], out


def test_log_with_no_19_digit_id_is_entirely_off_scale(tmp_path):
    """A log whose ids are ALL sub-10^18 establishes no scale at all — the shape
    a wrong-basis writer produces into a new log. Every int id is reported."""
    rc, out = _run_gate(tmp_path, _event(
        f'{{"id": {MICRO}}}',
        '{"id": 1790080400000000}',
    ))

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out)
    assert len(lines) == 2, out
    assert "the file carries no id at that scale" in lines[0], lines[0]


# ── tolerances that keep the gate usable on the live board ────────────────

def test_live_board_prefix_and_duplicates_are_tolerated(tmp_path):
    """The live log's own shape: 979 pre-scale ids (1,2,3… then an epoch-seconds
    and an epoch-millis era) BEFORE the first 19-digit id, plus two duplicate
    19-digit ids (lines 1069 and 1128 of the real file). The append-only log
    cannot be renumbered and boardctl warns rather than fails on re-used ids, so
    the gate must stay silent — while still asserting the scale region."""
    rc, out = _run_gate(tmp_path, _event(
        '{"id": 1, "event_type": "legacy"}',
        '{"id": 2, "event_type": "legacy"}',
        '{"id": 1789703183, "event_type": "legacy-epoch-seconds"}',
        '{"id": 1789872174749, "event_type": "legacy-epoch-millis"}',
        '{"id": 1789915599706355200, "event_type": "task_completed"}',
        f'{{"id": {BIG - 1}, "event_type": "audit"}}',
        f'{{"id": {BIG}, "event_type": "audit"}}',
        f'{{"id": {BIG}, "event_type": "audit"}}',          # duplicate — tolerated
        f'{{"id": {BIG - 1}, "event_type": "audit"}}',      # duplicate of an EARLIER line
        f'{{"id": {BIG + 1}, "event_type": "audit"}}',
    ))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], out
    assert "4 pre-scale legacy id(s) tolerated" in out, out
    assert "2 duplicate id(s) tolerated" in out, out


def test_legacy_event_shapes_are_skipped(tmp_path):
    """Id-less annotation rows, null/empty/string/bool ids and malformed JSON are
    legacy shapes, not scale violations — the gate must skip them (and count
    none of them as int ids)."""
    rc, out = _run_gate(tmp_path, _event(
        '{"kind": "refile_suppressed", "detail": "annotation row, no id"}',
        '{"id": null, "event_type": "audit"}',
        '{"id": "", "event_type": "audit"}',
        '{"id": "PM-001", "event_type": "task_added"}',
        '{"id": "EVT-PM-20260920-001", "event_type": "audit"}',
        '{"id": true, "event_type": "audit"}',
        'not json at all',
        '["not", "an", "object"]',
        "",
    ))

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], out
    assert "0 int id(s) scanned" in out, out


def test_missing_events_file_skips_the_class_silently(tmp_path):
    """No events.jsonl in the board directory → the class says nothing at all
    (the documented behaviour checks 8/9 have for a missing board), so a CI
    runner or a test rig never has to carry one."""
    rc, out = _run_gate(tmp_path, None)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "event-id-ascending" not in out, out


def test_empty_events_file_exits_zero(tmp_path):
    """An empty log is valid — nothing to validate, nothing reported."""
    rc, out = _run_gate(tmp_path, [])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], out


# ── resolution + wiring ───────────────────────────────────────────────────

def test_explicit_events_flag_wins(tmp_path):
    """A caller can point the class at any log; the off-scale file behind the
    flag is the one reported, wherever the board lives."""
    other = tmp_path / "elsewhere-events.jsonl"
    other.write_text('{"id": 5}\n', encoding="utf-8")
    rc, out = _run_gate(tmp_path, [f'{{"id": {BIG}}}'],
                        extra=["--events", str(other)])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert str(other) in out, out
    assert _violations(out) == [
        f"VIOLATION {gate.EVENT_ID_CLASS} 1: id=5 is below the 19-digit floor "
        f"{gate.EVENT_ID_SCALE} and the file carries no id at that scale "
        f"(every int id in it is off-scale)"], out


def test_board_sibling_resolution_does_not_fall_back_to_this_checkout(tmp_path):
    """``--board`` in a directory with no events.jsonl resolves to NOTHING — it
    never falls back to this checkout's log (that would validate the wrong file
    silently)."""
    board = tmp_path / "tasks.jsonl"
    board.write_text(json.dumps({"id": "FIX-1", "status": "pending"}) + "\n", encoding="utf-8")

    assert gate.resolve_events_path(None, str(board), str(REPO_ROOT)) is None
    assert gate.resolve_events_path(None, None, str(REPO_ROOT)) == str(
        REPO_ROOT / ".coding-hermes" / "board" / "events.jsonl")


def test_scan_is_read_only(tmp_path):
    """The scanner reports without rewriting the log — bytes must be identical."""
    events = tmp_path / "events.jsonl"
    lines = [f'{{"id": {BIG}}}', f'{{"id": {MICRO}}}', '{"id": "PM-001"}']
    events.write_text("".join(l + "\n" for l in lines), encoding="utf-8")
    before = events.read_bytes()

    violations, stats = gate.scan_event_ids(str(events))

    assert len(violations) == 1 and violations[0][0] == 2, violations
    assert stats["int_ids"] == 2 and stats["max_id"] == BIG, stats
    assert events.read_bytes() == before


def test_new_check_class_is_registered_and_documented():
    """The daily report builds its summary from CHECK_CLASSES, and the numbered
    Checks list is the class's only documentation — a class missing from either
    is an incomplete change (same contract check 10 pins)."""
    assert gate.EVENT_ID_CLASS == "event-id-ascending"
    assert gate.EVENT_ID_SCALE == 10 ** 18
    assert gate.EVENT_ID_CLASS in gate.CHECK_CLASSES, gate.CHECK_CLASSES
    doc = gate.__doc__ or ""
    assert " 11. event-id-ascending" in doc, "check 11 missing from the docstring Checks list"
    assert f"VIOLATION {gate.EVENT_ID_CLASS}" in doc, "docstring carries no example violation"
    assert "board checks (8-11)" in doc, "--board-only description must cover the new class"
