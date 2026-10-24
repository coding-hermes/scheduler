#!/usr/bin/env python3
"""fleet-infra-findings.py — classify a finding as fleet-infra vs project-local,
route the fleet-infra ones to ONE shared-infra board, and throttle re-filings
(SCHED-GAP-128).

WHY THIS EXISTS. The qa/dogfood/PM lanes report on the SHARED substrate: the
bunker agent hosts, the port pool, config delivery, the gateway key. Those
conditions are fleet-wide — the same port-pool exhaustion describes every lane
standing on that host — but each lane filed them on the board of whatever
project it happened to be poking. Measured 2026-10-22: 150 rows across 60
board files carried the port-pool / dropped-config / stale-binary vocabulary,
and the fleet's own PM stand-in measured consensus at 236 pending rows of
which 17 were distinct (144 = fleet-wide bunker spam). One fix lands once; the
copies sit open on every other board, and every re-enable audit re-wades
through them.

THE RULE, in three parts:

  1. CLASSIFY before filing. A finding's text is matched against
     ``fleet-infra-patterns.json`` beside this script. An entry is a NAME, a
     regex ``pattern``, and an optional ``requires`` regex. Both must match
     for the finding to be fleet-infra, and the ``requires`` half is
     load-bearing: "stale-binary" alone is a class of PROJECT findings too (a
     repo whose own deployed binary predates HEAD — GAP-056 in hermes-dagger,
     DF-HERMES-CANOPY-32's :8091 route). Requiring a substrate token
     (bunker/bunkerd/las-NN/agent-host/QA-BUNKER) is what separates "the host
     the fleet stands on is stale" from "this project ships a stale binary".
     A classifier without that discriminator would sweep real project work
     into the fleet bucket — a worse failure than the noise it removes.

  2. FILE ONCE. A fleet-infra finding goes to the shared-infra board
     (``bunker``'s — the project that owns the agent hosts) as a
     ``FLEET-INFRA-N`` row carrying ``classification`` and a ``cross_ref``
     naming the project that OBSERVED it, and does NOT go on the observing
     project's board. A second project reporting the same pattern while an
     open ``FLEET-INFRA`` row of that pattern exists is suppressed (the
     cross-reference is appended to that row's ``cross_ref.observations``,
     which is how "17 projects saw it" becomes one row and still counts 17).
     The observing board gets a ``fleet_infra_diverted`` EVENTS row and no
     task row, so the finding stays traceable where it was seen.

  3. THROTTLE the filing. The scheduler already throttles LANE-health alerts
     for 30 minutes on a per-project key (``starvationThrottleWindow``,
     SCHED-GAP-014; ``consecutiveFailureThrottleWindow``, SCHED-GAP-137c) and
     reads the last emission back out of the events table so the memory
     survives the per-pass escalator. Board filings had no equivalent: a lane
     that runs twice inside a tick window re-files the identical condition.
     This module keeps the same 30-minute window on the same key shape, with
     the pattern appended so port-pool and stale-binary about one host keep
     independent cadences — ``(project, pattern)``, NUL-separated so the
     halves can never collide. The memory is a JSON ledger read back on every
     call (durable across processes, exactly like the events-table read-back)
     written under an exclusive ``flock`` so two lanes filing at once cannot
     lose a stamp.

FAIL-SAFE, NOT FAIL-CLOSED. Every helper degrades to the pre-existing
behaviour rather than dropping work: an unreadable pattern file classifies
everything ``project-local``; an unresolvable infra board returns
``route=local``; a throttle-store IO error allows the filing. A routing fault
must never lose a finding.

CLI
  python3 ops/fleet-infra-findings.py classify "bunker port-pool exhausted on consensus"
  # {"label": "fleet-infra:port_pool", ...}
  python3 ops/fleet-infra-findings.py classify --list
  python3 ops/fleet-infra-findings.py file --board <tasks.jsonl> --project consensus \\
      --title "run_battery FAIL — bunkerd port-pool exhaustion on bunker-las-03" --apply
  python3 ops/fleet-infra-findings.py stats                 # fleet census
  python3 ops/fleet-infra-findings.py throttle --clear      # drop the ledger

Stdlib only (python3 >= 3.8); the collapse tool
(``ops/collapse-fleet-infra-findings.py``) imports this module by path.
"""
from __future__ import annotations

import fcntl
import json
import os
import re
import sys
import time
from typing import Any

HERE = os.path.dirname(os.path.abspath(__file__))
PATTERNS_FILENAME = "fleet-infra-patterns.json"
PATTERNS_ENV = "FLEET_INFRA_PATTERNS"

FLEET = "fleet-infra"
LOCAL = "project-local"
LABEL_SEP = ":"

#: The shared-infra board's project. The board is resolved from the scheduler
#: DB's registered workdir (never a hardcoded path) — see resolve_infra_board.
INFRA_PROJECT = "bunker"
INFRA_ID_PREFIX = "FLEET-INFRA"
#: Row ``origin`` values this module writes. The gate accepts a row as
#: fleet-infra when it carries ``classification: fleet-infra:<pattern>`` (both
#: writers stamp it); ``origin`` is the human-readable provenance.
ORIGIN_ROUTED = "fleet-infra-classifier"
ORIGIN_CONSOLIDATED = "fleet-infra-collapse"

