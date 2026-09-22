#!/usr/bin/env python3
"""Reconcile the stand-in PM ledger with project boards (GAP-047, split GAP-049).

The stand-in ledger (~/.hermes/stand-in/ledger.json) never learned about board
closures: items stay status='added'/'picked_up' for weeks while their board row
in <workdir>/.coding-hermes/board/tasks.jsonl is already 'complete'. That drift
feeds a dead-end escalation loop and blocks the PM propose leg.

GAP-049 split: aged ledger items are NOT one homogeneous "stuck" problem.
This version classifies every aged non-terminal item into explicit,
machine-readable categories and re-evaluates stale items against board truth:

    board_open_work   board row exists and is genuinely open (not complete).
                      Legitimate open work — never "drift", never closable.
    drift_closable    board row is 'complete' but the ledger item is not
                      reconciled. The ONLY category eligible for auto-close,
                      and only on --apply.
    stale_drift       ledger status 'stale' (previous terminal state) whose
                      board row is now 'complete'. Re-evaluated, surfaced, and
                      reconciled to 'verified' with evidence on --apply.
    stale_terminal    stale ledger item whose board row is still genuinely
                      open. Stays terminal; only surfaced, never written.
    board_missing     no board found for the project.
    no_id_match       board exists but no board row matches the item id;
                      accompanied by per-project manual-review candidates
                      (title similarity — advisory only, NEVER auto-closed).
    ambiguous         multiple suffix-match candidates (previous fuzzy logic).
    age_unknown       added_at unparseable, so age cannot be computed.

Status vocabulary (verified mapping):
    board 'complete'      -> ledger 'verified'
    terminal ledger set   = {verified, complete}   (stale is RE-EVALUATED, not
                                                    silently treated as healthy)
    non-terminal ledger   = {added, picked_up, blocked, in_progress, stale}

Success semantics (GAP-049): there is NO universal "stuck < N" bar. Genuine
open work is not drift and must not fail a repair target. The actionable
measure is closable drift (drift_closable + stale_drift) plus the honest
manual-work buckets (board_missing, no_id_match, ambiguous, age_unknown).

Only three ledger fields are ever touched: status, last_checked_at,
verification_evidence. No other field is read-modified or rewritten.

Modes:
    (default)  dry-run: print classifications, per-project counts, totals,
               and a machine-readable summary JSON (between
               <<<RECONCILE-SUMMARY-JSON-BEGIN/END>>> markers); exit 0.
               Nothing is ever written.
    --apply    copy ledger.json -> ledger.json.<backup-suffix>, then write the
               new ledger atomically (tmp file in the same dir + os.replace).
               Refuses to write when the change count is 0 (nothing to do is
               not an error: exits 0 with a note).

digest_input() additionally powers the PM tick digest generator embedded in
pm-standin-tick.sh (imported from the live reconciler symlink), so the digest
and this tool share one classification implementation.

Stdlib only: json, sqlite3, os, sys, datetime, argparse, difflib, re.
"""

import argparse
import json
import os
import re
import sqlite3
import sys
from datetime import datetime, timezone
from difflib import SequenceMatcher

LEDGER_DEFAULT = os.path.expanduser("~/.hermes/stand-in/ledger.json")
SCHEDULER_DB_DEFAULT = os.path.expanduser("~/.hermes/coding-hermes/scheduler.db")
BACKUP_SUFFIX_DEFAULT = "bak-20260913"
BOARD_REL = os.path.join(".coding-hermes", "board", "tasks.jsonl")

EVIDENCE_DATE = "2026-09-13"
EVIDENCE_TPL = (
    "RECONCILE " + EVIDENCE_DATE +
    " (GAP-047 closure tick): board row {id} complete in {path}"
)

# GAP-049: 'stale' moved OUT of the terminal set — a stale ledger item is
# re-evaluated against board truth (drift if its board row is complete).
TERMINAL_STATUSES = {"verified", "complete"}
NON_TERMINAL_STATUSES = {"added", "picked_up", "blocked", "in_progress", "stale"}
BOARD_DONE = "complete"
STUCK_AGE_HOURS = 48.0

