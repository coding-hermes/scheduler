#!/usr/bin/env python3
"""Consume hop-liveness alerts into a real, agent-read effect (SCHED-GAP-16967).

ops/hop-liveness-watch.sh detects a dead gating hop and appends one JSON
record per alert to a JSONL file. Until SCHED-GAP-16967 nothing consumed that
file: a watchdog whose output nobody reads is the same "green but did
nothing" failure class it was built to catch, one layer down.

THE SURFACE
  This consumer turns each ``event=down`` alert into a PENDING ROW in the
  foreman board (``.coding-hermes/board/tasks.jsonl`` of the checkout it
  ships in). A pending row is the one effect in this fleet an agent provably
  reads: the scheduler's board-awareness reader
  (internal/scheduler/board_awareness.go countPendingBoard) counts pending
  rows and feeds the pending-boost tier, the foreman prompt picks pending
  rows as dispatchable work, and ops/check-fleet-invariants.py audits every
  row's vocabulary / id-slot / content-dup invariants. No log line involved.

  An ``event=recovered`` record CLOSES the effect it opened: the down row's
  status flips to ``complete`` (worker_status=complete, completed_at stamped,
  worker_summary carrying the recovery note) — exactly the closure shape
  schedulerd --verify-board treats as a closed row with evidence.

IDEMPOTENCE
  A record's identity is ``hop|event|at`` — the exact tuple the row's AC
  names. Replay protection is two-layered:

  1. the row id is derived from that identity (``HOPDOWN-<hop>-<at>``), so a
     replayed alert finds its row already on the board — any status — and
     does nothing. This guard survives even the LOSS of the state file;
  2. a processed-identity ledger in the state file is the fast path, and the
     dedup that survives ledger eviction (retention-capped FIFO).

  A replayed ``recovered`` record is also a no-op: a row closed by recovery
  stays closed (recovery is never undone by replay), and a second ``down``
  record for a hop that already has an OPEN incident row does not open a
  second row — one incident, one row.

ROTATION AND RETENTION
  The consumer tail-reads the alerts file from the byte offset stored in its
  state file; only complete (newline-terminated) lines are consumed, so a
  torn trailing write is picked up on the next run. Before reading it stats
  the file: when the stored offset is past EOF, or the inode changed, the
  file was truncated or rotated — the offset resets to 0 and the whole file
  is re-read. That replay is safe BY CONSTRUCTION (layer 1 above): already
  consumed records find their rows and no-ops. The ledger records what was
  processed; when it evicts (default cap 5000, oldest first), the board scan
  is the guard that remains. Records an external rotator deletes BEFORE a
  run sees them are gone — the watcher's hourly re-alert (repeat=true) is
  the backstop that still files the open incident under the open-incident
  dedup rule.

USAGE
  ops/hop-alert-consume.py [options]

    --alerts PATH   alert JSONL to consume
                    (env HOP_ALERT_CONSUME_ALERTS;
                     default ~/.hermes/logs/hop-liveness-alerts.jsonl)
    --board PATH    foreman board tasks.jsonl to write rows into
                    (env HOP_ALERT_CONSUME_BOARD;
                     default <this checkout>/.coding-hermes/board/tasks.jsonl)
    --state PATH    consumer state / offset file
                    (env HOP_ALERT_CONSUME_STATE;
                     default ~/.hermes/logs/hop-alert-consumer-state.json)
    --ledger N      processed-identity ledger retention (default 5000)
    --json          print ONE summary JSON object on stdout instead of the
                    human summary lines
    --strict        exit 2 when this run OPENED at least one new effect
                    (a down hop is outstanding — same convention as the
                    watcher's --strict)
    -h | --help

EXIT CODES
  0  ran; effects reported (a down hop is a finding, not a consumer fault)
  2  --strict and at least one new effect was opened this run
  3  the consumer itself could not run (alerts file unusable, unwritable
     board/state location, malformed state file that cannot be repaired)

One consumer at a time: an flock on <state>.lock serializes runs, the same
discipline the watcher uses, so a timer tick overlapping a manual run cannot
double-file an incident.

Tests: tests/test_hop_alert_consume.py drives this file in-process against
fixture alerts under a temp dir (never the live ~/.hermes/logs path), and
ops/hop-liveness-watch-selftest.sh section 6 is the end-to-end falsifier:
consumer stopped => no effect; consumer run => exactly one effect naming the
hop, the exit status and how long it has been down.
"""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

