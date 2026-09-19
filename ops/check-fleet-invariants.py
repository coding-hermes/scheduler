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
  1. caps          — global --max-concurrent and per-namespace max_concurrent
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

Usage:  python3 ops/check-fleet-invariants.py [--db PATH] [--toml PATH] [--json]
        python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only

``--board-only`` runs checks 8-9 and nothing else, so it needs neither the live
DB nor fleet.toml — that is the mode CI uses (a runner has no ~/.hermes state).
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import sqlite3
import sys

DEFAULT_DB = os.path.expanduser("~/.hermes/coding-hermes/scheduler.db")
DEFAULT_TOML = os.path.expanduser("~/.hermes/fleet.toml")

GLOBAL_CAP_EXPECTED = 10          # daemon --max-concurrent (user unit)
FOREMAN_NS = "coding-hermes"
FOREMAN_CAP_EXPECTED = 8
SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
COOLDOWN_FLOOR = 21600            # 6h — Bane's uniform law
# Enabled lanes legitimately paced slower than the floor (namespace cadence tiers).
COOLDOWN_TIERS = {"qa-audit": 86400, "release-engineer": 604800}
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
# states the PM cycle writes.
BOARD_ALLOWED_STATUSES = ("pending", "complete", "duplicate")
BOARD_LEGACY_CLOSED_STATUSES = ("done", "completed", "closed")

CHECK_CLASSES = ("caps", "admission", "cooldown", "executors", "workdirs", "adaptive", "boards",
                 "targets", "parity", "board-vocab", "board-legacy-status")


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
    ap.add_argument("--board-only", action="store_true",
                    help="run ONLY the environment-independent board checks (8/9); "
                         "skips the live-DB/TOML checks 1-7. This is the CI mode: "
                         "a runner has no ~/.hermes/coding-hermes/scheduler.db, so "
                         "the fleet checks cannot run there.")
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
            bad("parity", ns, f"admission_mode: db={row.get('admission_mode')!r} toml={tv!r}")
        tv = toml_value(block, "max_concurrent")
        if tv is not None and int(tv) != int(row.get("max_concurrent") or 0):
            bad("parity", ns, f"max_concurrent: db={row.get('max_concurrent')} toml={tv}")

    # 8. board vocabulary -----------------------------------------------------
    # Writer-side gate (SCHED-GAP-164). The daemon's READERS accept a wide open
    # vocabulary for forward compatibility (internal/scheduler/board_freshness.go
    # openStatuses), but only status=="pending" is dispatchable: the foreman
    # prompt picks pending rows and the pending-boost counter
    # (board_awareness.go) counts only those. So a row minted as "todo" /
    # "open" / "in_progress" is visible work that no lane will ever pick up.
    # Read-only — rows are never rewritten here. A missing board skips the check
    # silently, so the script stays runnable in test rigs and old-style workdirs.
    # Same rule as Go's scheduler.ValidateBoardVocab (internal/scheduler/
    # board_vocab.go); the expected sets are pinned equal by
    # TestBoardVocabValidator_AllowedSetMatchesPythonGate.
    board = args.board or find_board_path(os.path.dirname(os.path.abspath(__file__)))
    if board and os.path.isfile(board):
        rows = legacy = 0
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
                if status in BOARD_ALLOWED_STATUSES:
                    continue
                rid = board_row_id(row)
                if status in BOARD_LEGACY_CLOSED_STATUSES:
                    legacy += 1
                    bad("board-legacy-status", rid, f"status={status} (use 'complete')")
                else:
                    bad("board-vocab", rid, f"status={status}")
        info.append({"class": "board-vocab", "subject": board,
                     "detail": f"{rows} row(s) scanned, {legacy} legacy closed spelling(s)"})

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