#: 30 minutes — the same window (and the same per-project key shape) the
#: scheduler's lane-health throttles use (starvationThrottleWindow /
#: consecutiveFailureThrottleWindow, internal/scheduler/alert_escalation.go).
DEFAULT_THROTTLE_S = 1800
THROTTLE_ENV = "FLEET_INFRA_THROTTLE_S"
THROTTLE_STATE_ENV = "FLEET_INFRA_THROTTLE_STATE"
DEFAULT_THROTTLE_STATE = "~/.hermes/coding-hermes/fleet-infra-throttle.json"

#: Statuses that mean "open" to every reader in this fleet
#: (board_freshness.go openStatuses / adaptive_cooldown.go boardOpenRows).
OPEN_STATUSES = ("pending", "in_progress")

_PATTERN_CACHE: dict[str, tuple[dict[str, Any], ...]] = {}


# ---------------------------------------------------------------------------
# patterns + classification
# ---------------------------------------------------------------------------
def patterns_path(path: str | None = None) -> str:
    """Where the pattern table lives (explicit > $FLEET_INFRA_PATTERNS > beside us)."""
    return path or os.environ.get(PATTERNS_ENV) or os.path.join(HERE, PATTERNS_FILENAME)


def load_patterns(path: str | None = None) -> tuple[dict[str, Any], ...]:
    """Compiled pattern entries, in file order. Never raises.

    Each entry keeps its raw ``name`` plus compiled ``pattern_re`` /
    ``requires_re`` (``None`` when the entry demands no co-signal). A
    malformed entry is skipped rather than poisoning the table; an
    unreadable or unparseable FILE yields ``()`` — everything then classifies
    project-local, which is the pre-SCHED-GAP-128 filing behaviour. The
    degradation is reported on stderr, never as a finding.
    """
    p = patterns_path(path)
    cached = _PATTERN_CACHE.get(p)
    if cached is not None:
        return cached
    try:
        with open(p, "r", encoding="utf-8") as fh:
            raw = json.load(fh)
    except (OSError, ValueError) as exc:
        print("fleet-infra-findings: warning: cannot load %s (%s) — every finding "
              "classifies project-local" % (p, exc), file=sys.stderr)
        raw = []
    if isinstance(raw, dict):
        raw = raw.get("patterns")
    entries: list[dict[str, Any]] = []
    for item in raw if isinstance(raw, list) else []:
        if not isinstance(item, dict):
            continue
        name = str(item.get("name") or "").strip()
        pat = item.get("pattern")
        if not name or not isinstance(pat, str) or not pat.strip():
            continue
        try:
            pattern_re = re.compile(pat, re.IGNORECASE)
        except re.error:
            pattern_re = re.compile(re.escape(pat), re.IGNORECASE)
        requires = item.get("requires")
        requires_re = None
        if isinstance(requires, str) and requires.strip():
            try:
                requires_re = re.compile(requires, re.IGNORECASE)
            except re.error:
                requires_re = re.compile(re.escape(requires), re.IGNORECASE)
        entries.append({
            "name": name,
            "pattern": pat,
            "pattern_re": pattern_re,
            "requires": requires,
            "requires_re": requires_re,
            "why": str(item.get("why") or ""),
        })
    out = tuple(entries)
    _PATTERN_CACHE[p] = out
    return out


def classify(text: str, path: str | None = None) -> dict[str, Any]:
    """Label one finding.

    Returns ``{"label", "pattern", "route", "matched"}`` where ``label`` is
    ``"fleet-infra:<name>"`` or ``"project-local"`` and ``route`` is
    ``"infra"`` or ``"local"``. Empty/whitespace text is project-local:
    absence of a finding is never an infra finding.

    First entry in file order wins, so the answer is deterministic and the
    table's order is meaningful (put the specific classes before broad ones).
    """
    text = str(text or "")
    if not text.strip():
        return {"label": LOCAL, "pattern": "", "route": "local", "matched": ""}
    for entry in load_patterns(path):
        m = entry["pattern_re"].search(text)
        if not m:
            continue
        req = entry["requires_re"]
        if req is not None and not req.search(text):
            continue
        return {
            "label": FLEET + LABEL_SEP + entry["name"],
            "pattern": entry["name"],
            "route": "infra",
            "matched": m.group(0),
        }
    return {"label": LOCAL, "pattern": "", "route": "local", "matched": ""}


def load_excludes(path: str | None = None) -> tuple[re.Pattern, ...]:
    """Compiled ``exclude`` regexes — titles that NAME the class to fix it.

    The scheduler's own SCHED-GAP-128 row says "fleet-wide infra alert spam …
    port-pool / dropped-config / stale-binary" in its title because it is the
    WORK ITEM that removes the spam. Collapsing it would delete the tracking
    row; counting it as a duplicate would make the gate fire on its own fix.
    Excluded titles are therefore neither collapsed nor counted.
    """
    p = patterns_path(path)
    key = p + "\x00exclude"
    cached = _PATTERN_CACHE.get(key)
    if cached is not None:
        return cached  # type: ignore[return-value]
    try:
        with open(p, "r", encoding="utf-8") as fh:
            raw = json.load(fh)
    except (OSError, ValueError):
        raw = {}
    items = raw.get("exclude") if isinstance(raw, dict) else None
    compiled: list[re.Pattern] = []
    for item in items if isinstance(items, list) else []:
        if not isinstance(item, str) or not item.strip():
            continue
        try:
            compiled.append(re.compile(item, re.IGNORECASE))
        except re.error:
            compiled.append(re.compile(re.escape(item), re.IGNORECASE))
    out = tuple(compiled)
    _PATTERN_CACHE[key] = out  # type: ignore[assignment]
    return out