SCHEMA = "hop-alert-consume/1"
STATE_SCHEMA = "hop-alert-consume-state/1"

WATCHDOG = "ops/hop-liveness-watch.sh"
CONSUMER = "ops/hop-alert-consume.py"

# The status a filed incident row carries while the hop is still down.
ROW_STATUS_OPEN = "pending"
# The canonical closed spelling (internal/scheduler/board_closure.go /
# internal/blocks/board.go closedBoardStatus). Legacy spellings
# (done/completed/closed) are flagged by ops/check-fleet-invariants.py.
ROW_STATUS_CLOSED = "complete"
WORKER_STATUS_CLOSED = "complete"

# Open statuses in the reader vocabulary (internal/scheduler/board_freshness.go
# openStatuses; check-fleet-invariants BOARD_ALLOWED_STATUSES minus the closed
# spelling). An incident row in any of these means "already filed, still open".
OPEN_STATUSES = ("pending", "in_progress")

ROW_ID_PREFIX = "HOPDOWN-"
DEFAULT_LEDGER = 5000

# The exact key set of the board's current writer shape (the newest rows on
# the live board carry these keys; the canonical 31-key BoardTaskRow and the
# 19-key current shape share this core). Order mirrors the live rows.
ROW_KEYS = (
    "id", "title", "status", "priority", "complexity", "depends_on",
    "detail", "acceptance_criteria", "capability_tags", "worker_status",
    "dispatched_at", "completed_at", "attempts", "exit_code", "commit_hash",
    "guard_result", "ci_result", "worker_summary", "foreman_note",
    "blocked_reason", "review_notes", "created_at", "updated_at",
    "blocked_since",
)


def now_iso() -> str:
    """UTC timestamp in the board's ISO convention (2026-11-08T09:50:36+00:00)."""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def alert_identity(rec: dict, raw_line: str) -> str | None:
    """hop|event|at — the replay identity. None when the record is unusable.

    A record without ``at`` (never emitted by the current watcher) still gets
    a stable identity from the raw line's digest, so a replay of the same
    bytes stays deduped.
    """
    hop = rec.get("hop")
    event = rec.get("event")
    if not isinstance(hop, str) or not hop or not isinstance(event, str) or not event:
        return None
    at = rec.get("at")
    if not isinstance(at, str) or not at:
        at = "line-" + hashlib.sha256(raw_line.encode("utf-8")).hexdigest()[:16]
    return f"{hop}|{event}|{at}"


def row_id_for(rec: dict, identity: str) -> str:
    """Deterministic row id: HOPDOWN-<hop>-<at sanitized>.

    ``at`` is compacted to [A-Za-z0-9] (2026-10-29T04:44:00Z ->
    20261029T044400Z). A record with no ``at`` falls back to the same
    line-digest the identity used, so the id is still stable per replay.
    """
    hop = str(rec.get("hop", "")).strip()
    at = str(rec.get("at", "")).strip()
    if not at or at.startswith("line-"):
        at = identity.rsplit("|", 1)[-1]
    return ROW_ID_PREFIX + re.sub(r"[^A-Za-z0-9-]", "", f"{hop}-{at}")


def open_down_rows(rows: list[dict], hop: str) -> list[dict]:
    """Open incident rows (any `at`) for one hop, in file order."""
    prefix = ROW_ID_PREFIX + re.sub(r"[^A-Za-z0-9-]", "", hop) + "-"
    return [
        r for r in rows
        if str(r.get("id", "")).startswith(prefix) and r.get("status") in OPEN_STATUSES
    ]


