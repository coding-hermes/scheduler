#!/usr/bin/env python3
"""push_proposals.py — stand-in PM push-scratch → REAL board rows (SCHED-GAP-207).

Extracted verbatim from the inline heredoc in pm-standin-tick.sh (GAP-020's
deploy leg) so the two SCHED-GAP-207 writer rules are unit-testable instead
of living in an untestable bash heredoc.

The defect this fixes: the inline leg minted "PM-" + (max numeric row suffix
+ 1) with NO dedupe and NO open-row check, so a recurring proposal re-filed
day after day under PM-NNN ids while the previous copy sat open — the exact
id-as-SLOT shape SCHED-GAP-207 measured at 29% of open rows fleet-wide.

Rules enforced here, in order (INSERT-before-event doctrine preserved — the
caller still appends the event row itself after this script exits 0):

  1. UNIQUE ID PER FILED FINDING (load-bearing): a proposal whose TITLE
     token-overlaps an OPEN row's title (ratio >= TITLE_DUP_THRESHOLD) is a
     re-file of an existing finding — NO new row is minted for it. The
     previous row stays the single open finding; the suppression is
     annotated to the board's events.jsonl ("refile_suppressed" naming the
     surviving row id) so the closure protocol (ask 2) has an audit trail.
  2. ID CONTINUATION never collides: a new row's suffix continues after the
     highest PM-NNN suffix EVER used on the board (open or closed) — a
     minted id can never shadow an older row.
  3. LAST-RESORT GUARD: if a computed id already exists on the board (any
     status — a hand-written board may not follow the PM-NNN suffix
     convention), the proposal is skipped with an event annotation rather
     than appended. The board can therefore never gain a second open row
     under one id from this writer.

Event-id rule (SCHED-GAP-206): every event this leg appends carries an id on
the board's 19-digit epoch-nanosecond scale, derived from the LOG's own max id
by ``next_event_id`` (never from an epoch/constant basis — the defect that
planted two epoch-microsecond lines in this project's events.jsonl and FAILed
the live ``boardctl validate``). The helper is public so any caller that needs
the next event id uses the same basis.

Board shape compatibility: writes the same row dict the inline leg wrote
(id/title/priority/status/origin/created_at/reasoning) — no schema change.

Args:  BOARD_JSONL_PATH EVENTS_JSONL_PATH SCRATCH_PATH LEDGER_PATH PM_TARGET
Modes: dry-run by default (prints what WOULD happen, writes nothing);
       --apply mutates board + events + ledger.

Exit codes: 0 ok (even when every proposal was suppressed — that is the fix
working); 1 usage/IO error. A missing board exits 0 with a note (same
tolerance as the inline leg: a project without a board is skipped, not an
error).
"""
import json
import os
import re
import sys
import time
from datetime import datetime, timezone

# Title token-overlap threshold for "this proposal re-files an open row".
# Mirrors qa.js jsFindSim: overlap over the SMALLER side, >= 0.5 = dup —
# tolerates the "[P1] " prefix and day-counter/wording drift between runs.
TITLE_DUP_THRESHOLD = 0.5
# Event id scale (SCHED-GAP-206). events.jsonl uses ONE id scale — the 19-digit
# epoch-NANOSECOND scale (>= 10^18) — and an id must ascend in file order.
# Measured 2026-09-22 on this repo's own board: two lines were appended at
# epoch-MICROSECOND scale (16 digits, 1790080373408208) while the file sat at
# 1789977500000000031, because a writer picked its basis from an epoch/constant
# source instead of reading the file's existing max id; the live gate
# (`boardctl validate`) FAILed with "ids must ascend". EVENT_ID_SCALE mirrors
# EVENT_ID_SCALE in ops/check-fleet-invariants.py (check 11,
# internal/scheduler/board_event_id.go).
EVENT_ID_SCALE = 10 ** 18
# Nanoseconds per second: the BASE an empty (or all-legacy) log gets —
# int(time.time()) * EVENT_ID_BASE_NS is a 19-digit epoch-nanosecond id.
EVENT_ID_BASE_NS = 1_000_000_000
_STOPWORDS = frozenset(
    "the and for with new pm proposal propose pending row rows already open "
    "closed fix landed should would could this that from into onto over".split()
)


