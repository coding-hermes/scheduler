"""SCHED-GAP-16967 regression fixtures for the hop-liveness alert consumer.

Drives ``ops/hop-alert-consume.py`` in-process against FIXTURE alert files
under ``tmp_path`` (never the live ~/.hermes/logs path), pinning the row's
four acceptance criteria:

  * AC1 (observable effect) — one ``event=down`` alert becomes exactly ONE
    pending board row naming the hop, the exit status and how long the hop
    has been down; the row shape is the current board writer shape;
  * AC2 (idempotence) — replaying the same record (same hop+event+at) opens
    nothing new, with AND without the state file (the board scan alone is a
    sufficient dedup), and an ``event=recovered`` record closes the row it
    opened in the exact shape ``schedulerd --verify-board`` treats as closed
    with evidence (board_closure.go: status=complete AND worker_status=
    complete AND completed_at non-empty AND >=1 of reasoning/commit_hash/
    worker_summary);
  * AC3 (falsifier) — the no-consumer direction is pinned by test_brief_ac3:
    an injected down record no consumer has read leaves the board untouched;
  * AC4 (rotation/retention) — truncation and replacement reset the offset,
    the replay is deduped, untouched board lines stay BYTE-IDENTICAL across
    a closure rewrite, and the ledger evicts at the retention cap.

Cross-gate: the consumer's rows are run through the repo's own invariant
checker (ops/check-fleet-invariants.py --board-only) — vocabulary, id-slot
and content-dup must stay green on rows this script mints.

Run:  python3 -m pytest tests/test_hop_alert_consume.py -v
"""
from __future__ import annotations

import importlib.util
import io
import json
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
CONSUMER_PATH = REPO_ROOT / "ops" / "hop-alert-consume.py"
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"

# The exact record shape ops/hop-liveness-watch.sh emits (the crier-fleet
# 11h clean-exit outage from docs/hop-liveness-watchdog.md).
DOWN_RECORD = {
    "schema": "hop-liveness/1",
    "event": "down",
    "hop": "crier",
    "unit": "crier-fleet.service",
    "state": "down",
    "reason": "HOP_DOWN_CLEAN_EXIT",
    "exit_code": "0",
    "exit_kind": "exited",
    "result": "success",
    "down_s": 2460,
    "down_for": "41m",
    "since": "2026-10-29T04:03:00Z",
    "at": "2026-10-29T04:44:00Z",
    "repeat": False,
    "watchdog": "ops/hop-liveness-watch.sh",
}
RECOVERED_RECORD = {
    "schema": "hop-liveness/1",
    "event": "recovered",
    "hop": "crier",
    "unit": "crier-fleet.service",
    "state": "up",
    "reason": "HOP_UP",
    "exit_code": "0",
    "exit_kind": "exited",
    "result": "success",
    "down_s": None,
    "down_for": "",
    "since": None,
    "at": "2026-10-29T05:20:00Z",
    "repeat": False,
    "watchdog": "ops/hop-liveness-watch.sh",
}