# Classification categories (machine-readable, stable order).
CATEGORIES = (
    "board_open_work", "drift_closable", "stale_drift", "stale_terminal",
    "board_missing", "no_id_match", "ambiguous", "age_unknown",
)
# Categories that --apply may write to the ledger. Exact board-ID match on a
# complete board row remains the ONLY automatic closure authority.
ACTIONABLE_CATEGORIES = ("drift_closable", "stale_drift")

# Title-similarity candidates (advisory, manual review only).
CANDIDATE_LIMIT = 3
CANDIDATE_MIN_SCORE = 0.30

SUMMARY_JSON_BEGIN = "<<<RECONCILE-SUMMARY-JSON-BEGIN>>>"
SUMMARY_JSON_END = "<<<RECONCILE-SUMMARY-JSON-END>>>"


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


# --------------------------------------------------------------------------- #
# GAP-049 classification + title-similarity helpers (pure, testable)
# --------------------------------------------------------------------------- #
def classify_item(status, board_status, board_found):
    """Pure classification per GAP-049.

    status        : ledger status string
    board_status  : board row status (ignored when board_found is False)
    board_found   : whether a board row was matched for the item
    Returns one of CATEGORIES.
    """
    if not board_found:
        return "board_missing"
    if status == "stale":
        return "stale_drift" if board_status == BOARD_DONE else "stale_terminal"
    if board_status == BOARD_DONE:
        return "drift_closable"
    return "board_open_work"


def title_similarity(a, b):
    """Deterministic 0.0..1.0 title similarity (difflib SequenceMatcher on
    lowercased, whitespace-collapsed titles)."""
    na = re.sub(r"\s+", " ", str(a or "").strip().lower())
    nb = re.sub(r"\s+", " ", str(b or "").strip().lower())
    if not na or not nb:
        return 0.0
    return SequenceMatcher(None, na, nb).ratio()


def title_candidates(item_title, board_rows, limit=CANDIDATE_LIMIT,
                     min_score=CANDIDATE_MIN_SCORE):
    """Deterministic, bounded advisory candidates for one unmatched item.

    Returns the top `limit` board rows by similarity >= min_score, ties broken
    by board id (stable order). NEVER applied automatically — these exist only
    to point a human at plausible matches.

    Each candidate: {board_id, board_title, board_status, score}.
    """
    scored = []
    for bid in sorted(board_rows):
        row = board_rows[bid]
        score = title_similarity(item_title, row.get("title") or row.get("summary") or "")
        if score >= min_score:
            scored.append((score, bid))
    scored.sort(key=lambda t: (-t[0], t[1]))
    out = []
    for score, bid in scored[:limit]:
        row = board_rows[bid]
        out.append({
            "board_id": bid,
            "board_title": str(row.get("title") or "")[:120],
            "board_status": row.get("status") or "",
            "score": round(score, 4),
        })
    return out


def build_no_id_matches(no_id_items, rows_cache):
    """Per-project no_id_match detail with advisory title candidates.

    no_id_items : {project: [ledger item dicts without a board-id match]}
    rows_cache  : {project: board rows dict or None}

    Returns {project: {"no_id_match": True, "count": N, "items": [...]}} —
    projects with zero unmatched items are absent. Deterministic: items sorted
    by item id, candidates from a sorted board-id walk, capped per item.
    """
    details = {}
    for project in sorted(no_id_items):
        unmatched = no_id_items[project]
        if not unmatched:
            continue
        rows = rows_cache.get(project) or {}
        entries = []
        for item in sorted(unmatched, key=lambda it: str(it.get("id") or "")):
            entries.append({
                "item_id": str(item.get("id") or ""),
                "title": str(item.get("title") or "")[:120],
                "ledger_status": item.get("status") or "",
                "candidates": title_candidates(item.get("title") or "", rows),
            })
        details[project] = {
            "no_id_match": True,
            "count": len(entries),
            "items": entries,
        }
    return details