def title_tokens(text):
    """Lowercase alphanumeric tokens minus stopwords; >2 chars."""
    return [
        w for w in re.sub(r"[^a-z0-9 ]", " ", str(text or "").lower()).split()
        if len(w) > 2 and w not in _STOPWORDS
    ]


def title_overlap(a, b):
    """Token overlap over the SMALLER side (0.0..1.0); 0.0 when either is empty."""
    ta, tb = title_tokens(a), title_tokens(b)
    if not ta or not tb:
        return 0.0
    inter = sum(1 for w in ta if w in tb)
    return inter / min(len(ta), len(tb))


def read_board(path):
    """Tolerant read: list of parsed dict rows; malformed lines skipped."""
    rows = []
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line.startswith("{"):
                    continue
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                if isinstance(rec, dict):
                    rows.append(rec)
    except FileNotFoundError:
        pass
    return rows


def open_titles_and_max_suffix(rows, prefix):
    """(open titles list, highest PM-NNN suffix ever used incl. closed rows)."""
    open_titles = []
    max_suffix = 0
    for r in rows:
        rid = str(r.get("id", ""))
        if not rid.startswith(prefix):
            continue
        suf = rid[len(prefix):]
        if suf.isdigit():
            max_suffix = max(max_suffix, int(suf))
        if str(r.get("status", "")).lower() in ("pending", "in_progress") and r.get("title"):
            open_titles.append(str(r["title"]))
    return open_titles, max_suffix


def append_line(path, obj):
    """O_APPEND one JSON line, terminating an unterminated tail first."""
    payload = json.dumps(obj) + "\n"
    prefix = b""
    try:
        with open(path, "rb") as fh:
            tail = fh.read()[-1:]
        if tail and tail != b"\n":
            prefix = b"\n"
    except (FileNotFoundError, OSError):
        pass
    with open(path, "ab") as fh:  # O_APPEND; never "w"
        fh.write(prefix + payload.encode("utf-8"))


