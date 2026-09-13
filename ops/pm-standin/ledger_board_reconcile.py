#!/usr/bin/env python3
"""Reconcile the stand-in PM ledger with project boards (board task GAP-047).

The stand-in ledger (~/.hermes/stand-in/ledger.json) never learned about board
closures: items stay status='added'/'picked_up' for weeks while their board row
in <workdir>/.coding-hermes/board/tasks.jsonl is already 'complete'. That drift
feeds a dead-end escalation loop and blocks the PM propose leg.

This script reads the ledger, resolves each item's project to its board via
scheduler.db (table `projects`, column `workdir`, opened read-only), matches the
ledger id to a board row, and — only when the board row is 'complete' and the
ledger item is non-terminal — flips the ledger item to 'verified' with evidence.

Status vocabulary (verified mapping):
    board 'complete'      -> ledger 'verified'
    terminal ledger set   = {verified, complete, stale}
    non-terminal ledger   = {added, picked_up, blocked, in_progress}

Only three ledger fields are ever touched: status, last_checked_at,
verification_evidence. No other field is read-modified or rewritten.

Modes:
    (default)  dry-run: print what would change, per-project counts, totals; exit 0
    --apply    copy ledger.json -> ledger.json.bak-20260913, then write the new
               ledger atomically (tmp file in the same dir + os.replace).
               Refuses to write when the dry-run change count is 0 (nothing to do
               is not an error: exits 0 with a note).

Stdlib only: json, sqlite3, os, sys, datetime, argparse.
"""

import argparse
import json
import os
import re
import sqlite3
import sys
from datetime import datetime, timezone

LEDGER_DEFAULT = os.path.expanduser("~/.hermes/stand-in/ledger.json")
SCHEDULER_DB_DEFAULT = os.path.expanduser("~/.hermes/coding-hermes/scheduler.db")
BACKUP_SUFFIX_DEFAULT = "bak-20260913"
BOARD_REL = os.path.join(".coding-hermes", "board", "tasks.jsonl")

EVIDENCE_DATE = "2026-09-13"
EVIDENCE_TPL = (
    "RECONCILE " + EVIDENCE_DATE +
    " (GAP-047 closure tick): board row {id} complete in {path}"
)

TERMINAL_STATUSES = {"verified", "complete", "stale"}
NON_TERMINAL_STATUSES = {"added", "picked_up", "blocked", "in_progress"}
BOARD_DONE = "complete"
STUCK_AGE_HOURS = 48.0


# --------------------------------------------------------------------------- #
# helpers
# --------------------------------------------------------------------------- #
def now_utc_iso():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_ts(raw):
    """Tolerant ISO-8601 parse. Accepts 'Z' and '+00:00' suffixes and bare
    'YYYY-MM-DD HH:MM:SS'. Returns aware UTC datetime or None."""
    if not raw or not isinstance(raw, str):
        return None
    s = raw.strip()
    if not s:
        return None
    cand = s[:-1] + "+00:00" if s.endswith("Z") else s
    try:
        dt = datetime.fromisoformat(cand)
    except ValueError:
        for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%d"):
            try:
                dt = datetime.strptime(cand, fmt)
                break
            except ValueError:
                dt = None
        if dt is None:
            return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def age_hours(raw, now):
    dt = parse_ts(raw)
    if dt is None:
        return None
    return (now - dt).total_seconds() / 3600.0


def read_jsonl_rows(path):
    """Tolerant per-line JSONL parse; malformed lines skipped (counted).
    Returns (rows_by_id_keep_last, malformed_count)."""
    rows = {}
    malformed = 0
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except ValueError:
                malformed += 1
                continue
            if isinstance(rec, dict) and rec.get("id") not in (None, ""):
                rows[str(rec["id"])] = rec  # last occurrence wins (refiles)
    return rows, malformed


class ProjectResolver:
    """project -> board path, via scheduler.db workdir then path fallbacks."""

    def __init__(self, db_path):
        self.workdirs = {}
        self.db_error = None
        try:
            uri = "file:{}?mode=ro".format(db_path.replace("?", "%3f"))
            con = sqlite3.connect(uri, uri=True)
            try:
                for name, workdir in con.execute("SELECT name, workdir FROM projects"):
                    self.workdirs[str(name)] = workdir or ""
            finally:
                con.close()
        except sqlite3.Error as exc:
            self.db_error = "{}: {}".format(type(exc).__name__, exc)

    def resolve(self, project):
        """Return (board_path, workdir, source) or (None, workdir, reason)."""
        reasons = []
        if self.db_error:
            reasons.append("scheduler.db unusable ({})".format(self.db_error))
        wd = self.workdirs.get(project)
        candidates = []
        if wd:
            candidates.append((wd, "scheduler.db workdir"))
        else:
            reasons.append("project absent/empty workdir in scheduler.db")
        candidates.append((os.path.join("/home/kara", project), "fallback /home/kara/<project>"))
        candidates.append(
            (os.path.join("/home/kara", project.replace("-", "_")),
             "fallback /home/kara/<project with - -> _>")
        )
        for workdir, source in candidates:
            board = os.path.join(workdir, BOARD_REL)
            if os.path.exists(board):
                return board, workdir, source
            reasons.append("no board at {}".format(board))
        return None, wd, "; ".join(reasons)