def build_down_row(rec: dict, identity: str, rid: str, provenance: str) -> dict:
    """One pending board row naming the hop, the exit status and the downtime."""
    hop = rec.get("hop", "?")
    unit = rec.get("unit", "?")
    exit_code = rec.get("exit_code", "?")
    exit_kind = rec.get("exit_kind", "?")
    result = rec.get("result", "?")
    down_for = rec.get("down_for") or "unknown"
    since = rec.get("since") or "unknown"
    reason = rec.get("reason", "?")
    title = (
        f"Hop {hop} DOWN — unit {unit} exited {exit_code} ({exit_kind}/{result}), "
        f"down for {down_for} since {since} ({reason})"
    )
    acceptance = (
        f"1. Diagnose why hop {hop} ({unit}) is down: systemctl --user status {unit} "
        f"and journalctl -u {unit} -n 100 --no-pager; the watchdog saw exit={exit_code} "
        f"({exit_kind}, result={result}) and it has been down for {down_for} since {since}. "
        f"2. Restore the hop (restart or repair per fleet doctrine) or show the owner "
        f"ruling that it stays down. 3. Verify it is active again "
        f"(systemctl --user is-active {unit} = active) and only then close this row."
    )
    row = {
        "id": rid,
        "title": title,
        "status": ROW_STATUS_OPEN,
        "priority": "P1",
        "complexity": 1,
        "depends_on": [],
        "detail": {
            "source": CONSUMER,
            "schema": "hop-liveness/1",
            "alert_identity": identity,
            "alert": rec,
            "provenance": provenance,
        },
        "acceptance_criteria": acceptance,
        "capability_tags": ["ops", "hop-liveness"],
        "worker_status": ROW_STATUS_OPEN,
        "dispatched_at": None,
        "completed_at": None,
        "attempts": 0,
        "exit_code": None,
        "commit_hash": None,
        "guard_result": None,
        "ci_result": None,
        "worker_summary": None,
        "foreman_note": f"{CONSUMER}: auto-filed from {WATCHDOG} alert {identity} (SCHED-GAP-16967)",
        "blocked_reason": None,
        "review_notes": None,
        "created_at": now_iso(),
        "updated_at": now_iso(),
        "blocked_since": None,
    }
    assert set(row.keys()) == set(ROW_KEYS), "row shape drifted from ROW_KEYS"
    return row


def read_board(path: Path) -> list[dict]:
    """Every parseable dict row, in file order. Malformed lines are skipped —
    malformed JSONL is a different gate's concern (check-fleet-invariants)."""
    rows: list[dict] = []
    if not path.is_file():
        return rows
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except ValueError:
                continue
            if isinstance(obj, dict):
                rows.append(obj)
    return rows


def closure_fields(rec: dict, stamp: str) -> dict:
    """The field updates that close a row with recovery evidence.

    A closed row is status=complete AND worker_status=complete AND a non-empty
    completed_at, and carries at least one of reasoning/commit_hash/
    worker_summary (board_closure.go) — worker_summary carries the note here.
    """
    hop = rec.get("hop", "?")
    return {
        "status": ROW_STATUS_CLOSED,
        "worker_status": WORKER_STATUS_CLOSED,
        "completed_at": stamp,
        "updated_at": stamp,
        "worker_summary": (
            f"recovered: hop {hop} answered again (recovery record at "
            f"{rec.get('at', 'unknown')}); closed by {CONSUMER} (SCHED-GAP-16967)"
        ),
    }


def close_rows_in_file(path: Path, target_ids: set[str], rec: dict, stamp: str) -> int:
    """Rewrite ONLY the target rows' lines; every other line stays verbatim.

    The live board is written by many hands (foreman tick, PM stand-in,
    boardctl), so a closure must never re-serialize rows it is not closing.
    Returns the number of rows actually closed.
    """
    updates = closure_fields(rec, stamp)
    count = 0
    if path.is_file():
        original = path.read_text(encoding="utf-8", errors="surrogateescape")
    else:
        original = ""
    out: list[str] = []
    for line in original.splitlines(keepends=True):
        obj = None
        stripped = line.strip()
        if stripped:
            try:
                parsed = json.loads(stripped)
            except ValueError:
                parsed = None
            if isinstance(parsed, dict):
                obj = parsed
        if obj is not None and obj.get("id") in target_ids:
            obj.update(updates)
            out.append(json.dumps(obj, ensure_ascii=False) + "\n")
            count += 1
        else:
            out.append(line)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8", errors="surrogateescape") as fh:
            fh.write("".join(out))
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return count