def is_excluded(row: dict[str, Any], path: str | None = None) -> bool:
    """True when a row's title marks it as the fix for this class, not a member."""
    title = str(row.get("title") or "")
    return any(rx.search(title) for rx in load_excludes(path))


def classify_row(row: dict[str, Any], path: str | None = None) -> dict[str, Any]:
    """Classify a BOARD ROW: pattern from the TITLE, co-signal from the body.

    This asymmetry is the whole point. A row's title is where it names its own
    finding; the body is prose that also mentions other findings ("the five P1
    rows are all port-pool exhaustion…"). Matching the pattern against the
    body classified DOC-4 (a docs/env-table row whose reasoning cites a config
    drop) and TR-238 (a bunker-provisioning row whose body quotes a gateway
    error) as fleet-infra — measured 2026-10-22 on the live fleet, before this
    rule. So: the PATTERN must appear in the title, while the required
    substrate token may come from title+body (a title like "Pool-exhaustion
    class recurs" carries the pattern and names /run/bunker only in its
    detail).

    Falls back to the stamped ``classification`` when the row has one, so a
    row already routed keeps its class even if the pattern table changes.
    """
    stamped = pattern_of(str(row.get("classification") or ""))
    if is_excluded(row, path):
        return {"label": LOCAL, "pattern": "", "route": "local", "matched": ""}
    if stamped:
        return {"label": FLEET + LABEL_SEP + stamped, "pattern": stamped,
                "route": "infra", "matched": ""}
    title = str(row.get("title") or "")
    # The co-signal is read from the TITLE and DETAIL only. `reasoning` /
    # `review_notes` / `foreman_note` are the chatty audit fields where a lane
    # narrates the whole fleet ("…the five P1 rows are all bunker-las-03
    # port-pool…"), and reading them re-admits the rows this rule exists to
    # exclude (measured: warpfs DF-WARPFS-98/99, whose own cargo binary is
    # stale, name the bunker fleet only in their dogfood prose).
    co_text = title + "\n" + str(row.get("detail") or "")
    for entry in load_patterns(path):
        m = entry["pattern_re"].search(title)
        if not m:
            continue
        req = entry["requires_re"]
        if req is not None and not req.search(co_text):
            continue
        return {"label": FLEET + LABEL_SEP + entry["name"],
                "pattern": entry["name"], "route": "infra", "matched": m.group(0)}
    return {"label": LOCAL, "pattern": "", "route": "local", "matched": ""}


def is_infra_carrier(row: dict[str, Any]) -> bool:
    """True when a row is a fleet-infra CARRIER this system owns.

    Carriers are rows stamped ``classification: fleet-infra:<pattern>`` or
    written by this module's origins. The shared-infra board also carries its
    own native rows that merely TALK about the substrate (NET-BUNKER-009); a
    collapse must extend a carrier, never repurpose a project's own finding.
    """
    if pattern_of(str(row.get("classification") or "")):
        return True
    return str(row.get("origin") or "") in (ORIGIN_ROUTED, ORIGIN_CONSOLIDATED)


def pattern_of(label: str) -> str:
    """The pattern name inside a ``fleet-infra:<name>`` label, else ``""``."""
    label = str(label or "")
    if label.startswith(FLEET + LABEL_SEP):
        return label[len(FLEET) + len(LABEL_SEP):]
    return ""


def row_text(row: dict[str, Any]) -> str:
    """The text a row is classified on: title + the prose fields that carry detail.

    Every caller must use THIS function. A qa finding's symptom often lives in
    ``detail``/``reasoning`` while the title is a paraphrase ("Pool-exhaustion
    class recurs…" names /run/bunker in the detail) — classifying the title
    alone loses exactly the rows the collapse exists to find.
    """
    parts = []
    for key in ("title", "detail", "reasoning", "review_notes", "foreman_note"):
        val = row.get(key)
        if isinstance(val, str) and val:
            parts.append(val)
    cr = row.get("cross_ref")
    if isinstance(cr, dict):
        parts.append(json.dumps(cr, sort_keys=True))
    return "\n".join(parts)


def row_pattern(row: dict[str, Any], path: str | None = None) -> str:
    """Pattern a row belongs to, or ``""`` for a project-local row.

    Uses :func:`classify_row`'s title/body asymmetry — never a bare body scan,
    which turned every row that merely MENTIONED a fleet term into a candidate.
    """
    return classify_row(row, path)["pattern"]


# ---------------------------------------------------------------------------
# board IO
# ---------------------------------------------------------------------------
def read_board(path: str) -> list[dict[str, Any]]:
    """Every parseable dict row of a JSONL board. Missing file = ``[]``.

    Unparseable lines are skipped, never raised: an append-only board in a live
    fleet carries legacy/garbled lines and a reader that dies on them cannot
    be used inside a filing tick.
    """
    rows: list[dict[str, Any]] = []
    try:
        fh = open(path, "r", encoding="utf-8", errors="replace")
    except FileNotFoundError:
        return rows
    except OSError:
        return rows
    with fh:
        for line in fh:
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if isinstance(row, dict):
                rows.append(row)
    return rows


