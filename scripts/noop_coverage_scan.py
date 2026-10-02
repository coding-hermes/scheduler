#!/usr/bin/env python3
"""SCHED-GAP-1688 AC7 -- the zero-artifact coverage scan.

Over a window (default 24h) every tick that produced ZERO artifacts (no commits
AND no changed files) MUST carry either an artifact or a /noop/ record in
DuckBrain. The headline number is the count of zero-artifact ticks WITH no
record: it must be 0 once the end-of-tick hook (SCHED-GAP-1688) is armed. This
script IS the proof that ships beside that change.

  python3 scripts/noop_coverage_scan.py --hours 24

Exit codes:
  0  the count is 0, OR the hook is off (nothing to prove yet -- reported honestly)
  1  the hook is ON and covered < total (unexplained idle ticks exist)
  2  the scan itself could not run (DB or DuckBrain unreachable) -- never a false 0
"""
import argparse
import json
import os
import sqlite3
import sys
import urllib.request

DB = os.environ.get("SCHEDULER_DB", os.path.expanduser("~/.hermes/coding-hermes/scheduler.db"))
DUCK = os.environ.get("DUCKBRAIN_API", "http://127.0.0.1:3000")
NS = os.environ.get("DUCKBRAIN_NOOP_NS", "infra")


def token():
    for p in ("~/.duckbrain/foreman-status.token", "~/.duckbrain/infra.token"):
        f = os.path.expanduser(p)
        if os.path.isfile(f):
            return open(f).read().strip()
    return os.environ.get("DUCKBRAIN_API_KEY", "")


def noop_tick_ids(timeout=30):
    """The tick id (`<lane>-<ts>`) tail of every /noop/<date>/<lane>/<tick>."""
    url = f"{DUCK}/api/memories?namespace={NS}&prefix=/noop/&limit=5000"
    req = urllib.request.Request(url, headers={"x-api-key": token()})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        data = json.load(r)
    out = set()
    for m in data.get("items", []):
        parts = (m.get("key") or "").split("/")
        if len(parts) >= 5 and parts[1] == "noop":
            out.add(parts[-1])
    return out


def zero_artifact_ticks(hours):
    con = sqlite3.connect(DB)
    con.row_factory = sqlite3.Row
    q = ("SELECT id, project_name, COALESCE(outcome,'') AS outcome FROM ticks "
         "WHERE commits=0 AND files_changed=0 AND status IN ('completed','failed') "
         # spawned_at is stored as LOCAL ISO-8601 (e.g. ...T22:36:36-05:00) while
         # datetime('now') is UTC: datetime() normalises both to UTC so the window
         # is not silently shifted by the machine's offset.
         "AND datetime(spawned_at) >= datetime('now', ?)")
    return [dict(r) for r in con.execute(q, (f"-{hours} hours",))]


def hook_armed():
    return os.environ.get("SCHEDULER_IDLE_INTERVENTION", "").strip().lower() in ("1", "true", "yes", "on")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hours", type=int, default=24)
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    try:
        ticks = zero_artifact_ticks(a.hours)
    except Exception as e:  # noqa: BLE001
        print(f"scan: scheduler DB unreadable ({e}) -- cannot prove coverage", file=sys.stderr)
        return 2
    try:
        recorded = noop_tick_ids()
    except Exception as e:  # noqa: BLE001
        print(f"scan: DuckBrain unreachable ({e}) -- cannot prove coverage", file=sys.stderr)
        return 2

    total = len(ticks)
    uncovered = sorted((t for t in ticks if t["id"] not in recorded), key=lambda t: t["id"])
    armed = hook_armed()
    report = {
        "window_hours": a.hours,
        "hook_armed": armed,
        "zero_artifact_ticks": total,
        "with_noop_record": total - len(uncovered),
        "without_record": len(uncovered),
        "uncovered": [{"id": t["id"], "lane": t["project_name"], "outcome": t["outcome"]} for t in uncovered[:50]],
    }
    if a.json:
        print(json.dumps(report, indent=2))
    else:
        print(f"SCHED-GAP-1688 coverage scan -- last {a.hours}h (hook_armed={armed})")
        print(f"  zero-artifact ticks : {total}")
        print(f"  with a /noop/ record: {total - len(uncovered)}")
        print(f"  WITHOUT a record    : {len(uncovered)}   <-- must be 0 once the hook is proven")
        for t in report["uncovered"][:10]:
            print(f"    - {t['lane']:24s} {t['id']}  outcome={t['outcome'] or '-'}")
        if not armed:
            print("  NOTE: the hook is OFF -- these are the ticks it WOULD have asked to explain"
                  " (this is the 'before' measurement).")

    if not armed:
        return 0
    return 1 if uncovered else 0


if __name__ == "__main__":
    sys.exit(main())