def classify_aged_items(items, board_loader, now):
    """Classify every aged non-terminal ledger item against board truth.

    items        : ledger items list
    board_loader : callable project -> rows dict, or None when the project has
                   no board (single source of board access; callers may wrap
                   ProjectResolver + read_jsonl_rows, or inject a fixture).
    now          : aware UTC datetime reference for age computation.

    Aged = non-terminal status AND age(added_at) > STUCK_AGE_HOURS. Younger
    items and terminal items are ignored (they were never the drift set).

    Returns {"records": [...], "totals": {...}, "per_project": {...},
             "no_id_items": {project: [item]}, "rows_cache": {project: rows}}.
    Each record: {item, project, item_id, age, category, board_id, row, how}
    (board_id/row/how are None when no board row was matched; age is None for
    age_unknown items).
    """
    rows_cache = {}
    resolved_rows = {}   # project -> rows | None (None = board missing)
    per_project = {}
    no_id_items = {}
    records = []
    totals = {k: 0 for k in CATEGORIES}
    totals["aged_nonterminal"] = 0
    totals["fuzzy"] = 0

    def counts_for(project):
        return per_project.setdefault(project, dict(
            {k: 0 for k in CATEGORIES}, aged_nonterminal=0, fuzzy=0))

    for item in items:
        status = item.get("status")
        if status not in NON_TERMINAL_STATUSES:
            continue
        age = age_hours(item.get("added_at"), now)
        if age is not None and age <= STUCK_AGE_HOURS:
            continue
        project = item.get("project") or "<none>"
        item_id = str(item.get("id"))
        counts = counts_for(project)
        counts["aged_nonterminal"] += 1
        totals["aged_nonterminal"] += 1

        if age is None:
            category = "age_unknown"
            board_id = row = how = None
        else:
            if project not in resolved_rows:
                resolved_rows[project] = board_loader(project)
            rows = resolved_rows[project]
            rows_cache[project] = rows
            if rows is None:
                category, board_id, row, how = "board_missing", None, None, None
            else:
                board_id, row, how = match_board_row(item_id, rows)
                if board_id is None:
                    if how.startswith("ambiguous"):
                        category = "ambiguous"
                    else:
                        category = "no_id_match"
                        no_id_items.setdefault(project, []).append(item)
                else:
                    category = classify_item(status, row.get("status"), True)
                    if how == "fuzzy":
                        counts["fuzzy"] += 1
                        totals["fuzzy"] += 1

        counts[category] += 1
        totals[category] += 1
        records.append({
            "item": item, "project": project, "item_id": item_id,
            "age": age, "category": category,
            "board_id": board_id, "row": row, "how": how,
        })

    return {
        "records": records,
        "totals": totals,
        "per_project": per_project,
        "no_id_items": no_id_items,
        "rows_cache": rows_cache,
    }


def board_loader_from_resolver(resolver):
    """Wrap a ProjectResolver into a board_loader callable for
    classify_aged_items: project -> rows dict, or None when no board exists."""

    def loader(project):
        board_path, _workdir, _source = resolver.resolve(project)
        if board_path is None:
            return None
        rows, _malformed = read_jsonl_rows(board_path)
        return rows

    return loader