def read_board_lines(path: str) -> list[str]:
    """The board's PHYSICAL lines (newline stripped). Used by rewriters."""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            return [ln.rstrip("\n") for ln in fh]
    except FileNotFoundError:
        return []


def append_jsonl(path: str, rows: list[dict[str, Any]]) -> int:
    """Append rows with the one-object-per-line invariant. Returns rows written.

    O_APPEND only: never "w"/"wb" and never a shell redirect — the shape that
    truncated a board to zero bytes and had the git tail commit the deletion
    (QA-TASK-ROUTER-7). The existing tail is newline-terminated first so a new
    row can never fuse onto an unterminated last line (DAGGER-0943).
    """
    if not rows:
        return 0
    body = "".join(json.dumps(r, ensure_ascii=False, sort_keys=False) + "\n" for r in rows)
    needs_nl = False
    if os.path.exists(path):
        try:
            with open(path, "rb") as fh:
                fh.seek(-1, os.SEEK_END) if os.path.getsize(path) else None
                needs_nl = bool(os.path.getsize(path)) and fh.read(1) != b"\n"
        except OSError:
            needs_nl = False
    payload = (("\n" if needs_nl else "") + body).encode("utf-8")
    with open(path, "ab") as fh:
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    return len(rows)


def file_signature(path: str) -> tuple[int, int] | None:
    """``(size, mtime_ns)`` — a cheap "did this file move under me?" signature."""
    try:
        st = os.stat(path)
    except OSError:
        return None
    return (st.st_size, st.st_mtime_ns)


class BoardChangedError(RuntimeError):
    """The board changed between the read and the rewrite — nothing was written."""


def write_board_lines(path: str, lines: list[str], backup_dir: str | None = None,
                      expect: tuple[int, int] | None = None,
                      make_backup: bool = True) -> str:
    """Atomically replace a board with ``lines``; returns the backup path.

    A full rewrite (unlike an append) is only legitimate for the ONE caller
    that owns a collapse: it takes a byte copy of the board FIRST, writes a
    temp file in the same directory and renames it, so a crash mid-write can
    never leave a half board behind. ``backup_dir`` (default: beside the board,
    ``<board>.bak-<epoch>``) is the restore point.

    ``expect`` is the lost-update guard for a LIVE board: the caller passes the
    :func:`file_signature` it read, and a board that changed since (a lane
    appended a row while the migration was classifying) raises
    :class:`BoardChangedError` with the file untouched. Without it a rewrite
    silently discards whatever a concurrent lane appended in between.
    """
    data = "".join(ln + "\n" for ln in lines)
    for ln in lines:
        if "\n" in ln:
            raise ValueError("board lines must not contain newlines")
    if expect is not None and file_signature(path) != expect:
        raise BoardChangedError(
            "%s changed during the run (expected signature %r, found %r) — "
            "nothing written; re-run to pick up the new rows"
            % (path, expect, file_signature(path)))
    backup = None
    if make_backup and os.path.exists(path):
        stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        target_dir = backup_dir or os.path.dirname(path)
        if target_dir:
            os.makedirs(target_dir, exist_ok=True)
        backup = os.path.join(target_dir,
                              "%s.bak-%s" % (os.path.basename(path), stamp))
        with open(path, "rb") as src, open(backup, "wb") as dst:
            dst.write(src.read())
    tmp = path + ".tmp.%d" % os.getpid()
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)
    return backup or ""