def match_board_row(item_id, board_rows):
    """Exact id first, then a UNIQUE suffix match (logged as fuzzy).
    Returns (board_id, board_row, how) or (None, None, why)."""
    if item_id in board_rows:
        return item_id, board_rows[item_id], "exact"
    suffix = "-" + item_id
    cands = [
        bid for bid in board_rows
        if bid.endswith(suffix) or item_id.endswith("-" + bid)
    ]
    if len(cands) == 1:
        return cands[0], board_rows[cands[0]], "fuzzy"
    if len(cands) > 1:
        return None, None, "ambiguous({} candidates: {})".format(
            len(cands), ",".join(sorted(cands)[:4]))
    return None, None, "no_id_match"


def load_ledger(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def detect_format(raw):
    """Detect the ledger's existing json.dump formatting so a rewrite touches
    only the intended fields. Returns (indent, trailing_newline, ensure_ascii)."""
    try:
        raw.encode("ascii")
        ensure_ascii = True     # no literal non-ASCII bytes -> escapes were used
    except UnicodeEncodeError:
        ensure_ascii = False
    m = re.match(r"\{\n([ \t]*)\"", raw)
    if m:
        indent = len(m.group(1)) or 1
        return indent, raw.endswith("\n"), ensure_ascii
    if "{\n" in raw:
        return 1, raw.endswith("\n"), ensure_ascii
    return None, raw.endswith("\n"), ensure_ascii   # compact single-line style


def render_ledger(doc, indent, trailing_newline, ensure_ascii=True):
    if indent is None:
        out = json.dumps(doc, separators=(",", ":"), ensure_ascii=ensure_ascii)
    else:
        out = json.dumps(doc, indent=indent, ensure_ascii=ensure_ascii)
    if trailing_newline and not out.endswith("\n"):
        out += "\n"
    return out


def write_ledger_atomic(path, data, backup_suffix):
    """Backup original, then tmp-write + os.replace in the same directory.
    Format-preserving; returns (backup_path, format_report)."""
    with open(path, "r", encoding="utf-8") as fh:
        original = fh.read()
    indent, trailing_nl, ensure_ascii = detect_format(original)
    backup_path = "{}.{}".format(path, backup_suffix)
    with open(backup_path, "w", encoding="utf-8") as fh:
        fh.write(original)
    tmp_path = "{}.tmp.{}".format(path, os.getpid())
    with open(tmp_path, "w", encoding="utf-8") as fh:
        fh.write(render_ledger(data, indent, trailing_nl, ensure_ascii))
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp_path, path)
    return backup_path, (indent, trailing_nl, ensure_ascii)