# --------------------------------------------------------------------------- #
# PM tick digest generation (shared with pm-standin-tick.sh)
# --------------------------------------------------------------------------- #
def digest_input(ledger_path, init_path, out_path, board_loader=None, now=None):
    """Build the PM digest bundle with the GAP-049 reconciliation split.

    board_loader None  -> reconciler unavailable: legacy status counts only,
                          the four GAP-049 counts are None (honest absence).
    board_loader given -> aged non-terminal items classified against board
                          truth; stuck_48h lists board_open_work items ONLY.

    Writes the bundle JSON to out_path (indent=1, live format) and returns the
    bundle dict.
    """
    if now is None:
        now = datetime.now(timezone.utc)
    with open(ledger_path, "r", encoding="utf-8") as fh:
        led = json.load(fh)
    items = led.get("items", led if isinstance(led, list) else [])
    counts = {}
    for it in items:
        st = it.get("status", "?")
        counts[st] = counts.get(st, 0) + 1

    oldest = []
    current_added = []
    for it in items:
        if it.get("status") not in TERMINAL_STATUSES:
            oldest.append({"id": it.get("id"), "project": it.get("project"),
                           "title": (it.get("title") or "")[:80],
                           "status": it.get("status"), "added_at": it.get("added_at", "")})
        if it.get("status") == "added":
            current_added.append({"id": it.get("id"), "project": it.get("project"),
                                  "title": (it.get("title") or "")[:100]})
    oldest.sort(key=lambda x: x.get("added_at", ""))

    reconciliation = None
    if board_loader is not None:
        report = classify_aged_items(items, board_loader, now)
        totals = report["totals"]
        no_id_details = build_no_id_matches(report["no_id_items"], report["rows_cache"])
        open_items = []
        for rec in report["records"]:
            if rec["category"] != "board_open_work":
                continue
            open_items.append({
                "id": rec["item_id"], "project": rec["project"],
                "title": (rec["item"].get("title") or "")[:80],
                "age_h": round(rec["age"], 1),
            })
        open_items.sort(key=lambda x: (x.get("age_h") or 0), reverse=True)
        reconciliation = {
            "available": True,
            "aged_nonterminal": totals["aged_nonterminal"],
            "categories": {k: totals[k] for k in CATEGORIES},
            "closable_drift": totals["drift_closable"] + totals["stale_drift"],
            "no_id_matches": no_id_details,
            "note": (
                "board_open_work = genuine open work (NOT drift); "
                "drift_closable + stale_drift are the only auto-closable "
                "categories (--apply, exact board-ID authority); "
                "no_id_match candidates are manual-review only."),
        }
        stuck = open_items
        stuck_count = len(open_items)
        stuck_note = (
            "stuck_48h/stuck_count list board_open_work items ONLY since "
            "GAP-049 (genuine open work) — pre-GAP-049 they conflated all "
            "aged non-terminal items; use the reconciliation counts.")
    else:
        reconciliation = None
        stuck = []
        for it in items:
            if it.get("status") in TERMINAL_STATUSES:
                continue
            age = age_hours(it.get("added_at"), now)
            if age is not None and age > STUCK_AGE_HOURS:
                stuck.append({"id": it.get("id"), "project": it.get("project"),
                              "title": (it.get("title") or "")[:80],
                              "age_h": round(age, 1)})
        stuck.sort(key=lambda x: x.get("age_h") or 0, reverse=True)
        stuck_count = len(stuck)
        stuck_note = (
            "reconciler unavailable — legacy conflation: stuck_48h/stuck_count "
            "mix genuine open work with potentially closable drift.")

    try:
        with open(init_path, "r", encoding="utf-8") as fh:
            ini = json.load(fh)
        init_status = [(i.get("id"), i.get("status"), (i.get("last_updated") or "")[:19])
                       for i in ini.get("initiatives", [])]
    except Exception as exc:  # honest degrade, never crash the digest
        init_status = [("ERROR", str(exc), "")]

    bundle = {
        "generated_at": now.isoformat(),
        "ledger_total": len(items),
        "counts": counts,
        "oldest_unverified": oldest[:5],
        "stuck_48h": stuck[:8],
        "stuck_count": stuck_count,
        "stuck_count_legacy_note": stuck_note,
        "current_added": current_added,
        "initiatives": init_status,
        "digest_valid": True,
        # --- GAP-049 digest-facing split (AC4) ---
        "reconciliation_available": reconciliation is not None,
        "reconciliation": reconciliation,
        "drift_closable_count": (reconciliation["categories"]["drift_closable"]
                                 if reconciliation else None),
        "board_open_work_count": (reconciliation["categories"]["board_open_work"]
                                  if reconciliation else None),
        "stale_drift_count": (reconciliation["categories"]["stale_drift"]
                              if reconciliation else None),
        "no_id_match_count": (reconciliation["categories"]["no_id_match"]
                              if reconciliation else None),
    }
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(bundle, fh, indent=1)
    return bundle


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
def main(argv=None, now=None):
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

    # SCHED-GAP-207 (drive-by fix): `now` is the injectable clock seam. The
    # GAP-049 fixture suite builds its world against a FIXED reference instant
    # (NOW = 2026-09-13) but main() read the wall clock directly, so the
    # fixture's deliberately-young item (added_at = NOW - 2h) silently aged
    # past the 48h window once the wall clock reached 2026-09-15 — a test
    # time-bomb that flipped no_id_match_count 6→7 for every run after that
    # date. Production behaviour is unchanged (None = wall clock, as before);
    # the test suite now passes now=NOW and is deterministic again.
    if now is None:
        now = datetime.now(timezone.utc)
    now_iso = now_utc_iso()
    mode = "APPLY" if args.apply else "DRY-RUN"

    print("=" * 78)
    print("ledger_board_reconcile.py  mode={}  (GAP-047 + GAP-049 split)".format(mode))
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

    resolver = ProjectResolver(args.scheduler_db)
    print("db projects   : {} rows{}".format(
        len(resolver.workdirs),
        "" if not resolver.db_error else "  [ERROR: {}]".format(resolver.db_error)))
    print("")

    # ---- classify (single pass; boards loaded once per project) ------------ #
    resolved = {}    # project -> (board_path, source) for the resolution table
    row_counts = {}  # project -> (rows, malformed) for the resolution table

    def loader(project):
        board_path, _workdir, source = resolver.resolve(project)
        resolved[project] = (board_path, source)
        if board_path is None:
            return None
        rows, malformed = read_jsonl_rows(board_path)
        row_counts[project] = (rows, malformed)
        return rows

    report = classify_aged_items(items, loader, now)
    records = report["records"]
    per_project = report["per_project"]
    totals = report["totals"]
    no_id_details = build_no_id_matches(report["no_id_items"], report["rows_cache"])

    # ---- board resolution table ------------------------------------------- #
    print("-" * 78)
    print("BOARD RESOLUTION ({} projects with aged non-terminal ledger items)".format(
        len(resolved)))
    print("-" * 78)
    for project in sorted(resolved):
        board_path, source = resolved[project]
        if board_path is None:
            print("  {:<28} -> NO BOARD   ({})".format(project, source))
        else:
            rows, malformed = row_counts.get(project, ({}, 0))
            print("  {:<28} -> {}  [{}]  rows={} malformed={}".format(
                project, board_path, source, len(rows), malformed))
    print("")

    # ---- classification table --------------------------------------------- #
    print("-" * 78)
    print("CLASSIFICATION (GAP-049: aged non-terminal ledger items vs board truth)")
    print("-" * 78)
    for project in sorted(per_project):
        c = per_project[project]
        print("  {:<28} aged={:<3} open_work={:<3} drift_close={:<3} "
              "stale_drift={:<3} stale_term={:<3} missing={:<3} nomatch={:<3} "
              "ambiguous={:<3} age_unknown={:<3} fuzzy={}".format(
                  project, c["aged_nonterminal"], c["board_open_work"],
                  c["drift_closable"], c["stale_drift"], c["stale_terminal"],
                  c["board_missing"], c["no_id_match"], c["ambiguous"],
                  c["age_unknown"], c["fuzzy"]))
    print("")
    print("TOTALS: " + " ".join(
        "{}={}".format(k, totals[k])
        for k in ("aged_nonterminal",) + CATEGORIES + ("fuzzy",)))
    print("")

    # ---- no_id_match manual-review candidates ----------------------------- #
    print("-" * 78)
    print("NO-ID-MATCH MANUAL-REVIEW CANDIDATES (title similarity — advisory "
          "only, never auto-applied)")
    print("-" * 78)
    if not no_id_details:
        print("  (none)")
    else:
        for project in sorted(no_id_details):
            detail = no_id_details[project]
            print("  {}: {} unmatched item(s) — MANUAL REVIEW ONLY".format(
                project, detail["count"]))
            for entry in detail["items"]:
                print("    ledger item: {}  ({})".format(
                    entry["item_id"], entry["title"]))
                if not entry["candidates"]:
                    print("      (no title-similarity candidates above threshold)")
                for cand in entry["candidates"]:
                    print("      ? board {}  score={:.4f}  [{}]  {}".format(
                        cand["board_id"], cand["score"], cand["board_status"],
                        cand["board_title"]))
    print("")

    # ---- planned changes --------------------------------------------------- #
    planned = [rec for rec in records if rec["category"] in ACTIONABLE_CATEGORIES]
    print("-" * 78)
    print("PLANNED LEDGER CLOSURES (board row 'complete'; drift_closable + "
          "stale_drift only — board-ID authority, never title match)")
    print("-" * 78)
    for rec in planned:
        extra = "" if rec["how"] == "exact" else "  [{}]".format(rec["how"])
        print("  {:<28} {:<20} {} -> verified  [{}]{}".format(
            rec["project"], rec["item_id"], rec["item"].get("status"),
            rec["category"], extra))
    print("")

    changes = len(planned)
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
            board_paths = {p: resolved[p][0] for p in resolved if resolved[p][0]}
            with open(args.ledger, "r", encoding="utf-8") as fh:
                raw_before = fh.read()
            indent, trailing_nl, ensure_ascii = detect_format(raw_before)
            roundtrip = render_ledger(
                doc, indent, trailing_nl, ensure_ascii) == raw_before
            print("format roundtrip (doc unchanged): {} [indent={}, "
                  "trailing_newline={}, ensure_ascii={}]".format(
                      "IDENTICAL" if roundtrip else "MISMATCH",
                      indent, trailing_nl, ensure_ascii))
            for rec in planned:
                item = rec["item"]
                item["status"] = "verified"
                item["last_checked_at"] = now_iso
                item["verification_evidence"] = EVIDENCE_TPL.format(
                    id=rec["board_id"], path=board_paths[rec["project"]])
            backup_path, _fmt = write_ledger_atomic(
                args.ledger, doc, args.backup_suffix)
            applied = True
            print("backup written : {}".format(backup_path))
            print("ledger written : {} (atomic tmp + os.replace, "
                  "format-preserving)".format(args.ledger))
            print("fields touched : status, last_checked_at, verification_evidence")
            print("items closed   : {}".format(changes))
    else:
        print("-" * 78)
        print("DRY-RUN: {} ledger item(s) would be closed; nothing written.".format(
            changes))
        print("Re-run with --apply to write (atomic + backup).")
        print("-" * 78)

    # ---- summary JSON (GAP-049 machine-readable output) -------------------- #
    summary = {
        "schema": "pm-ledger-reconcile-summary/1",
        "mode": mode,
        "generated_at": now_iso,
        "ledger": args.ledger,
        "ledger_items": len(items),
        "aged_nonterminal": totals["aged_nonterminal"],
        "categories": {k: totals[k] for k in CATEGORIES},
        "closable_drift": totals["drift_closable"] + totals["stale_drift"],
        "applied": applied,
        "backup": backup_path,
        "drift_closable_count": totals["drift_closable"],
        "board_open_work_count": totals["board_open_work"],
        "stale_drift_count": totals["stale_drift"],
        "stale_terminal_count": totals["stale_terminal"],
        "board_missing_count": totals["board_missing"],
        "no_id_match_count": totals["no_id_match"],
        "ambiguous_count": totals["ambiguous"],
        "age_unknown_count": totals["age_unknown"],
        "drift_closable_items": [
            {"id": rec["item_id"], "project": rec["project"],
             "from_status": rec["item"].get("status"),
             "board_id": rec["board_id"], "match": rec["how"]}
            for rec in planned if rec["category"] == "drift_closable"],
        "stale_drift_items": [
            {"id": rec["item_id"], "project": rec["project"],
             "from_status": rec["item"].get("status"),
             "board_id": rec["board_id"], "match": rec["how"]}
            for rec in planned if rec["category"] == "stale_drift"],
        "board_open_work_items": [
            {"id": rec["item_id"], "project": rec["project"],
             "board_id": rec["board_id"], "board_status": rec["row"].get("status")}
            for rec in records if rec["category"] == "board_open_work"][:20],
        "no_id_matches": no_id_details,
        # legacy field, honestly relabelled for pre-GAP-049 consumers
        "stuck_48h": [
            {"id": rec["item_id"], "project": rec["project"]}
            for rec in records if rec["category"] == "board_open_work"][:8],
        "stuck_count_legacy_note": (
            "stuck_48h lists board_open_work items ONLY since GAP-049 "
            "(genuine open work) — pre-GAP-049 it conflated all aged "
            "non-terminal items; use the per-category counts instead."),
    }
    print("")
    print("=" * 78)
    print("SUMMARY JSON (machine-readable; GAP-049 split)")
    print("=" * 78)
    print(SUMMARY_JSON_BEGIN)
    print(json.dumps(summary, indent=2, sort_keys=True))
    print(SUMMARY_JSON_END)

    residual_open = (totals["board_open_work"] + totals["stale_terminal"]
                     + totals["age_unknown"])
    print("")
    print("SUMMARY: items_closed={} applied={} residual_open_work={} backup={}".format(
        changes, applied, residual_open, backup_path or "none"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