def next_event_id(events_path: str) -> int:
    """Next events id on the board's 19-digit epoch-nanosecond scale.

    Derived from the file's own max integer id (never from an epoch basis),
    and floored at ``10**18`` so a board with no ids yet still starts on the
    scale the gate asserts (SCHED-GAP-206 / event-id-ascending).
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
                if max_id is None or eid > max_id:
                    max_id = eid
    except (FileNotFoundError, OSError):
        pass
    if max_id is None or max_id < 10 ** 18:
        return max(10 ** 18, int(time.time() * 1e9))
    return max_id + 1


def events_path_for(board_path: str) -> str:
    """The events log beside a board (the fleet's one layout)."""
    return os.path.join(os.path.dirname(board_path), "events.jsonl")


# ---------------------------------------------------------------------------
# infra board resolution + ids
# ---------------------------------------------------------------------------
def scheduler_db_path() -> str:
    return (os.environ.get("SCHEDULER_DB")
            or os.path.expanduser("~/.hermes/coding-hermes/scheduler.db"))


def project_workdirs(name: str, db_path: str | None = None) -> list[str]:
    """Registered workdirs for a project, best-first. Read-only; never raises.

    ``<name>`` then ``<name>-foreman`` (the primary lane that owns the repo
    board) then any lane whose workdir basename is ``name``.
    """
    import sqlite3

    try:
        con = sqlite3.connect("file:%s?mode=ro" % (db_path or scheduler_db_path()),
                              uri=True, timeout=2.0)
        try:
            rows = con.execute("SELECT name, workdir FROM projects "
                               "WHERE workdir IS NOT NULL AND workdir <> ''").fetchall()
        finally:
            con.close()
    except Exception:
        return []
    by_name = {str(n): str(w) for n, w in rows}
    out: list[str] = []
    for key in (name, name + "-foreman"):
        wd = by_name.get(key)
        if wd and wd not in out:
            out.append(wd)
    for _n, wd in rows:
        wd = str(wd)
        if os.path.basename(wd.rstrip("/")) == name and wd not in out:
            out.append(wd)
    return out


def board_in(workdir: str) -> str | None:
    """``<workdir>/.coding-hermes/board/tasks.jsonl`` when it exists."""
    cand = os.path.join(workdir or "", ".coding-hermes", "board", "tasks.jsonl")
    return cand if os.path.isfile(cand) else None


def resolve_infra_board(explicit: str | None = None, home: str | None = None,
                        project: str = INFRA_PROJECT) -> str | None:
    """The shared-infra board path, or ``None``.

    Order: explicit argv → ``$FLEET_INFRA_BOARD`` → ``<home>/<project>`` (the
    test seam: a fixture home can never resolve the live board) → every
    workdir the scheduler DB registers for the project. The DB is consulted
    only when *home* is the real home, so tests never reach it.
    """
    for cand in (explicit, os.environ.get("FLEET_INFRA_BOARD")):
        if cand:
            path = os.path.abspath(os.path.expanduser(cand))
            return path if os.path.isfile(path) else None
    home = home or os.path.expanduser("~")
    found = board_in(os.path.join(home, project))
    if found:
        return found
    try:
        if os.path.abspath(home) == os.path.abspath(os.path.expanduser("~")):
            for wd in project_workdirs(project):
                found = board_in(wd)
                if found:
                    return found
    except Exception:
        return None
    return None


def mint_infra_id(taken: set[str] | list[str]) -> str:
    """Next free ``FLEET-INFRA-N``: past the highest suffix EVER used."""
    rx = re.compile(r"^%s-(\d+)(?:-[rR]\d+)?$" % re.escape(INFRA_ID_PREFIX))
    maxn = 0
    for rid in taken:
        m = rx.match(str(rid))
        if m:
            maxn = max(maxn, int(m.group(1)))
    n = maxn
    while True:
        n += 1
        cand = "%s-%d" % (INFRA_ID_PREFIX, n)
        if cand not in taken:
            return cand


# ---------------------------------------------------------------------------
# board-filing throttle — the 30-minute key
# ---------------------------------------------------------------------------
def throttle_window_s() -> int:
    """Window length: ``$FLEET_INFRA_THROTTLE_S`` else 1800 (30 min)."""
    raw = os.environ.get(THROTTLE_ENV)
    if raw:
        try:
            val = int(float(raw))
            if val > 0:
                return val
        except ValueError:
            print("fleet-infra-findings: warning: %s=%r is not a positive number — "
                  "using %ds" % (THROTTLE_ENV, raw, DEFAULT_THROTTLE_S), file=sys.stderr)
    return DEFAULT_THROTTLE_S


def throttle_state_path(path: str | None = None) -> str:
    """Ledger location (explicit > ``$FLEET_INFRA_THROTTLE_STATE`` > default)."""
    return os.path.abspath(os.path.expanduser(
        path or os.environ.get(THROTTLE_STATE_ENV) or DEFAULT_THROTTLE_STATE))


def throttle_key(project: str, pattern: str) -> str:
    """``(project, pattern)`` NUL-joined — the scheduler's key shape, + pattern.

    NUL cannot occur in either half, so ("a", "b_c") and ("a_b", "c") can
    never collide. The pattern is part of the key on purpose: port-pool
    exhaustion and a stale binary about the same host are DIFFERENT findings
    and each deserves its own 30-minute cadence.
    """
    return "%s\x00%s" % (project, pattern)


class BoardFilingThrottle:
    """Durable per-``(project, pattern)`` filing throttle.

    The window mirrors the scheduler's lane-health throttles (30 min, keyed on
    the project, memory read back so it survives a fresh process). Here the
    memory is a small JSON ledger guarded by ``flock``: a filing tick reads it,
    decides, stamps it, and writes it back atomically. IO failure anywhere
    degrades to "allow the filing" — a broken throttle must never swallow a
    finding.
    """

    def __init__(self, path: str | None = None, window_s: int | None = None):
        self.path = throttle_state_path(path)
        self.window_s = window_s if window_s is not None else throttle_window_s()

    # -- raw ledger ---------------------------------------------------------
    def _load(self) -> dict[str, Any]:
        try:
            with open(self.path, "r", encoding="utf-8") as fh:
                data = json.load(fh)
            if isinstance(data, dict) and isinstance(data.get("entries"), dict):
                return data
        except (OSError, ValueError):
            pass
        return {"version": 1, "window_s": self.window_s, "entries": {}}

    def _store(self, data: dict[str, Any]) -> None:
        directory = os.path.dirname(self.path)
        if directory:
            os.makedirs(directory, exist_ok=True)
        tmp = "%s.tmp.%d" % (self.path, os.getpid())
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=1, sort_keys=True)
            fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, self.path)

    # -- decisions ----------------------------------------------------------
    def check(self, project: str, pattern: str, now: float | None = None) -> dict[str, Any]:
        """``{"allowed", "remaining_s", "last_ts", "last_row"}`` for the key."""
        now = time.time() if now is None else now
        entry = self._load()["entries"].get(throttle_key(project, pattern))
        if not isinstance(entry, dict):
            return {"allowed": True, "remaining_s": 0, "last_ts": None, "last_row": None}
        try:
            last = float(entry.get("ts"))
        except (TypeError, ValueError):
            return {"allowed": True, "remaining_s": 0, "last_ts": None, "last_row": None}
        elapsed = now - last
        if elapsed >= self.window_s:
            return {"allowed": True, "remaining_s": 0, "last_ts": last,
                    "last_row": entry.get("row_id")}
        return {"allowed": False, "remaining_s": int(round(self.window_s - elapsed)),
                "last_ts": last, "last_row": entry.get("row_id")}

    def stamp(self, project: str, pattern: str, row_id: str,
              now: float | None = None) -> None:
        """Record a filing for the key. ``flock``-serialised; atomic replace."""
        now = time.time() if now is None else now
        directory = os.path.dirname(self.path) or "."
        os.makedirs(directory, exist_ok=True)
        lock_path = self.path + ".lock"
        with open(lock_path, "a+") as lock_fh:
            fcntl.flock(lock_fh.fileno(), fcntl.LOCK_EX)
            try:
                data = self._load()
                data["window_s"] = self.window_s
                data["entries"][throttle_key(project, pattern)] = {
                    "project": str(project), "pattern": str(pattern),
                    "row_id": str(row_id), "ts": now,
                    "iso": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
                }
                self._store(data)
            finally:
                fcntl.flock(lock_fh.fileno(), fcntl.LOCK_UN)

    def clear(self, key: str | None = None) -> int:
        """Drop one key (``project:pattern``) or the whole ledger. Returns removed."""
        data = self._load()
        if key is None:
            removed = len(data["entries"])
            data["entries"] = {}
        else:
            project, _, pattern = key.partition(":")
            removed = 1 if data["entries"].pop(throttle_key(project, pattern), None) else 0
        self._store(data)
        return removed