def append_row(path: Path, row: dict) -> None:
    """Append one row as a single JSONL line (the watcher's append discipline)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(row, ensure_ascii=False) + "\n")


def new_records(alerts_path: Path, offset: int, inode: int | None):
    """Tail-read complete lines from *offset*, handling truncation/rotation.

    Returns (pairs, new_offset, rotation_resets, malformed) where pairs is a
    list of (record, raw_line) tuples for every line that parsed to a JSON
    object, and malformed counts the non-blank lines that did not. A
    truncated or replaced file resets the offset to 0 — replay safety is the
    board scan's job, so the reset is always safe.
    """
    resets = 0
    if not alerts_path.is_file():
        return [], offset, resets, 0
    st = alerts_path.stat()
    if offset > st.st_size:
        resets += 1          # truncated in place (or replaced by a shorter file)
        offset = 0
    elif inode is not None and st.st_ino != inode and offset > 0:
        resets += 1          # file replaced (rotated): content before EOF is unrelated
        offset = 0
    with open(alerts_path, "rb") as fh:
        fh.seek(offset)
        data = fh.read()
    cut = data.rfind(b"\n")
    if cut == -1:
        return [], offset, resets, 0
    consumed = data[: cut + 1]
    new_offset = offset + cut + 1
    pairs: list[tuple[dict, str]] = []
    malformed = 0
    for raw in consumed.splitlines():
        line = raw.decode("utf-8", errors="replace").strip()
        if not line:
            continue         # blank line: not a record, not an error
        try:
            obj = json.loads(line)
        except ValueError:
            malformed += 1
            continue
        if isinstance(obj, dict):
            pairs.append((obj, line))
        else:
            malformed += 1   # valid JSON, not an object
    return pairs, new_offset, resets, malformed


def load_state(path: Path, alerts_path: str) -> dict:
    """State bound to the alerts path; a different file starts fresh."""
    try:
        with open(path, encoding="utf-8") as fh:
            state = json.load(fh)
        if not isinstance(state, dict):
            raise ValueError("state is not an object")
    except (OSError, ValueError):
        state = {}
    if state.get("alerts") != alerts_path:
        state = {}
    state.setdefault("schema", STATE_SCHEMA)
    state.setdefault("alerts", alerts_path)
    state.setdefault("offset", 0)
    state.setdefault("inode", None)
    state.setdefault("ledger", [])
    state.setdefault("totals", {"records": 0, "rows_opened": 0, "rows_closed": 0})
    return state


def save_state(path: Path, state: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    state["updated_at"] = now_iso()
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def acquire_lock(state_path: Path):
    """Non-blocking flock on <state>.lock; None when another run holds it."""
    lock_path = Path(str(state_path) + ".lock")
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    fh = open(lock_path, "w")
    try:
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        fh.close()
        return None
    return fh


def main(argv: list[str] | None = None) -> int:
    here = Path(__file__).resolve().parent
    default_board = here.parent / ".coding-hermes" / "board" / "tasks.jsonl"
    home = os.environ.get("HOME", str(Path.home()))
    parser = argparse.ArgumentParser(
        description="Consume hop-liveness alerts into pending board rows (SCHED-GAP-16967)")
    parser.add_argument("--alerts", default=os.environ.get(
        "HOP_ALERT_CONSUME_ALERTS",
        str(Path(home) / ".hermes" / "logs" / "hop-liveness-alerts.jsonl")))
    parser.add_argument("--board", default=os.environ.get(
        "HOP_ALERT_CONSUME_BOARD", str(default_board)))
    parser.add_argument("--state", default=os.environ.get(
        "HOP_ALERT_CONSUME_STATE",
        str(Path(home) / ".hermes" / "logs" / "hop-alert-consumer-state.json")))
    parser.add_argument("--ledger", type=int, default=DEFAULT_LEDGER)
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--strict", action="store_true")
    args = parser.parse_args(argv)

    alerts_path = Path(args.alerts).expanduser()
    board_path = Path(args.board).expanduser()
    state_path = Path(args.state).expanduser()

    if args.ledger < 0:
        print(f"{CONSUMER}: --ledger must be >= 0", file=sys.stderr)
        return 3
    try:
        board_path.parent.mkdir(parents=True, exist_ok=True)
        state_path.parent.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        print(f"{CONSUMER}: cannot create board/state directory: {exc}", file=sys.stderr)
        return 3

    lock = acquire_lock(state_path)
    if lock is None:
        if not args.json:
            print(f"{CONSUMER}: another run holds the lock; skipping")
        return 0

    try:
        state = load_state(state_path, str(alerts_path))
        pairs, new_offset, resets, malformed = new_records(
            alerts_path, int(state["offset"]), state.get("inode"))
    except OSError as exc:
        print(f"{CONSUMER}: cannot read alerts/state: {exc}", file=sys.stderr)
        return 3

    try:
        st = alerts_path.stat()
        inode = st.st_ino
    except OSError:
        inode = None

    ledger: list[str] = list(state["ledger"])
    ledger_set = set(ledger)
    totals = dict(state["totals"])

    stats = {
        "records": 0,
        "skipped": malformed,
        "already": 0,
        "rows_opened": 0,
        "rows_closed": 0,
        "dedup_open_incident": 0,
        "ignored_events": 0,
        "rotation_resets": resets,
    }
    opened_ids: list[str] = []

    if pairs:
        board_rows = read_board(board_path)
        for rec, raw in pairs:
            identity = alert_identity(rec, raw)
            if identity is None:
                stats["skipped"] += 1
                continue
            stats["records"] += 1
            event = rec["event"]
            if identity in ledger_set:
                stats["already"] += 1
                continue
            if event == "down":
                rid = row_id_for(rec, identity)
                if any(r.get("id") == rid for r in board_rows):
                    stats["already"] += 1        # replayed alert: row already filed
                elif open_down_rows(board_rows, str(rec.get("hop", ""))):
                    stats["dedup_open_incident"] += 1  # one incident, one row
                else:
                    row = build_down_row(rec, identity, rid, f"offset {state['offset']}")
                    append_row(board_path, row)
                    board_rows.append(row)
                    stats["rows_opened"] += 1
                    opened_ids.append(rid)
            elif event == "recovered":
                targets = {r["id"] for r in open_down_rows(board_rows, str(rec.get("hop", "")))}
                if targets:
                    stamp = now_iso()
                    closed = close_rows_in_file(board_path, targets, rec, stamp)
                    stats["rows_closed"] += closed
                    # Keep the in-memory view in sync for the rest of this run
                    # (a later record must see the row as closed).
                    updates = closure_fields(rec, stamp)
                    for r in board_rows:
                        if r.get("id") in targets:
                            r.update(updates)
            else:
                stats["ignored_events"] += 1
            ledger.append(identity)
            ledger_set.add(identity)
        # An append-only run never rewrites the file (sibling writers are
        # safe); only a recovery closure rewrites, and it already persisted
        # above via write_board.

        if len(ledger) > args.ledger:
            ledger = ledger[-args.ledger:]

        totals["records"] = totals.get("records", 0) + stats["records"]
        totals["rows_opened"] = totals.get("rows_opened", 0) + stats["rows_opened"]
        totals["rows_closed"] = totals.get("rows_closed", 0) + stats["rows_closed"]

        state["offset"] = new_offset
        state["inode"] = inode
        state["ledger"] = ledger
        state["totals"] = totals
        try:
            save_state(state_path, state)
        except OSError as exc:
            print(f"{CONSUMER}: cannot write state file: {exc}", file=sys.stderr)
            return 3
    else:
        # Nothing new: still persist the rotation reset / inode refresh so the
        # next run does not re-detect the same rotation.
        if resets or state.get("inode") != inode:
            state["offset"] = new_offset
            state["inode"] = inode
            try:
                save_state(state_path, state)
            except OSError as exc:
                print(f"{CONSUMER}: cannot write state file: {exc}", file=sys.stderr)
                return 3

    summary = {
        "schema": SCHEMA,
        "at": now_iso(),
        "alerts": str(alerts_path),
        "board": str(board_path),
        "state": str(state_path),
        "records": stats["records"],
        "effects": {
            "rows_opened": stats["rows_opened"],
            "rows_closed": stats["rows_closed"],
            "open_effect_ids": opened_ids,
        },
        "dedup_open_incident": stats["dedup_open_incident"],
        "already": stats["already"],
        "skipped": stats["skipped"],
        "ignored_events": stats["ignored_events"],
        "rotation_resets": stats["rotation_resets"],
        "ledger_size": len(ledger),
        "totals": totals,
    }
    if args.json:
        print(json.dumps(summary, ensure_ascii=False))
    else:
        print(
            f"{CONSUMER}: {stats['records']} record(s) consumed, "
            f"{stats['rows_opened']} row(s) opened, {stats['rows_closed']} row(s) closed, "
            f"{stats['already']} replay(s) deduped, {stats['skipped']} skipped, "
            f"{stats['rotation_resets']} rotation reset(s) — board {board_path}"
        )

    if args.strict and stats["rows_opened"] > 0:
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
