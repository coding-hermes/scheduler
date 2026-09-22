#!/usr/bin/env python3
"""Check the live fleet config against the invariants the operator set.

Read-only. Exit 0 = every invariant holds; exit 1 = at least one violation
(printed as ``VIOLATION <class> <subject>: <detail>``). ``--json`` prints the
same result as a JSON document.

Why this exists: the fleet's shape is CONFIG (caps, admission modes, cooldowns,
which executor a lane drives) and config has no unit test — nothing fails when it
drifts back. This is the regression gate for that class of change. Run it after
every scheduler deploy and from the daily report.

Checks
  1. caps          — per-namespace max_concurrent (foreman 8, satellites 1).
                     The GLOBAL --max-concurrent has no DB column; it is
                     parity-checked instead: pass the daemon's actual
                     ``--global-cap N`` on a deploy and the checker fails when
                     ``[scheduler] max_concurrent`` in fleet.toml pins a
                     different value (loader.go feeds that key into the same
                     config the flag overrides, so a stale pin is drift).
                     Without ``--global-cap`` the comparison is skipped — the
                     checker cannot see the daemon's argv.
  2. admission     — tasks ONLY where real work lives; satellites on timers
  3. cooldown law  — no enabled lane below the 6h floor without a documented tier
  4. executors     — no enabled lane driving a retired driver script (the 5
                     names in the RETIRED_DRIVERS tuple below; the Go enable
                     path rejects the same 5 — internal/api/retired_drivers.go)
  5. workdirs      — every enabled lane's workdir exists
  6. targets       — every satellite's target project exists and is enabled
  7. store parity  — DB and fleet.toml agree on the operator's pins
  8. board vocab   — every JSONL board row carries a dispatchable status.
                     The board's writers are gated to
                     BOARD_ALLOWED_STATUSES; rows still carrying a legacy
                     closed spelling (done/completed/closed) are reported
                     under the separate 'board-legacy-status' class so the PM
                     cycle can sweep them in the same tick. Without this, a row
                     minted as e.g. 'todo' is visible to the fleet but picked by
                     nobody — the foreman prompt and the pending-boost counter
                     both read status=="pending" only. Skipped silently when no
                     board file is found (test rigs, old-style workdirs).
  9. board content dup — two rows with DIFFERENT ids whose non-volatile
                     content is identical (sha256 over the row minus the
                     volatile bookkeeping fields the PM cycle overwrites:
                     timestamps, notes, dispatch counters). The same finding
                     filed twice inflates the pending-boost count and keeps
                     the board undrained, so the check fires when a
                     fingerprint group carries >= 2 rows AND at least one of
                     them is still open (pending/in_progress). Groups where
                     every row is closed are exempt — historical boards
                     legitimately carry similar closure notes. Reported one
                     line per offending GROUP, all row ids listed. Skipped
                     silently when no board file is found, same as check 8.
 10. sync-orientation — every ENABLED ``*-sync`` lane is ORIENTED: someone who
                     lands in its workdir (a gap-push or dogfood picker, the
                     next tick's agent) must be able to read what the lane is,
                     which DuckBrain namespace it targets and how to consume
                     it, WITHOUT reading scheduler internals. Three facts, all
                     read from the workdir:
                       (a) ``README.md`` exists there and is non-empty (a
                           zero-byte stub does not orient);
                       (b) the README NAMES its target namespace — a line that
                           mentions ``namespace`` AND carries the lane's base as
                           a whole HYPHEN-delimited word (``trouble-sync`` →
                           ``trouble``). A >=2-token hyphen run of the base is
                           also accepted, because the live convention shortens
                           long lane bases (``h3-sdk-go-foreman-sync`` declares
                           ``sdk-go``); a single token lifted out of a
                           multi-token base does NOT count, and neither does a
                           bare mention of the word "namespace";
                       (c) the consumption contract is findable — either a
                           companion ``<lane-base>-sync-data/SKILL.md`` under
                           the skills root (default ``~/.hermes/skills``,
                           overridable with ``--skills-root``; searched
                           recursively, the skills live in category subdirs),
                           or an explicit in-README pointer to where it lives:
                           a ``/sync/``-prefixed marker key, an ``/api/`` route,
                           or the companion skill's name.
                     Mutation-found classes: a README-less lane whose
                     consumption contract exists is still un-orientable, and a
                     pointer without the namespace statement does not say what
                     the lane targets — (b) and (c) are independent.
                     One violation per lane (every unmet fact in the detail,
                     "; "-joined) and a lane whose workdir is MISSING is
                     SKIPPED: check 5 already names that fact, and one fact
                     never earns two lines. Disabled lanes are exempt, exactly
                     like check 5. The class SKIPS SILENTLY when the skills
                     root does not exist (a CI runner, a test rig) — the gate
                     must never require the live fleet to pass in CI. Runs in
                     the live-DB mode with the project rows; ``--board-only``
                     never reads them, so it is a no-op there. Measured
                     2026-09-19: 48 of 51 ``*-sync`` workdirs carried no README
                     and nothing in the fleet checked for one.
                       VIOLATION sync-orientation trouble-sync: no README.md in
                       '~/.hermes/sync-workdirs/trouble-sync' — a picker/dogfood
                       agent cannot orient this -sync shell

Usage:  python3 ops/check-fleet-invariants.py [--db PATH] [--toml PATH] [--json]
        python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only

``--board-only`` runs the environment-independent board checks (8-9) and
nothing else, so it needs neither the live DB nor fleet.toml — that is the mode
CI uses (a runner has no ~/.hermes state). The fleet checks (1-7 and 10) read
the project rows and are no-ops there.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import sqlite3
import sys

DEFAULT_DB = os.path.expanduser("~/.hermes/coding-hermes/scheduler.db")
DEFAULT_TOML = os.path.expanduser("~/.hermes/fleet.toml")

# The GLOBAL --max-concurrent lives in the daemon's argv (and optionally in
# fleet.toml's [scheduler] max_concurrent, which loader.go folds into the same
# resolved config the flag overrides). It has NO scheduler.db column, so there
# is nothing in the DB to assert a constant against — it is checked as
# flag↔TOML parity via the ``--global-cap`` argument instead (see check 7b).
FOREMAN_NS = "coding-hermes"
FOREMAN_CAP_EXPECTED = 8
SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
COOLDOWN_FLOOR = 21600            # 6h — Bane's uniform law
# Enabled lanes legitimately paced slower than the floor (namespace cadence tiers).
COOLDOWN_TIERS = {"qa-audit": 86400, "release-engineer": 604800}
# Satellite family cadence (SCHED-GAP-178, Bane 2026-09-19). MUST stay equal
# to SATELLITE_FAMILY_PINS in ~/.hermes/scripts/fleet-cooldown-policy.py (the
# policy writer); the family-floor check below fails any enabled satellite
# whose cooldown_s / cooldown_floor_s drifts off its family's pin.
SATELLITE_FAMILY_PINS = {"qa": 43200, "pm": 86400, "sync": 43200, "dogfood": 259200}
# Coding-hermes foreman PRIMARIES are the projects in the `coding-hermes`
# namespace whose name does NOT carry a satellite suffix (-qa / -pm / -sync /
# -dogfood). Satellites live in their own family namespaces; the auditor and
# other meta lanes in the coding-hermes namespace are reported as INFO
# (or simply not named a coverage violation) because they legitimately have
# no satellite set of their own — the `qa-audit` auditor, the
# `coding-hermes-scheduler` self-tick, `coding-hermes-supervisor`, etc.
CODING_HERMES_PRIMARY_NS = "coding-hermes"
# Retired dagger-era driver scripts (5). MUST stay identical — same names,
# same count — to `retiredDriverScripts` in internal/api/retired_drivers.go,
# which the enable path (createProject / updateProject) enforces. Pinned by
# tests/test_check_fleet_invariants_retired_drivers.py.
RETIRED_DRIVERS = ("pm-standin-tick.sh", "qa-scheduler-tick.sh",
                   "sync-scheduler-tick.sh", "dogfood-scheduler-tick.sh",
                   "dagger-role-tick.sh")

# Board writer vocabulary (check 8). MUST stay in lockstep with
# internal/scheduler/board_vocab.go (BoardAllowedStatuses /
# BoardLegacyClosedStatuses) — pinned by
# TestBoardVocabValidator_AllowedSetMatchesPythonGate. Only "pending" is
# dispatchable by the foreman prompt; "complete"/"duplicate" are the terminal
# states the PM cycle writes. "in_progress" is the live-lane claim the foreman
# wave writes at dispatch (RELEASE-007) — a legitimate writer status, already
# treated as open by the readers (board_freshness.go openStatuses,
# adaptive_cooldown.go boardOpenRows). The gate's remaining purpose is to catch
# PARKED spellings ("todo", "open", "rework", ...) and missing/empty statuses.
BOARD_ALLOWED_STATUSES = ("pending", "in_progress", "complete", "duplicate")
BOARD_LEGACY_CLOSED_STATUSES = ("done", "completed", "closed")
# The canonical closed spelling (mirror of Go's closedBoardStatus,
# internal/blocks/board.go:70) — the only terminal state a refile may follow.
closed_status_complete = "complete"

# Board content duplicate detection (check 9, class "board-content-dup").
# A row's fingerprint is taken over EVERY field except the ones below —
# the volatile bookkeeping fields the PM/foreman cycle overwrites on every
# dispatch, so two re-filed copies of one finding differ only there:
#   id            — by definition distinct between two different rows
#   created_at / updated_at / dispatched_at / completed_at — timestamps
#   review_notes / foreman_note / worker_summary — audit prose
#   attempts / events_count — dispatch counters
VOLATILE_FINGERPRINT_FIELDS = (
    "id", "created_at", "updated_at", "dispatched_at", "completed_at",
    "review_notes", "foreman_note", "worker_summary", "attempts", "events_count",
)
# Statuses where a content-duplicate still matters: a duplicated finding that
# is still open inflates the pending-boost counter (board_awareness.go) and
# keeps the board undrained. Closed rows are exempt.
CONTENT_DUP_OPEN_STATUSES = ("pending", "in_progress")
CONTENT_DUP_CLASS = "board-content-dup"
# Board id-slot detection (check 9b, class "board-id-slot", SCHED-GAP-207).
# An id holding TWO OR MORE OPEN rows is a recurring SLOT, not a finding: the
# satellite lane re-filed each cycle's finding under the same id without ever
# closing the previous row (measured 2026-09-22: 29% of open rows fleet-wide
# collide on id; QA-CONSENSUS-1 held 85 open rows under one id). Exact-content
# dedupe (check 9) cannot see this class — each cycle re-words the condition,
# so every copy has a fresh fingerprint. A group where every row is closed is
# exempt, and so is exactly ONE open row under an id whose earlier rows are
# closed: re-filing AFTER closing the previous row is the legitimate cycle.
ID_SLOT_CLASS = "board-id-slot"

# Sync-lane orientation (check 10, class "sync-orientation"). A `*-sync` lane's
# workdir is a SHELL, not a repo: it holds the lane's scratch space, and the
# only place a gap-push/dogfood picker (or the next tick's agent) can learn what
# the lane is, which DuckBrain namespace it targets and how to consume it is the
# workdir's README plus the companion `<base>-sync-data` skill. Measured
# 2026-09-19: 48 of 51 `*-sync` workdirs carried no README and NOTHING in the
# fleet checked for one — the cited `duckbrain-sync` instance was repaired by
# hand, so the class stayed open. This gate is the repo-side half of
# SCHED-GAP-094; provisioning the files is the sync-workdirs lane's job.
SYNC_ORIENTATION_CLASS = "sync-orientation"
DEFAULT_SKILLS_ROOT = os.path.expanduser("~/.hermes/skills")
# The in-README consumption pointer: a marker key, an HTTP route, or the
# companion data skill's own name. Any one of the three tells a consumer where
# the lane's consumption contract lives, which is what (c) asks for.
SYNC_CONTRACT_POINTER_RE = re.compile(r"/sync/|/api/|[a-z0-9][a-z0-9._-]*-sync-data", re.I)

CHECK_CLASSES = ("caps", "admission", "cooldown", "executors", "workdirs", "adaptive", "boards",
                 "coverage", "family-floor", "targets", "parity",
                 "board-vocab", "board-legacy-status", "board-content-dup",
                 "board-id-slot", "sync-orientation")


def find_board_path(start: str) -> str | None:
    """Walk up from *start* looking for ``.coding-hermes/board/tasks.jsonl``."""
    cur = os.path.abspath(start)
    while True:
        cand = os.path.join(cur, ".coding-hermes", "board", "tasks.jsonl")
        if os.path.isfile(cand):
            return cand
        parent = os.path.dirname(cur)
        if parent == cur:
            return None
        cur = parent


def board_row_id(row: dict) -> str:
    """Row identifier for reporting: ``id``, then ``task_id``, then ``<unknown>``.

    Mirrors Go's boardRowID — only a non-empty STRING counts as an id.
    """
    for key in ("id", "task_id"):
        val = row.get(key)
        if isinstance(val, str) and val.strip():
            return val.strip()
    return "<unknown>"


def board_row_status(row: dict) -> str:
    """Normalised status: absent/None → "", else ``str(...).lower().strip()``.

    Mirrors Go's boardRowStatus.
    """
    raw = row.get("status")
    if raw is None:
        return ""
    return str(raw).lower().strip()


def compute_content_fingerprint(row: dict) -> str:
    """Stable content fingerprint of a board row: sha256 over the row minus
    the volatile bookkeeping fields (:data:`VOLATILE_FINGERPRINT_FIELDS`),
    serialized with sorted keys and compact separators so the digest depends
    only on the CONTENT, never on dict order or formatting.

    Two rows with the same fingerprint carry byte-identical findings once the
    volatile fields the PM cycle overwrites are ignored.
    """
    content = {k: v for k, v in row.items() if k not in VOLATILE_FINGERPRINT_FIELDS}
    serialized = json.dumps(content, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(serialized.encode()).hexdigest()[:16]


# The metric half of SCHED-GAP-207 ask (3): "open rows" must count unique
# FINDINGS, not accumulated slots. boardOpenRows (Go,
# internal/scheduler/adaptive_cooldown.go) counts every open row line, so a
# board where QA-CONSENSUS-1 was filed 85x reads as 85 rows of work. This
# repo-side meter counts DISTINCT OPEN IDS: the number a human would call
# "the open work". Exposed by check 9b's scan as INFO and by --json.
def count_unique_open_ids(board: str) -> dict:
    """Count open board rows two ways: raw open-row lines and distinct open ids.

    Returns {"rows": <open rows>, "ids": <distinct open ids>,
             "slot_rows": <open rows beyond the first per id>}.
    ``slot_rows`` is the inflation the id-slot habit adds — the part of the
    raw number that is log, not work. Malformed lines are skipped (same
    tolerance as the violation scan); a missing board returns all zeros.
    """
    out = {"rows": 0, "ids": 0, "slot_rows": 0}
    if not board or not os.path.isfile(board):
        return out
    open_ids: set[str] = set()
    with open(board, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if not isinstance(row, dict):
                continue
            status = board_row_status(row)
            if status in BOARD_LEGACY_CLOSED_STATUSES or status == closed_status_complete:
                continue
            out["rows"] += 1
            open_ids.add(board_row_id(row))
    out["ids"] = len(open_ids)
    out["slot_rows"] = out["rows"] - out["ids"] if out["rows"] >= len(open_ids) else 0
    return out


def index_sync_data_skills(skills_root: str) -> set[str]:
    """Names of every ``*-sync-data`` skill present under *skills_root*.

    Recursive: the skills live in category subdirectories (``data/`` and
    friends), so a one-level scan would miss every companion skill and turn
    the orientation gate into an unconditional failure. A skill counts only
    when it carries a ``SKILL.md`` — an empty directory is not a contract.
    """
    names: set[str] = set()
    for dirpath, dirnames, _ in os.walk(skills_root):
        for d in dirnames:
            if d.endswith("-sync-data") and os.path.isfile(os.path.join(dirpath, d, "SKILL.md")):
                names.add(d)
    return names


def sync_readme_declares_namespace(text: str, base: str) -> bool:
    """Does *text* (a sync lane's README) NAME its target DuckBrain namespace?

    The rule is deliberately mechanical: a LINE that mentions ``namespace``
    (case-insensitive) AND carries one of two ACCEPTED FORMS of the lane's base
    — the base itself (``trouble-sync`` → ``trouble``), or a contiguous hyphen
    run of at least TWO of its tokens (``h3-sdk-go-foreman-sync`` → ``sdk-go``).
    Everything is matched on token boundaries, so a bare mention of the word
    "namespace" does NOT pass, and neither does a single token lifted out of a
    multi-token base (``h3-sdk-go-foreman-sync`` is not oriented by the word
    ``go``).

    The two-token form is not a convenience: the live convention shortens long
    lane bases, because the DuckBrain namespace is not the scheduler project
    name (``h3-sdk-go-foreman-sync`` documents its namespace as ``sdk-go``).
    A match must be a whole HYPHEN-delimited word: the base appearing inside a
    longer hyphenated token (``other-fixture-orientation-target-x``) or a longer
    alphanumeric run (``fixture-orientation-targets``) is not a declaration.
    """
    tokens = [t for t in base.split("-") if t]
    runs = ["-".join(tokens[i:j])
            for i in range(len(tokens))
            for j in range(i + 2, len(tokens) + 1)]
    candidates = [base] + runs
    if not candidates:
        return False
    for line in text.splitlines():
        if "namespace" not in line.lower():
            continue
        for cand in candidates:
            if re.search(rf"(?<![a-z0-9-]){re.escape(cand)}(?![a-z0-9-])", line, re.I):
                return True
    return False


def parse_toml_blocks(text: str, header: str) -> dict[str, str]:
    """Split a flat TOML file into {id/name: block text} for ``[[header]]``."""
    out, cur, key = {}, None, None
    for line in text.splitlines():
        if line.startswith("[["):
            if cur and key:
                out[key] = cur
            cur = [line] if line.strip() == f"[[{header}]]" else None
            key = None
        elif cur is not None:
            cur.append(line)
            m = re.match(r'\s*(?:id|name)\s*=\s*"([^"]+)"', line)
            if m and key is None:
                key = m.group(1)
    if cur and key:
        out[key] = cur
    return {k: "\n".join(v) for k, v in out.items()}


def parse_toml_table(text: str, header: str) -> str:
    """Extract the body of a single-bracket ``[header]`` table from flat TOML.

    :func:`parse_toml_blocks` only matches ``[[header]]`` array-of-table
    entries (the ``[[projects]]`` / ``[[namespaces]]`` shape); the root
    ``[scheduler]`` table is a single-bracket table and is invisible to it.
    Returns the table's lines joined by newlines, or ``""`` when absent.
    A nested ``[header.sub]`` table does NOT terminate the scan (its lines
    belong to the parent in TOML semantics) — only a new top-level ``[``
    header does.
    """
    out: list[str] = []
    in_table = False
    for line in text.splitlines():
        if line.startswith("["):
            is_ours = line.strip() == f"[{header}]"
            is_child = line.startswith(f"[{header}.")
            if in_table and not is_child:
                break  # a sibling top-level table ends the section
            in_table = is_ours or in_table
            if is_ours:
                continue
            if in_table:
                out.append(line)  # a [header.sub] line stays in the body
            continue
        if in_table:
            out.append(line)
    return "\n".join(out)


def toml_value(block: str, field: str) -> str | None:
    m = re.search(rf'^\s*{re.escape(field)}\s*=\s*"?([^"\n]+)"?', block, re.M)
    return m.group(1).strip() if m else None


def main(argv: list[str] | None = None) -> int:
    """Run the invariants checks and return the process exit code (0 = clean).

    *argv* is the argument vector seam: ``None`` reads ``sys.argv`` (the CLI
    path used by CI and the runbook), while a test passes an explicit list
    (``["--db", <tmp>, "--toml", <missing>, "--board", <missing>]``) so the gate
    can be driven in-process against a fixture DB without a subprocess. The
    function is import-safe on purpose — the file is still a script
    (``ops/check-fleet-invariants.py``), loaded by path in the tests.
    """
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--toml", default=DEFAULT_TOML)
    ap.add_argument("--board", default=None,
                    help="JSONL board for check 8; default: walk up from this script "
                         "to .coding-hermes/board/tasks.jsonl (skipped when absent)")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--global-cap", type=int, default=None,
                    help="the daemon's actual global --max-concurrent value; "
                         "parity-checked against fleet.toml [scheduler] "
                         "max_concurrent (skipped when omitted — the checker "
                         "cannot see the daemon's argv on its own)")
    ap.add_argument("--board-only", action="store_true",
                    help="run ONLY the environment-independent board checks (8/9); "
                         "skips the live-DB/TOML checks 1-7. This is the CI mode: "
                         "a runner has no ~/.hermes/coding-hermes/scheduler.db, so "
                         "the fleet checks cannot run there.")
    ap.add_argument("--skills-root", default=DEFAULT_SKILLS_ROOT,
                    help="skills root searched for a sync lane's companion "
                         "<base>-sync-data skill (check 10). Default ~/.hermes/skills; "
                         "when the root does not exist the check skips silently, so a "
                         "runner/test rig never has to carry live fleet state.")
    args = ap.parse_args(argv)

    violations: list[dict] = []
    info: list[dict] = []

    def bad(cls: str, subject: str, detail: str) -> None:
        violations.append({"class": cls, "subject": subject, "detail": detail})

    # The live-DB checks 1-7 need ~/.hermes/coding-hermes/scheduler.db and
    # ~/.hermes/fleet.toml — neither exists on a CI runner. --board-only keeps the
    # board checks runnable there: the DB is not opened and the fleet maps stay
    # empty, which makes every fleet check a no-op (check 1 is guarded below).
    con = None
    namespaces: dict = {}
    projects: dict = {}
    have_ownership = False
    if not args.board_only:
        con = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
        con.row_factory = sqlite3.Row
        namespaces = {r["id"]: dict(r) for r in con.execute("SELECT * FROM namespaces")}
        projects = {r["name"]: dict(r) for r in con.execute("SELECT * FROM projects")}
        have_ownership = "board_ownership" in [r[1] for r in con.execute("PRAGMA table_info(projects)")]

    # 1. caps -----------------------------------------------------------------
    if con is not None:
        if namespaces.get(FOREMAN_NS, {}).get("max_concurrent") != FOREMAN_CAP_EXPECTED:
            bad("caps", FOREMAN_NS, f"max_concurrent={namespaces.get(FOREMAN_NS, {}).get('max_concurrent')} "
                                    f"expected {FOREMAN_CAP_EXPECTED} (the foremen's guaranteed room)")
        for ns in SATELLITE_NS:
            row = namespaces.get(ns)
            if row is None:
                bad("caps", ns, "namespace missing")
            elif row.get("max_concurrent") != 1:
                bad("caps", ns, f"max_concurrent={row.get('max_concurrent')} expected 1 (one global slot per satellite family)")

    # 2. admission ------------------------------------------------------------
    for ns, row in namespaces.items():
        mode = (row.get("admission_mode") or "").strip()
        if not mode:
            bad("admission", ns, "no admission_mode set (falls back to cooldown by luck, not by config)")
        elif ns == FOREMAN_NS and mode != "tasks":
            bad("admission", ns, f"mode={mode} — the foremen namespace must be tasks (fast with work, timer when perpetual-only)")
        elif ns != FOREMAN_NS and mode != "cooldown":
            bad("admission", ns, f"mode={mode} — satellite namespaces must be timer-paced (cooldown)")

    # 3. cooldown law ---------------------------------------------------------
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        cd = p.get("cooldown_s") or 0
        if cd < COOLDOWN_FLOOR and name not in COOLDOWN_TIERS:
            bad("cooldown", name, f"cooldown_s={cd} below the {COOLDOWN_FLOOR}s (6h) floor — sub-6h pins are retired")
        if name in COOLDOWN_TIERS and cd != COOLDOWN_TIERS[name]:
            bad("cooldown", name, f"cooldown_s={cd} != documented tier {COOLDOWN_TIERS[name]}")

    # 4. executors ------------------------------------------------------------
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        blob = (p.get("command") or "") + (p.get("prompt") or "")
        for driver in RETIRED_DRIVERS:
            if driver in blob and "RETIRED" not in blob and "do NOT run" not in blob:
                bad("executors", name, f"instructs the retired driver {driver} — lanes run their skill")

    # 5. workdirs -------------------------------------------------------------
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        wd = p.get("workdir") or ""
        if not wd or not os.path.isdir(wd):
            bad("workdirs", name, f"workdir missing: {wd!r}")

    # 5b. adaptive arming (Bane 2026-09-19: "we should not have adaptive cool down
    # for the satellite lanes"). Adaptive cooldown exists to slow a lane that stops
    # making progress; satellites are already paced by their family floor and their
    # namespace cap, so arming one only delays a lane whose job is to report — and an
    # armed satellite makes its family's cadence unreadable. Disarm, don't debate.
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        m = re.match(r"^(.+)-(qa|pm|dogfood|sync)$", name)
        if m and p.get("adaptive_cooldown"):
            bad("adaptive", name, "satellite lane is adaptive-armed — satellites must never arm adaptive cooldown")

    # 5c. boards --------------------------------------------------------------
    # A lane must be able to read its work. A satellite reads its PRIMARY's board, so a
    # missing board link means the lane runs blind (measured 2026-09-19: 11 -pm lanes had
    # none while their targets did). Repo-less lanes (a sync lane targeting a DuckBrain
    # data source with no project row) are the documented exception — reported as INFO,
    # never a violation.
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        wd = p.get("workdir") or ""
        if not wd or not os.path.isdir(wd):
            continue
        b = os.path.join(wd, ".coding-hermes", "board")
        if os.path.exists(b) or os.path.islink(b):
            continue
        m = re.match(r"^(.+)-(qa|pm|dogfood|sync)$", name)
        if m and m.group(1) in projects:
            bad("boards", name, f"board does not resolve in {wd!r} while its target {m.group(1)!r} exists")

    # 5d. coverage ------------------------------------------------------------
    # Every coding-hermes PRIMARY needs an enabled qa/pm/sync/dogfood satellite.
    # Discrimination (SCHED-GAP-178, Bane 2026-09-19): the project must live in
    # the `coding-hermes` namespace (the projects row carries a `namespace_id`
    # column populated from the namespaces table) AND its name must NOT match
    # the satellite suffix. Internal/meta lanes in the same namespace that
    # legitimately have no satellite set of their own (e.g. qa-audit, the
    # scheduler self-tick, the supervisor) are reported as INFO — they are
    # children, not primaries, of the foreman family.
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        if re.match(r"^(.+)-(qa|pm|dogfood|sync)$", name):
            continue
        ns = p.get("namespace_id") or ""
        if ns != CODING_HERMES_PRIMARY_NS:
            continue
        for suffix in ("qa", "pm", "sync", "dogfood"):
            child = projects.get(f"{name}-{suffix}")
            if not child or not child.get("enabled"):
                bad("coverage", f"{name}-{suffix}",
                    f"missing or disabled — every coding-hermes primary needs an enabled {suffix} lane "
                    f"(primary {name!r} lives in the {ns!r} namespace)")

    # 5e. family-floor --------------------------------------------------------
    # Every enabled satellite sits on its family's cadence pin (qa 43200, pm
    # 86400, sync 43200, dogfood 259200). Both cooldown_s AND cooldown_floor_s
    # must equal the family value — a satellite reads the primary's board
    # through a symlink and is therefore permanently 'has work', which the
    # REDUCE rule in fleet-cooldown-policy.py would otherwise pull to the 6h
    # floor on every run. The pair is checked together because either field
    # alone is enough to make cadence unreadable.
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        m = re.match(r"^(.+)-(qa|pm|dogfood|sync)$", name)
        if not m:
            continue
        suffix = m.group(2)
        expected = SATELLITE_FAMILY_PINS.get(suffix)
        if expected is None:
            continue
        cd = int(p.get("cooldown_s") or 0)
        fl = int(p.get("cooldown_floor_s") or 0)
        if cd != expected or fl != expected:
            bad("family-floor", name,
                f"cooldown_s={cd} / cooldown_floor_s={fl} — satellite family expects {expected} for {suffix} "
                f"(see SATELLITE_FAMILY_PINS, also mirrored in fleet-cooldown-policy.py)")

    # 6. satellite targets ----------------------------------------------------
    # A sync lane may legitimately target a DuckBrain data source rather than a
    # fleet project (repo-less lanes, report dirs). Naming a target is not enough
    # to call it broken, and a name allowlist would rot — so ask for EVIDENCE:
    # the lane's own workdir exists AND it has completed a tick in the last 7
    # days. Otherwise the target really is dead weight.
    for name, p in projects.items():
        if not p.get("enabled"):
            continue
        m = re.match(r"^(.+)-(qa|pm|dogfood|sync)$", name)
        if not m:
            continue
        base = m.group(1)
        t = projects.get(base)
        if t is not None and not t.get("enabled"):
            bad("targets", name, f"target {base!r} is DISABLED — the lane would burn a clean-machine battery on nothing")
            continue
        if t is not None:
            continue
        cands = (f"/home/kara/{base}", f"/home/kara/{base.replace('-', '_')}",
                 os.path.expanduser(f"~/.hermes/{base}"))
        if any(os.path.isdir(c) for c in cands):
            continue
        wd = p.get("workdir") or ""
        last = con.execute(
            "SELECT MAX(spawned_at) FROM ticks WHERE project_name=? AND status='completed'", (name,)).fetchone()[0]
        active = False
        if bool(wd) and os.path.isdir(wd) and last:
            try:
                ts = dt.datetime.fromisoformat(str(last).replace("Z", "+00:00"))
                if ts.tzinfo is not None:
                    ts = ts.astimezone().replace(tzinfo=None)
                active = (dt.datetime.now() - ts) <= dt.timedelta(days=7)
            except ValueError:
                active = False
        if active:
            info.append({"class": "targets", "subject": name,
                         "detail": f"external data target {base!r} (no repo/project) — lane is active, verified by its own workdir + recent completed tick"})
        else:
            bad("targets", name, f"target {base!r} is neither a fleet project nor a path on disk, and the lane shows no recent activity")

    # 7. store parity ---------------------------------------------------------
    toml = open(args.toml).read() if os.path.exists(args.toml) else ""
    proj_blocks = parse_toml_blocks(toml, "projects")
    ns_blocks = parse_toml_blocks(toml, "namespaces")
    for name, p in projects.items():
        if not p.get("enabled") or name not in proj_blocks:
            continue
        block = proj_blocks[name]
        for field in ("cooldown_s", "cooldown_floor_s", "cooldown_ceiling_s"):
            tv = toml_value(block, field)
            if tv is not None and p.get(field) is not None and int(tv) != int(p[field]):
                bad("parity", name, f"{field}: db={p[field]} toml={tv} — a pin in one store is drift")
        tv = toml_value(block, "board_ownership")
        if have_ownership and tv is not None:
            dbv = (p.get("board_ownership") or "").strip()
            if dbv != tv.strip():
                bad("parity", name, f"board_ownership: db={dbv!r} toml={tv!r}")
    for ns, row in namespaces.items():
        block = ns_blocks.get(ns)
        if not block:
            continue
        tv = toml_value(block, "admission_mode")
        if tv is not None and (row.get("admission_mode") or "") != tv:
            bad("parity", ns, f"admission_mode: db={row.get('admission_mode')} toml={tv}")
        tv = toml_value(block, "max_concurrent")
        if tv is not None and int(tv) != int(row.get("max_concurrent") or 0):
            bad("parity", ns, f"max_concurrent: db={row.get('max_concurrent')} toml={tv}")

    # 7b. global-cap parity -----------------------------------------------------
    # The global --max-concurrent has no DB column, so unlike the per-store
    # checks above this is ARGV↔TOML parity: the operator passes the daemon's
    # actual flag value (--global-cap N) and the checker fails when fleet.toml
    # [scheduler] max_concurrent pins a different number. That TOML key feeds
    # the daemon's resolved config (internal/config/loader.go:176) and is
    # overridden only when the flag is explicitly set, so a stale pin drifts
    # into every restart that omits the flag. Without --global-cap this check
    # is skipped — a checker cannot assert a value it cannot see.
    if args.global_cap is not None:
        tv = toml_value(parse_toml_table(toml, "scheduler"), "max_concurrent")
        if tv is not None and int(tv) != int(args.global_cap):
            bad("parity", "[scheduler]",
                f"global max_concurrent: daemon={args.global_cap} toml={tv} — a stale "
                f"[scheduler] max_concurrent pin overrides every restart that omits the flag")

    # 10. sync orientation ------------------------------------------------------
    # A `*-sync` lane's workdir is a SHELL, not a repo. Anyone who lands in it —
    # a gap-push/dogfood picker, the next tick's agent — must be able to read
    # what the lane is, which DuckBrain namespace it targets and how to consume
    # it WITHOUT reading scheduler internals. Three facts, read from the workdir:
    #   (a) README.md exists and is non-empty;
    #   (b) the README NAMES its namespace (a `namespace` line carrying the
    #       lane's base as a whole hyphen-delimited word, or a >=2-token hyphen
    #       run of it);
    #   (c) the consumption contract is findable — the companion
    #       `<base>-sync-data` skill under the skills root, OR an explicit
    #       in-README pointer (/sync/ marker, /api/ route, skill name).
    # (b) tests meaning and (c) tests discoverability; they are independent, so
    # one repaired does not describe the other. One violation per LANE, every
    # unmet fact collected into the detail. A lane whose workdir is MISSING is
    # skipped outright — check 5 already names that fact and one fact never
    # earns two lines. Disabled lanes are exempt, exactly like check 5.
    #
    # CI SAFETY: nothing here may require live fleet state. When the skills root
    # is absent (a GitHub runner, a test rig) the class SKIPS SILENTLY, the same
    # documented behaviour checks 8/9 have for a missing board — the pytest
    # battery is what CI runs, and it must stay green on a bare checkout.
    # Read-only: no workdir is written and no project row is touched.
    if con is not None:
        skills = (index_sync_data_skills(args.skills_root)
                  if os.path.isdir(args.skills_root) else None)
        for name, p in projects.items():
            if not p.get("enabled") or not name.endswith("-sync"):
                continue
            wd = p.get("workdir") or ""
            if not wd or not os.path.isdir(wd):
                continue  # check 5 owns the missing-workdir fact
            base = name[:-len("-sync")]
            missing: list[str] = []
            readme = os.path.join(wd, "README.md")
            text = ""
            if not os.path.isfile(readme) or os.path.getsize(readme) == 0:
                missing.append(f"no README.md in {wd!r} — a picker/dogfood agent cannot "
                               f"orient this -sync shell")
            else:
                text = open(readme, encoding="utf-8", errors="replace").read()
                if not sync_readme_declares_namespace(text, base):
                    missing.append(f"README.md names no target namespace — state the DuckBrain "
                                   f"namespace this lane syncs (a line mentioning `namespace` "
                                   f"and carrying {base!r} as a whole token)")
            has_skill = skills is None or f"{base}-sync-data" in skills
            has_pointer = bool(text) and bool(SYNC_CONTRACT_POINTER_RE.search(text))
            if skills is not None and not has_skill and not has_pointer:
                missing.append(f"no consumption contract findable — expected a "
                               f"{base}-sync-data skill under {args.skills_root!r} or a README "
                               f"pointer to one (/sync/ marker, /api/ route, or the skill name)")
            if missing:
                bad(SYNC_ORIENTATION_CLASS, name, "; ".join(missing))

    # 8. board vocabulary + 9. board content duplicates ------------------------
    # Writer-side gate (SCHED-GAP-164). The daemon's READERS accept a wide open
    # vocabulary for forward compatibility (internal/scheduler/board_freshness.go
    # openStatuses), but only status=="pending" is dispatchable: the foreman
    # prompt picks pending rows and the pending-boost counter
    # (board_awareness.go) counts only those. So a row minted as "todo" /
    # "open" / "rework" is visible work that no lane will ever pick up.
    # "in_progress" is different: it is the live-lane claim the foreman wave
    # writes at dispatch (RELEASE-007), so it is allowed. Read-only — rows are
    # never rewritten here. A missing board skips the check silently, so the
    # script stays runnable in test rigs and old-style workdirs.
    # Same rule as Go's scheduler.ValidateBoardVocab (internal/scheduler/
    # board_vocab.go); the expected sets are pinned equal by
    # TestBoardVocabValidator_AllowedSetMatchesPythonGate.
    #
    # Check 9 (SCHED-GAP-172) rides the same file pass: it groups rows by their
    # compute_content_fingerprint() and flags any group with >= 2 rows where at
    # least one row is still open (pending/in_progress). Reported ONE line per
    # offending GROUP with every row id listed (a pair file becomes a triple
    # after one more copy-paste — per-group keeps one finding per real
    # duplicate, not an O(n²) wall of pairs).
    board = args.board or find_board_path(os.path.dirname(os.path.abspath(__file__)))
    if board and os.path.isfile(board):
        rows = legacy = 0
        by_fingerprint: dict[str, list[tuple[str, str]]] = {}
        by_id: dict[str, list[str]] = {}
        with open(board, encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    row = json.loads(line)
                except ValueError:
                    continue  # malformed JSONL is a different gate's concern
                if not isinstance(row, dict):
                    continue
                rows += 1
                status = board_row_status(row)
                rid = board_row_id(row)
                if status in BOARD_ALLOWED_STATUSES:
                    pass
                elif status in BOARD_LEGACY_CLOSED_STATUSES:
                    legacy += 1
                    bad("board-legacy-status", rid, f"status={status} (use 'complete')")
                else:
                    bad("board-vocab", rid, f"status={status}")
                # 9. board content dup — fingerprint every parseable dict row,
                # closed or open; the exemption happens at flag time.
                by_fingerprint.setdefault(compute_content_fingerprint(row), []).append(
                    (rid, status))
                # 9b. board id-slot (SCHED-GAP-207) — every OPEN row joins its
                # id's group; closed rows do not (a closed history under an id
                # with one open row is the legitimate refile-after-close cycle).
                # Open for slot purposes: the dispatchable pair, the legacy
                # closed spellings are CLOSED, and everything else (including
                # missing/empty) is open — matching the reader vocabulary
                # (internal/scheduler/board_freshness.go openStatuses: unknown
                # spellings are open, never hidden work).
                if status not in BOARD_LEGACY_CLOSED_STATUSES and status != closed_status_complete:
                    by_id.setdefault(rid, []).append(status)
        info.append({"class": "board-vocab", "subject": board,
                     "detail": f"{rows} row(s) scanned, {legacy} legacy closed spelling(s)"})
        # SCHED-GAP-207 ask (3): surface the unique-findings view of "open
        # rows" alongside the raw count, so burndowns and "is this done"
        # callers can stop reading a slot-accumulated log as work.
        open_meter = count_unique_open_ids(board)
        info.append({"class": "open-rows", "subject": board,
                     "detail": (f"open rows={open_meter['rows']} "
                                f"unique open ids={open_meter['ids']} "
                                f"slot-inflated rows={open_meter['slot_rows']}")})
        for fp, members in by_fingerprint.items():
            if len(members) < 2:
                continue
            if not any(st in CONTENT_DUP_OPEN_STATUSES for _, st in members):
                continue  # all-closed groups are exempt (historical closure notes)
            bad(CONTENT_DUP_CLASS, f"[{','.join(rid for rid, _ in members)}]",
                f"identical content (fingerprint {fp})")
        for rid, statuses in by_id.items():
            if len(statuses) < 2:
                continue  # one open row under an id is a finding, not a slot
            bad(ID_SLOT_CLASS, rid,
                f"{len(statuses)} OPEN rows share this id — a recurring SLOT, not a "
                f"finding (close each previous row before re-filing; SCHED-GAP-207)")

    counts = {c: 0 for c in CHECK_CLASSES}
    for v in violations:
        counts[v["class"]] = counts.get(v["class"], 0) + 1
    checks = [{"class": c, "violations": counts.get(c, 0),
               "summary": f"{c}: {counts.get(c, 0)} violations"} for c in CHECK_CLASSES]

    result = {
        "ok": not violations,
        "checks": checks,
        "board_only": bool(args.board_only),
        "checked": {
            "namespaces": len(namespaces),
            "projects": len(projects),
            "enabled": sum(1 for p in projects.values() if p.get("enabled")),
        },
        "violations": violations,
        "info": info,
    }
    if args.json:
        print(json.dumps(result, indent=1))
    else:
        for v in violations:
            print(f"VIOLATION {v['class']} {v['subject']}: {v['detail']}")
        for i in info:
            print(f"INFO {i['class']} {i['subject']}: {i['detail']}")
        if args.board_only:
            print(f"{'PASS' if result['ok'] else 'FAIL'} — {len(violations)} violation(s) "
                  f"(board-only mode: fleet checks 1-7 not run — no local DB/TOML)")
        else:
            print(f"{'PASS' if result['ok'] else 'FAIL'} — {len(violations)} violation(s) across "
                  f"{result['checked']['namespaces']} namespaces / {result['checked']['enabled']} enabled lanes")
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