# ---------------------------------------------------------------------------
# filing one fleet-infra finding on the shared board
# ---------------------------------------------------------------------------
def now_iso(now: float | None = None) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() if now is None else now))


def file_finding(title: str, project: str, infra_board: str | None = None,
                 detail: str = "", priority: str = "P2", source: str = "qa",
                 board: str | None = None, apply: bool = True,
                 throttle: BoardFilingThrottle | None = None,
                 now: float | None = None,
                 patterns_path: str | None = None) -> dict[str, Any]:
    """Classify one finding and, when it is fleet-infra, file it ONCE.

    Returns a decision dict:

      ``route``       "local" | "infra"
      ``label``       "fleet-infra:<pattern>" | "project-local"
      ``action``      "passthrough" (local — the caller files it on its own
                      board unchanged), "filed" (new FLEET-INFRA row),
                      "observed" (appended to an open row's cross_ref),
                      "throttled" (same project+pattern inside 30 min), or
                      "unroutable" (no infra board — caller files locally)
      ``row_id``      the FLEET-INFRA row it landed on (when applicable)
      ``reason``      human-readable one-liner for logs/evidence

    The caller keeps ownership of its own board: a ``passthrough`` /
    ``unroutable`` decision means "file it yourself, as before".
    """
    now = time.time() if now is None else now
    cls = classify("%s\n%s" % (title or "", detail or ""), patterns_path)
    decision: dict[str, Any] = {
        "route": cls["route"], "label": cls["label"], "pattern": cls["pattern"],
        "action": "passthrough", "row_id": "", "reason": "",
        "project": str(project), "title": str(title or "")[:160],
    }
    if cls["route"] != "infra":
        decision["reason"] = "project-local finding — filed on %s as before" % project
        return decision

    pattern = cls["pattern"]
    # An EXPLICIT board must still resolve to a real file: a stale path would
    # otherwise be created by the first append and the finding would vanish
    # into a file nobody reads.
    infra_board = resolve_infra_board(infra_board) if infra_board else resolve_infra_board()
    if not infra_board:
        decision["action"] = "unroutable"
        decision["route"] = "local"
        decision["reason"] = ("no shared-infra board resolvable — filing on %s "
                              "so the finding is not lost" % project)
        return decision

    thr = throttle or BoardFilingThrottle()
    gate = thr.check(project, pattern, now=now)
    if not gate["allowed"]:
        decision["action"] = "throttled"
        decision["row_id"] = str(gate.get("last_row") or "")
        decision["reason"] = ("%s/%s filed %ds ago (row %s); 30-minute filing "
                              "window still open" % (project, pattern,
                                                     int(now - (gate["last_ts"] or now)),
                                                     gate.get("last_row") or "?"))
        return decision

    rows = read_board(infra_board)
    taken = {str(r.get("id", "")) for r in rows}
    existing = [r for r in rows
                if str(r.get("status", "")).lower() in OPEN_STATUSES
                and row_pattern(r, patterns_path) == pattern]
    if existing:
        target = existing[0]
        row_id = str(target.get("id", ""))
        decision.update(action="observed", row_id=row_id,
                        reason="open %s row %s already carries this pattern — "
                               "cross-reference appended, no new row" % (pattern, row_id))
        if apply:
            _append_observation(infra_board, target, project, title, detail, source, now)
            thr.stamp(project, pattern, row_id, now=now)
            _annotate_source(board, project, title, row_id, cls["label"], source, now)
        return decision

    row_id = mint_infra_id(taken)
    decision.update(action="filed", row_id=row_id,
                    reason="new shared-infra row %s for pattern %s (observed by %s)"
                           % (row_id, pattern, project))
    if not apply:
        return decision

    row = {
        "id": row_id,
        "title": "[fleet-infra:%s] %s" % (pattern, str(title or "")[:150]),
        "status": "pending",
        "priority": priority,
        "origin": ORIGIN_ROUTED,
        "classification": cls["label"],
        "created_at": now_iso(now),
        "reasoning": ("Shared-substrate finding (%s) observed while poking %s "
                      "(%s). Filed here ONCE instead of on that project's board: "
                      "one fix, one row (SCHED-GAP-128)." % (pattern, project, source)),
        "cross_ref": {
            "pattern": pattern,
            "observations": [{"project": str(project), "title": str(title or "")[:160],
                              "detail": str(detail or "")[:400], "source": source,
                              "observed_at": now_iso(now)}],
        },
    }
    append_jsonl(infra_board, [row])
    events = events_path_for(infra_board)
    if os.path.isdir(os.path.dirname(events)):
        try:
            append_jsonl(events, [{
                "id": next_event_id(events), "kind": "task_added", "task_id": row_id,
                "source": ORIGIN_ROUTED,
                "detail": "fleet-infra:%s observed by %s" % (pattern, project),
                "ts": now_iso(now),
            }])
        except OSError:
            pass
    thr.stamp(project, pattern, row_id, now=now)
    _annotate_source(board, project, title, row_id, cls["label"], source, now)
    return decision