def _load_module(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None, f"cannot load {path}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


consumer = _load_module(CONSUMER_PATH, "hop_alert_consume")
gate = _load_module(GATE_PATH, "check_fleet_invariants")


def _write_alerts(tmp_path: Path, records: list[dict], name: str = "alerts.jsonl") -> Path:
    alerts = tmp_path / name
    with open(alerts, "w", encoding="utf-8") as fh:
        for rec in records:
            fh.write(json.dumps(rec) + "\n")
    return alerts


def _consume(tmp_path: Path, alerts: Path, *extra: str) -> tuple[int, dict, str]:
    """Run the consumer in-process; returns (rc, summary, stdout text)."""
    board = tmp_path / "board" / "tasks.jsonl"
    state = tmp_path / "consumer-state.json"
    argv = [
        "--alerts", str(alerts),
        "--board", str(board),
        "--state", str(state),
        "--json", *extra,
    ]
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = consumer.main(argv)
    summary = {}
    text = buf.getvalue().strip()
    if text:
        summary = json.loads(text.splitlines()[-1])
    return rc, summary, text


def _board(tmp_path: Path) -> list[dict]:
    board = tmp_path / "board" / "tasks.jsonl"
    if not board.is_file():
        return []
    rows = []
    with open(board, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


# ---------------------------------------------------------------- AC1: effect


def test_one_down_alert_is_exactly_one_effect_naming_hop_exit_and_downtime(tmp_path):
    """AC1+AC3 running direction: one injected down record -> exactly ONE
    pending row naming the hop, the exit status and how long it has been
    down."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0, summary
    assert summary["effects"]["rows_opened"] == 1, summary
    rows = _board(tmp_path)
    assert len(rows) == 1, rows
    row = rows[0]
    assert row["status"] == "pending"
    assert row["id"] == "HOPDOWN-crier-2026-10-29T044400Z"
    assert "crier" in row["title"]
    assert "exit=0" in row["title"] or "exited 0" in row["title"]
    assert "41m" in row["title"]                      # how long it has been down
    assert "HOP_DOWN_CLEAN_EXIT" in row["title"]
    assert row["priority"] == "P1"
    # The row shape is the board's current writer key set.
    assert set(row.keys()) == set(consumer.ROW_KEYS)


def test_recovered_record_closes_the_effect_with_closure_evidence(tmp_path):
    """AC2: the recovered record closes the row it opened, in the exact shape
    board_closure.go treats as closed WITH evidence."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD, RECOVERED_RECORD])

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["effects"]["rows_opened"] == 1
    assert summary["effects"]["rows_closed"] == 1
    rows = _board(tmp_path)
    assert len(rows) == 1
    row = rows[0]
    assert row["status"] == "complete"
    assert row["worker_status"] == "complete"
    assert row["completed_at"]                      # non-empty
    assert "recovered" in row["worker_summary"]
    assert "crier" in row["worker_summary"]         # closure evidence names the hop


def test_consumer_rows_pass_the_repo_invariant_gate(tmp_path):
    """Cross-gate: a board holding the consumer's rows (one open, one closed)
    is green under check-fleet-invariants --board-only — no board-vocab,
    board-legacy-status, id-slot or content-dup violations."""
    alerts = _write_alerts(tmp_path, [
        DOWN_RECORD,
        {**DOWN_RECORD, "hop": "router", "unit": "task-router-proxy.service",
         "at": "2026-10-29T04:45:00Z"},
    ])
    _consume(tmp_path, alerts)
    board = tmp_path / "board" / "tasks.jsonl"

    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "no-scheduler.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(board),
            "--board-only",
        ])
    out = buf.getvalue()
    assert rc == 0, out
    assert "VIOLATION" not in out, out
    assert "PASS" in out, out


# ------------------------------------------------------------- AC2: idempotence


def test_replaying_the_same_record_does_not_duplicate_the_effect(tmp_path):
    """AC2: same hop+event+at again -> zero new effects, still one row."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])
    _consume(tmp_path, alerts)

    rc, summary, _ = _consume(tmp_path, alerts)   # full replay (fresh read, ledger hit)

    assert rc == 0
    assert summary["effects"]["rows_opened"] == 0
    assert summary["already"] >= 0
    assert len(_board(tmp_path)) == 1


def test_replay_dedup_survives_the_loss_of_the_state_file(tmp_path):
    """AC2 strong form: with the state file deleted (offset AND ledger gone),
    the board scan alone dedups the replay — the row id is derived from
    hop+event+at, so idempotence outlives the consumer's bookkeeping."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])
    _consume(tmp_path, alerts)
    (tmp_path / "consumer-state.json").unlink()

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["effects"]["rows_opened"] == 0
    assert summary["rotation_resets"] >= 0
    assert len(_board(tmp_path)) == 1