def next_event_id(events_path):
    """Next events.jsonl id: ``max(existing int ids >= 10^18) + 1``.

    The id basis is the FILE, never an epoch/constant — that is the SCHED-GAP-206
    defect (a writer on a fixed epoch basis planted two epoch-microsecond lines
    that descended below the board's 19-digit ids and FAILed the live gate).
    Read-only: the log is opened, never written or rewritten; each call re-reads
    it, so a caller appending several events in a row gets ascending ids.

    The base, when *events_path* is missing/empty or carries no qualifying id
    (the log is new, or holds only the legacy string/sub-scale shapes): a fresh
    id on the same 19-digit scale, ``int(time.time()) * EVENT_ID_BASE_NS`` —
    epoch nanoseconds of the current second.

    Tolerance mirrors the readers (check 11, ``scan_event_ids``): malformed
    lines are skipped, and only a JSON INTEGER id >= EVENT_ID_SCALE counts —
    absent/null/string/bool ids and sub-scale ids are not a basis to continue
    from.
    """
    max_id = None
    try:
        with open(events_path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line.startswith("{"):
                    continue
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(rec, dict):
                    continue
                eid = rec.get("id")
                if isinstance(eid, bool) or not isinstance(eid, int):
                    continue
                if eid < EVENT_ID_SCALE:
                    continue
                if max_id is None or eid > max_id:
                    max_id = eid
    except (FileNotFoundError, OSError):
        pass
    if max_id is None:
        return int(time.time()) * EVENT_ID_BASE_NS
    return max_id + 1


def push(board_path, events_path, scratch_path, ledger_path, target, apply, home=None):
    """Deploy one push-scratch batch. Returns a result dict (also printed).

    ``home`` overrides the tilde root used for the per-project workdir
    existence check (the inline leg hardcoded os.path.expanduser("~"); the
    parameter is the test seam — production callers pass nothing).
    """
    prefix = "PM-"
    board_rows = read_board(board_path)
    open_titles, max_suffix = open_titles_and_max_suffix(board_rows, prefix)
    existing_ids = {str(r.get("id", "")) for r in board_rows}

    batch = None
    if scratch_path:
        try:
            with open(scratch_path, "r", encoding="utf-8") as fh:
                lines = [l for l in fh if l.strip().startswith("{")]
            if lines:
                batch = json.loads(lines[-1])  # newest batch only (inline-leg contract)
        except (OSError, ValueError):
            batch = None
    proposals = (batch or {}).get("proposals", []) if isinstance(batch, dict) else []

    result = {
        "target": target, "proposals": len(proposals), "filed": [], "suppressed": [],
        "skipped_scope": 0, "apply": apply,
    }
    if not proposals:
        print(json.dumps(result))
        return result

    ledger = None
    if apply and ledger_path:
        try:
            with open(ledger_path, "r", encoding="utf-8") as fh:
                ledger = json.load(fh)
            if not isinstance(ledger, dict):
                ledger = {"items": ledger}
        except (OSError, ValueError):
            ledger = {"items": []}
        if not isinstance(ledger.get("items"), list):
            ledger["items"] = []

    now_iso = datetime.now(timezone.utc).isoformat()
    home = home or os.path.expanduser("~")

    for pr in proposals:
        proj = pr.get("project", "")
        title = pr.get("title", "")
        prio = pr.get("priority", "P2")
        gap = pr.get("gap", "")
        if target and proj != target:
            result["skipped_scope"] += 1
            continue
        if not (proj and title and os.path.isdir(os.path.join(home, proj))
                and board_path and os.path.isfile(board_path)):
            continue

        # Rule 1: re-file suppression — an open row already carries this
        # finding. NO new row; annotate events so the closure protocol has
        # its audit trail (SCHED-GAP-207 ask 2).
        surviving = None
        for ot in open_titles:
            if title_overlap(title, ot) >= TITLE_DUP_THRESHOLD:
                surviving = ot
                break
        if surviving is not None:
            result["suppressed"].append({"project": proj, "title": title[:120]})
            if apply and events_path:
                try:
                    append_line(events_path, {
                        # Scale-correct event id (SCHED-GAP-206): derived from the
                        # log's own max, never from an epoch/constant basis.
                        "id": next_event_id(events_path),
                        "kind": "refile_suppressed", "source": "stand-in-pm-dagger",
                        "project": proj, "title": title[:120],
                        "detail": ("re-file of an open finding suppressed "
                                   "(SCHED-GAP-207): an open row already carries "
                                   "this condition — close the previous row before "
                                   "filing a sibling"),
                        "ts": now_iso,
                    })
                except OSError:
                    pass
            continue

        # Rule 2: mint a UNIQUE id — continue after the highest suffix ever
        # used; the mint loop skips past ANY occupied id (open, closed, or
        # hand-written shapes) so a colliding id is structurally impossible
        # (SCHED-GAP-207 rule 3).
        max_suffix += 1
        task_id = prefix + str(max_suffix).zfill(3)
        while task_id in existing_ids:
            max_suffix += 1
            task_id = prefix + str(max_suffix).zfill(3)
        existing_ids.add(task_id)

        result["filed"].append({"project": proj, "id": task_id, "title": title[:120]})
        if not apply:
            continue
        row = {
            "id": task_id,
            "title": title[:150], "priority": prio,
            "status": "pending", "origin": "stand-in-pm-dagger",
            "created_at": now_iso,
            "reasoning": f"gap: {gap[:200]}",
        }
        append_line(board_path, row)
        if events_path:
            try:
                append_line(events_path, {
                    "id": task_id, "kind": "task_added", "source": "stand-in-pm-dagger",
                    "detail": title[:120], "ts": now_iso,
                })
            except OSError:
                pass
        if ledger is not None:
            ledger["items"].append({
                "id": proj + ":" + task_id, "project": proj, "title": title[:120],
                "status": "added", "priority": prio, "added_at": now_iso,
                "origin": "dagger-stand-in"})

    if apply and ledger is not None:
        try:
            with open(ledger_path, "w", encoding="utf-8") as fh:
                json.dump(ledger, fh, indent=1)
        except OSError:
            pass
    print(json.dumps(result))
    return result


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    apply = "--apply" in argv
    argv = [a for a in argv if a != "--apply"]
    if len(argv) != 5:
        print("usage: push_proposals.py [--apply] BOARD EVENTS SCRATCH LEDGER PM_TARGET",
              file=sys.stderr)
        return 1
    push(argv[0], argv[1], argv[2], argv[3], argv[4], apply)
    return 0


if __name__ == "__main__":
    sys.exit(main())