def _append_observation(infra_board: str, target: dict[str, Any], project: str,
                        title: str, detail: str, source: str, now: float) -> None:
    """Append this project's observation to the row's ``cross_ref`` in place.

    The row is rewritten as ONE physical line, leaving every other line of the
    board byte-identical: a cross-reference is bookkeeping on an existing
    finding, not a new finding, so it must not mint a row.
    """
    row_id = str(target.get("id", ""))
    lines = read_board_lines(infra_board)
    if not lines:
        return
    updated = None
    for idx, line in enumerate(lines):
        if not line.strip().startswith("{"):
            continue
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if not isinstance(row, dict) or str(row.get("id", "")) != row_id:
            continue
        cr = row.get("cross_ref")
        if not isinstance(cr, dict):
            cr = {"pattern": pattern_of(str(row.get("classification") or ""))}
            row["cross_ref"] = cr
        obs = cr.get("observations")
        if not isinstance(obs, list):
            obs = []
            cr["observations"] = obs
        obs.append({"project": str(project), "title": str(title or "")[:160],
                    "detail": str(detail or "")[:400], "source": source,
                    "observed_at": now_iso(now)})
        updated = idx
        lines[idx] = json.dumps(row, ensure_ascii=False)
        break
    if updated is None:
        return
    backup_dir = os.path.join(os.path.expanduser("~"), ".hermes", "coding-hermes",
                              "fleet-infra-backups")
    try:
        os.makedirs(backup_dir, exist_ok=True)
    except OSError:
        backup_dir = None
    write_board_lines(infra_board, lines, backup_dir=backup_dir)


def _annotate_source(board: str | None, project: str, title: str, row_id: str,
                     label: str, source: str, now: float) -> None:
    """Leave a ``fleet_infra_diverted`` EVENTS row on the observing board."""
    if not board:
        return
    events = events_path_for(board)
    if not os.path.isdir(os.path.dirname(events)):
        return
    try:
        append_jsonl(events, [{
            "id": next_event_id(events), "kind": "fleet_infra_diverted",
            "source": ORIGIN_ROUTED, "project": str(project),
            "title": str(title or "")[:160],
            "detail": "filed on the shared-infra board as %s (%s), not on this "
                      "project's board (SCHED-GAP-128)" % (row_id, label),
            "ts": now_iso(now),
        }])
    except OSError:
        pass


# ---------------------------------------------------------------------------
# discovery (used by stats + the collapse tool)
# ---------------------------------------------------------------------------
def is_linked_worktree(path: str) -> bool:
    """True when *path* is a git WORKTREE checkout, not a project's own repo.

    A worktree's ``.git`` is a FILE holding ``gitdir: ...``; the main checkout's
    is a directory. Board copies inside worktrees are the same rows as their
    owner repo — sweeping them would edit another agent's in-flight checkout
    and would count one finding many times.
    """
    dotgit = os.path.join(path, ".git")
    if not os.path.isfile(dotgit):
        return False
    try:
        with open(dotgit, "r", encoding="utf-8", errors="replace") as fh:
            return fh.read(200).lstrip().startswith("gitdir:")
    except OSError:
        return False