# --------------------------------------------------------------------------- #
# main
# --------------------------------------------------------------------------- #
def main(argv=None):
    ap = argparse.ArgumentParser(
        description="Reconcile stand-in PM ledger statuses with project boards.")
    ap.add_argument("--apply", action="store_true",
                    help="write the ledger (default: dry-run)")
    ap.add_argument("--ledger", default=LEDGER_DEFAULT,
                    help="ledger.json path (default: %(default)s)")
    ap.add_argument("--scheduler-db", default=SCHEDULER_DB_DEFAULT,
                    help="scheduler.db path (default: %(default)s)")
    ap.add_argument("--backup-suffix", default=BACKUP_SUFFIX_DEFAULT,
                    help="suffix for the pre-write backup (default: %(default)s)")
    args = ap.parse_args(argv)

    now = datetime.now(timezone.utc)
    now_iso = now_utc_iso()
    mode = "APPLY" if args.apply else "DRY-RUN"

    print("=" * 78)
    print("ledger_board_reconcile.py  mode={}  (GAP-047)".format(mode))
    print("=" * 78)
    print("now (UTC)     : {}".format(now_iso))
    print("ledger        : {}".format(args.ledger))
    print("scheduler.db  : {}".format(args.scheduler_db))
    print("board         : <resolved workdir>/{}/tasks.jsonl".format(
        os.path.join(".coding-hermes", "board")))

    doc = load_ledger(args.ledger)
    items = doc.get("items")
    if not isinstance(items, list):
        print("FATAL: ledger has no 'items' list", file=sys.stderr)
        return 2
    print("ledger items  : {}".format(len(items)))
    if len(items) != 1803:
        print("NOTE          : item count != 1803 (expected baseline)")

    resolver = ProjectResolver(args.scheduler_db)
    print("db projects   : {} rows{}".format(
        len(resolver.workdirs),
        "" if not resolver.db_error else "  [ERROR: {}]".format(resolver.db_error)))
    print("")

    # ---- per-project board cache ------------------------------------------ #
    board_cache = {}      # project -> (board_path, source, rows, malformed)
    planned = []          # (project, item, board_id, board_path, how)
    per_project = {}      # project -> dict(counts)
    resolution_lines = []

    for item in items:
        status = item.get("status")
        if status not in NON_TERMINAL_STATUSES:
            continue
        project = item.get("project") or "<none>"
        counts = per_project.setdefault(project, {
            "candidates": 0, "would_close": 0, "still_pending": 0,
            "board_missing": 0, "no_id_match": 0, "ambiguous": 0, "fuzzy": 0,
        })
        counts["candidates"] += 1

        if project not in board_cache:
            board_path, workdir, source = resolver.resolve(project)
            if board_path is None:
                board_cache[project] = (None, source, {}, 0)
                resolution_lines.append(
                    "  {:<28} -> NO BOARD   ({})".format(project, source))
            else:
                rows, malformed = read_jsonl_rows(board_path)
                board_cache[project] = (board_path, source, rows, malformed)
                resolution_lines.append(
                    "  {:<28} -> {}  [{}]  rows={} malformed={}".format(
                        project, board_path, source, len(rows), malformed))

        board_path, source, rows, malformed = board_cache[project]
        if board_path is None:
            counts["board_missing"] += 1
            continue

        board_id, row, how = match_board_row(str(item.get("id")), rows)
        if board_id is None:
            if how.startswith("ambiguous"):
                counts["ambiguous"] += 1
            else:
                counts["no_id_match"] += 1
            continue

        row_status = row.get("status")
        if row_status != BOARD_DONE:
            counts["still_pending"] += 1
            continue

        counts["would_close"] += 1
        if how == "fuzzy":
            counts["fuzzy"] += 1
        planned.append((project, item, board_id, board_path, how))

    # ---- board resolution table ------------------------------------------- #
    print("-" * 78)
    print("BOARD RESOLUTION ({} projects with non-terminal ledger items)".format(
        len(board_cache)))
    print("-" * 78)
    for line in resolution_lines:
        print(line)
    print("")

    # ---- planned changes -------------------------------------------------- #
    print("-" * 78)
    print("PLANNED LEDGER CLOSURES (board row 'complete' + ledger non-terminal)")
    print("-" * 78)
    for project, item, board_id, board_path, how in planned:
        extra = "" if how == "exact" else "  [{}]".format(how)
        print("  {:<28} {:<16} {} -> verified{}".format(
            project, str(item.get("id")), item.get("status"), extra))
        if how == "fuzzy":
            print("      fuzzy match: ledger {} <-> board {}".format(
                item.get("id"), board_id))
    print("")
    print("PER-PROJECT COUNTS (non-terminal candidates -> would_close)")
    for project in sorted(per_project):
        c = per_project[project]
        print("  {:<28} cand={:<3} close={:<3} pending={:<3} missing={:<3} "
              "nomatch={:<3} ambiguous={:<3} fuzzy={}".format(
                  project, c["candidates"], c["would_close"], c["still_pending"],
                  c["board_missing"], c["no_id_match"], c["ambiguous"], c["fuzzy"]))
    print("")

    totals = {
        k: sum(c[k] for c in per_project.values())
        for k in ("candidates", "would_close", "still_pending",
                  "board_missing", "no_id_match", "ambiguous", "fuzzy")
    }
    print("TOTALS: candidates={candidates} would_close={would_close} "
          "still_pending={still_pending} board_missing={board_missing} "
          "no_id_match={no_id_match} ambiguous={ambiguous} fuzzy={fuzzy}".format(**totals))
    print("")

    # ---- apply / dry-run -------------------------------------------------- #
    changes = totals["would_close"]
    applied = False
    backup_path = None
    if args.apply:
        print("-" * 78)
        print("APPLY")
        print("-" * 78)
        if changes == 0:
            print("nothing to do: 0 ledger items would change; NOT writing "
                  "(exit 0).")
        else:
            with open(args.ledger, "r", encoding="utf-8") as fh:
                raw_before = fh.read()
            indent, trailing_nl, ensure_ascii = detect_format(raw_before)
            roundtrip = render_ledger(
                doc, indent, trailing_nl, ensure_ascii) == raw_before
            print("format roundtrip (doc unchanged): {} [indent={}, "
                  "trailing_newline={}, ensure_ascii={}]".format(
                      "IDENTICAL" if roundtrip else "MISMATCH",
                      indent, trailing_nl, ensure_ascii))
            for project, item, board_id, board_path, how in planned:
                item["status"] = "verified"
                item["last_checked_at"] = now_iso
                item["verification_evidence"] = EVIDENCE_TPL.format(
                    id=board_id, path=board_path)
            backup_path, fmt = write_ledger_atomic(args.ledger, doc, args.backup_suffix)
            applied = True
            print("backup written : {}".format(backup_path))
            print("ledger written : {} (atomic tmp + os.replace, format-preserving)".
                  format(args.ledger))
            print("fields touched : status, last_checked_at, verification_evidence")
            print("items closed   : {}".format(changes))
    else:
        print("-" * 78)
        print("DRY-RUN: {} ledger item(s) would be closed; nothing written.".format(changes))
        print("Re-run with --apply to write (atomic + backup).")
        print("-" * 78)

    # ---- residual stuck analysis (reads the ledger fresh from disk) -------- #
    fresh = load_ledger(args.ledger)
    fresh_items = fresh.get("items", [])
    residual = []
    age_unknown = []
    for item in fresh_items:
        status = item.get("status")
        if status not in NON_TERMINAL_STATUSES:
            continue
        age = age_hours(item.get("added_at"), now)
        if age is None:
            age_unknown.append(item)
            residual.append((item, None))
        elif age > STUCK_AGE_HOURS:
            residual.append((item, age))

    print("")
    print("=" * 78)
    print("RESIDUAL STUCK SET (recomputed from ledger on disk after this run)")
    print("=" * 78)
    print("definition    : ledger status in {} AND age(added_at) > {}h (UTC)".format(
        sorted(NON_TERMINAL_STATUSES), int(STUCK_AGE_HOURS)))
    print("ledger items  : {}".format(len(fresh_items)))
    print("stuck_48h     : {}".format(len(residual)))
    print("age_unknown   : {} (added_at unparseable -> counted in stuck)".format(
        len(age_unknown)))
    print("mode          : {}{}".format(
        mode,
        "" if applied else " (ledger on disk is the PRE-apply state; "
        "closeable rows below appear here because nothing was written)"))
    print("PASS target   : stuck_48h < 20  -> {}".format(
        "PASS" if len(residual) < 20 else "FAIL (honest residual)"))

    reasons = {
        "board_still_pending": [],   # board row exists but is NOT 'complete'
        "board_complete_unwritten": [],  # closeable now, only possible pre-write
        "board_missing": [],
        "no_id_match": [],
        "ambiguous": [],
        "age_unknown": [],
    }
    for item, age in residual:
        project = item.get("project") or "<none>"
        item_id = str(item.get("id"))
        if age is None:
            reasons["age_unknown"].append((project, item_id, "added_at unparseable"))
            continue
        board_path, source, rows, malformed = board_cache.get(
            project, (None, "not resolved this run", {}, 0))
        if board_path is None:
            reasons["board_missing"].append((project, item_id, source))
            continue
        board_id, row, how = match_board_row(item_id, rows)
        if board_id is None:
            key = "ambiguous" if how.startswith("ambiguous") else "no_id_match"
            reasons[key].append((project, item_id, how))
        elif row.get("status") == BOARD_DONE:
            reasons["board_complete_unwritten"].append(
                (project, item_id, "board {} status=complete but ledger still {}".format(
                    board_id, item.get("status"))))
        else:
            reasons["board_still_pending"].append(
                (project, item_id, "board {} status={}".format(board_id, row.get("status"))))

    for key in ("board_still_pending", "board_complete_unwritten", "board_missing",
                "no_id_match", "ambiguous", "age_unknown"):
        bucket = reasons[key]
        print("")
        print("  {}: {}".format(key, len(bucket)))
        by_project = {}
        for project, item_id, detail in bucket:
            by_project.setdefault(project, []).append((item_id, detail))
        for project in sorted(by_project):
            entries = by_project[project]
            print("    {:<28} {} item(s)".format(project, len(entries)))
            for item_id, detail in sorted(entries):
                print("        {}  {}".format(item_id, detail))

    print("")
    print("SUMMARY: items_closed={} applied={} residual_stuck={} backup={}".format(
        changes, applied, len(residual), backup_path or "none"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