def test_second_down_record_while_incident_open_does_not_open_a_second_row(tmp_path):
    """One incident, one row: a second down record for the same hop while the
    incident row is still open (state lost, so the open-incident dedup path
    is the only guard) opens nothing new."""
    alerts = _write_alerts(tmp_path, [
        DOWN_RECORD,
        {**DOWN_RECORD, "at": "2026-10-29T05:44:00Z", "down_s": 4860,
         "down_for": "1h21m", "repeat": True},
    ])
    _consume(tmp_path, alerts)
    (tmp_path / "consumer-state.json").unlink()   # force the board-scan path

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["dedup_open_incident"] >= 1, summary
    rows = _board(tmp_path)
    assert len(rows) == 1
    assert rows[0]["id"] == "HOPDOWN-crier-2026-10-29T044400Z"


def test_replayed_recovery_does_not_reopen_or_double_close(tmp_path):
    """AC2: replaying the recovered record changes nothing — a closed row
    stays closed and no second closure effect is counted."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD, RECOVERED_RECORD])
    _consume(tmp_path, alerts)
    before = _board(tmp_path)

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["effects"]["rows_closed"] == 0
    assert summary["effects"]["rows_opened"] == 0
    assert _board(tmp_path) == before


def test_strict_exits_2_only_when_a_new_effect_was_opened(tmp_path):
    """--strict mirrors the watcher's convention: exit 2 for a fresh down
    effect, exit 0 when everything was already filed."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])

    rc1, _, _ = _consume(tmp_path, alerts, "--strict")
    assert rc1 == 2

    rc2, _, _ = _consume(tmp_path, alerts, "--strict")
    assert rc2 == 0


# ------------------------------------------------ AC4: rotation and retention


def test_truncated_file_resets_offset_and_replay_dedupes(tmp_path):
    """AC4: an in-place truncation (offset now past EOF) is detected, the
    offset resets to 0, and the re-read replay opens nothing AND re-opens
    nothing recovery already closed — a closed row stays closed."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD, RECOVERED_RECORD])
    _consume(tmp_path, alerts)                    # row opened, then closed
    # Rotation: truncate in place, keep only the down record (file is now
    # SHORTER than the stored offset — a detectable truncation).
    with open(alerts, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(DOWN_RECORD) + "\n")

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["rotation_resets"] == 1, summary
    assert summary["effects"]["rows_opened"] == 0
    assert summary["effects"]["rows_closed"] == 0
    assert len(_board(tmp_path)) == 1
    assert _board(tmp_path)[0]["status"] == "complete"   # recovery sticks


def test_rotated_file_replacement_resets_offset_and_replay_dedupes(tmp_path):
    """AC4: a replaced (rotated) file — new inode, fresh content — re-reads
    from 0 and the replayed record still opens nothing, even with the state
    file's ledger intact."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])
    _consume(tmp_path, alerts)
    fresh = tmp_path / "alerts-rotated.jsonl"
    with open(fresh, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(DOWN_RECORD) + "\n")
    fresh.replace(alerts)                          # new inode, same record

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["rotation_resets"] == 1, summary
    assert summary["effects"]["rows_opened"] == 0
    assert len(_board(tmp_path)) == 1


def test_closure_rewrite_leaves_sibling_lines_byte_identical(tmp_path):
    """AC4 retention contract: closing one row must not re-serialize rows it
    is not closing — a pre-existing sibling line survives the closure
    rewrite byte-for-byte (the live board is written by many hands)."""
    sibling = json.dumps(
        {"id": "SIBLING-1", "title": "hand-written  row  with  spacing",
         "status": "pending", "unicode": "caf\xe9-\u2014em-dash"},
        ensure_ascii=True, separators=(", ", ": "))
    board = tmp_path / "board" / "tasks.jsonl"
    board.parent.mkdir(parents=True, exist_ok=True)
    with open(board, "w", encoding="utf-8") as fh:
        fh.write(sibling + "\n")
    alerts = _write_alerts(tmp_path, [DOWN_RECORD, RECOVERED_RECORD])
    _consume(tmp_path, alerts)

    lines = board.read_text(encoding="utf-8").splitlines()
    assert len(lines) == 2
    assert lines[0] == sibling                    # byte-identical