def discover_boards(home: str | None = None, include_worktrees: bool = False,
                    db_path: str | None = None,
                    include_unregistered: bool = False) -> list[str]:
    """Every PROJECT board in the fleet, deduplicated by real path.

    The authoritative source is the scheduler DB's registered workdirs: a
    project is a fleet member because the scheduler says so, not because a
    directory happens to look like one. A ``<home>/*`` scan (opt-in,
    *include_unregistered*) is for the on-boarding case — it also drags in
    scratch clones and frozen QA copies (``qa9-frozen-before``) whose boards
    are byte copies of an owner repo's, and editing those is both unsafe and
    meaningless for this migration.

    Linked git worktrees are excluded for the same reason (their boards are
    copies of their owner repo's board): ``.git`` as a FILE with a
    ``gitdir:`` line is the worktree marker.
    """
    home = home or os.path.expanduser("~")
    found: list[str] = []
    seen: set[str] = set()

    def add(path: str | None) -> None:
        if not path:
            return
        real = os.path.realpath(path)
        if real in seen:
            return
        seen.add(real)
        found.append(path)

    try:
        import sqlite3

        con = sqlite3.connect("file:%s?mode=ro" % (db_path or scheduler_db_path()),
                              uri=True, timeout=2.0)
        try:
            for (wd,) in con.execute("SELECT workdir FROM projects "
                                     "WHERE workdir IS NOT NULL AND workdir <> ''").fetchall():
                wd = str(wd)
                if not include_worktrees and is_linked_worktree(wd):
                    continue
                add(board_in(wd))
        finally:
            con.close()
    except Exception:
        pass
    if include_unregistered:
        import glob
        for path in sorted(glob.glob(os.path.join(home, "*", ".coding-hermes", "board",
                                                 "tasks.jsonl"))):
            wd = os.path.dirname(os.path.dirname(os.path.dirname(path)))
            if not include_worktrees and is_linked_worktree(wd):
                continue
            add(path)
    return found


def census(boards: list[str] | None = None, include_worktrees: bool = False) -> dict[str, Any]:
    """Fleet-wide census: matching rows per board and per pattern, plus locals."""
    boards = boards if boards is not None else discover_boards(
        include_worktrees=include_worktrees)
    per_board: list[dict[str, Any]] = []
    by_pattern: dict[str, int] = {}
    local_rows = 0
    infra_rows = 0
    for board in boards:
        hits = []
        for row in read_board(board):
            pattern = row_pattern(row)
            if pattern:
                hits.append(row.get("id"))
                by_pattern[pattern] = by_pattern.get(pattern, 0) + 1
                infra_rows += 1
            elif classify(row_text(row))["matched"]:
                hits.append(row.get("id"))
                local_rows += 1
        if hits:
            per_board.append({"board": board, "rows": hits})
    return {
        "boards_scanned": len(boards),
        "boards_with_hits": len(per_board),
        "infra_rows": infra_rows,
        "local_rows_mentioning": local_rows,
        "by_pattern": by_pattern,
        "detail": per_board,
    }


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
def _print_json(obj: Any) -> None:
    print(json.dumps(obj, indent=1, sort_keys=False))


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__ or "")
        return 0
    cmd, rest = argv[0], argv[1:]
    if cmd == "classify":
        if "--list" in rest:
            for entry in load_patterns():
                print("%-14s %s%s" % (entry["name"], entry["pattern"],
                                      ("  [requires: %s]" % entry["requires"])
                                      if entry["requires"] else ""))
            return 0
        text = " ".join(rest) if rest else sys.stdin.read()
        _print_json(classify(text))
        return 0
    if cmd == "file":
        opts = _parse_opts(rest, {"--board", "--project", "--title", "--detail",
                                  "--priority", "--source", "--infra-board"})
        if not opts.get("--project") or not opts.get("--title"):
            print("usage: file --board <tasks.jsonl> --project <name> --title <text> "
                  "[--detail D] [--priority P] [--apply]", file=sys.stderr)
            return 1
        decision = file_finding(
            title=opts["--title"], project=opts["--project"],
            infra_board=opts.get("--infra-board"), detail=opts.get("--detail", ""),
            priority=opts.get("--priority", "P2"), source=opts.get("--source", "cli"),
            board=opts.get("--board"), apply="--apply" in rest,
        )
        _print_json(decision)
        return 0
    if cmd == "stats":
        opts = _parse_opts(rest, {"--home"})
        boards = (rest[rest.index("--boards") + 1].split(",")
                  if "--boards" in rest and rest.index("--boards") + 1 < len(rest)
                  else None)
        _print_json(census(boards=boards,
                           include_worktrees="--include-worktrees" in rest))
        return 0
    if cmd == "throttle":
        thr = BoardFilingThrottle()
        if "--clear" in rest:
            key = rest[rest.index("--clear") + 1] if len(rest) > rest.index("--clear") + 1 else None
            print("removed=%d state=%s" % (thr.clear(key), thr.path))
            return 0
        try:
            with open(thr.path, "r", encoding="utf-8") as fh:
                _print_json(json.load(fh))
        except (OSError, ValueError):
            print("empty state=%s window_s=%d" % (thr.path, thr.window_s))
        return 0
    print("unknown subcommand %r (classify|file|stats|throttle)" % cmd, file=sys.stderr)
    return 2


def _parse_opts(args: list[str], names: set[str]) -> dict[str, str]:
    """Tiny ``--flag value`` parser (the repo's ops scripts stay argparse-light)."""
    out: dict[str, str] = {}
    i = 0
    while i < len(args):
        tok = args[i]
        if tok in names and i + 1 < len(args) and not args[i + 1].startswith("--"):
            out[tok] = args[i + 1]
            i += 2
            continue
        i += 1
    return out


if __name__ == "__main__":
    sys.exit(main())