def test_ledger_evicts_at_the_retention_cap(tmp_path):
    """AC4: the processed-identity ledger is capped FIFO at --ledger; the
    board scan remains the dedup after eviction (row ids are deterministic)."""
    records = [
        {**DOWN_RECORD, "hop": f"hop{i}", "at": f"2026-10-29T04:44:{i:02d}Z"}
        for i in range(6)
    ]
    alerts = _write_alerts(tmp_path, records)
    rc1, s1, _ = _consume(tmp_path, alerts, "--ledger", "3")
    assert rc1 == 0
    assert s1["ledger_size"] == 3, s1             # 6 consumed, capped to 3
    assert s1["effects"]["rows_opened"] == 6

    # Full replay after eviction: ledger misses, the BOARD dedup holds.
    alerts_full = tmp_path / "alerts-replay.jsonl"
    with open(alerts_full, "w", encoding="utf-8") as fh:
        for rec in records:
            fh.write(json.dumps(rec) + "\n")
    rc2, s2, _ = _consume(tmp_path, alerts_full)  # new path => fresh offset
    assert rc2 == 0
    assert s2["effects"]["rows_opened"] == 0, s2
    assert len(_board(tmp_path)) == 6


# ------------------------------------------------------- record-level behavior


def test_partial_trailing_line_is_not_consumed_until_complete(tmp_path):
    """AC4: a torn trailing write (no newline) is left for the next run, then
    consumed once the line is complete."""
    alerts = tmp_path / "alerts.jsonl"
    line = json.dumps(DOWN_RECORD)
    with open(alerts, "w", encoding="utf-8") as fh:
        fh.write(line[: len(line) // 2])          # torn write, no newline
    rc, summary, _ = _consume(tmp_path, alerts)
    assert rc == 0
    assert summary["effects"]["rows_opened"] == 0
    assert len(_board(tmp_path)) == 0

    with open(alerts, "a", encoding="utf-8") as fh:
        fh.write(line[len(line) // 2:] + "\n")    # write completes
    rc, summary, _ = _consume(tmp_path, alerts)
    assert rc == 0
    assert summary["effects"]["rows_opened"] == 1
    assert len(_board(tmp_path)) == 1


def test_malformed_and_non_object_lines_are_skipped_and_counted(tmp_path):
    alerts = tmp_path / "alerts.jsonl"
    with open(alerts, "w", encoding="utf-8") as fh:
        fh.write("not json at all\n")
        fh.write("[1, 2, 3]\n")                   # valid JSON, not an object
        fh.write(json.dumps({"event": "down"}) + "\n")   # no hop -> unusable
        fh.write(json.dumps(DOWN_RECORD) + "\n")         # the one good record

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["skipped"] == 3, summary
    assert summary["records"] == 1
    assert summary["effects"]["rows_opened"] == 1


def test_unknown_event_is_ledgered_without_effect(tmp_path):
    """A future event spelling must not crash the consumer nor open a row."""
    alerts = _write_alerts(tmp_path, [{**DOWN_RECORD, "event": "flap"}])

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["ignored_events"] == 1
    assert summary["effects"] == {"rows_opened": 0, "rows_closed": 0,
                                  "open_effect_ids": []}
    assert len(_board(tmp_path)) == 0


def test_offset_growth_consumes_only_new_records(tmp_path):
    """Normal tail: a second run over a grown file processes only the new
    record (the first hop's row is not re-opened, the second hop's is)."""
    alerts = _write_alerts(tmp_path, [DOWN_RECORD])
    _consume(tmp_path, alerts)
    with open(alerts, "a", encoding="utf-8") as fh:
        fh.write(json.dumps({**DOWN_RECORD, "hop": "bunker",
                             "unit": "bunkerd.service",
                             "at": "2026-10-29T04:50:00Z",
                             "reason": "HOP_DOWN_FAILURE",
                             "exit_code": "1"}) + "\n")

    rc, summary, _ = _consume(tmp_path, alerts)

    assert rc == 0
    assert summary["records"] == 1
    assert summary["effects"]["rows_opened"] == 1
    rows = _board(tmp_path)
    assert len(rows) == 2
    assert {r["id"] for r in rows} == {
        "HOPDOWN-crier-2026-10-29T044400Z",
        "HOPDOWN-bunker-2026-10-29T045000Z",
    }
