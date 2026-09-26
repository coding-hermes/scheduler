#!/usr/bin/env python3
"""destination-coverage-check.py — verify data pipelines by rows-landed, not cron status.

Retro finding (2026-08-29/30 gateway hole): a broken writer persisted ZERO telegram
rows for ~2 days while every cron job reported status=ok. Cron status says a job
RAN; only the destination can say data LANDED. This checker counts yesterday's
rows at each destination, compares against the prior-7-day baseline, and exits 1
(alert) when a destination went silent or collapsed:

  destinations
    telegram   rows in ~/.hermes/state.db messages for sessions whose source is
               a telegram source (gateway-routed traffic; timestamp REAL epoch)
    ticks      rows in ~/.hermes/coding-hermes/scheduler.db ticks (created_at is
               TEXT; ISO-8601 with offset or 'YYYY-MM-DD HH:MM:SS')
    duckbrain  AGGREGATE commit volume across ~/duckbrain/namespaces/<ns> git
               repos; only the aggregate can ALERT. Per-namespace rows are
               printed as WARN context because a single namespace legitimately
               goes silent for days when its scheduler lane is paused or on
               cooldown — that is scheduling, not a dead pipeline. Namespaces
               with zero commits in the whole 8-day window are dormant/skipped.

  alert rules (per destination)
    yesterday == 0 and baseline7d_avg >= 1            -> ALERT (dead writer)
    baseline7d_avg >= 4 and yesterday < 0.25*baseline -> ALERT (collapse)
    yesterday == 0 and baseline7d_avg < 1             -> BASELINE-UNKNOWN
    otherwise                                          -> OK
    an unreadable database counts as ALERT (zero visibility is not OK)

Both sqlite databases are opened READ-ONLY with a short busy timeout — the
checker never blocks a live writer and never writes anything itself (idempotent:
two runs in a row print the same thing).

Usage:
  python3 scripts/destination-coverage-check.py                 # daily check
  python3 scripts/destination-coverage-check.py --json          # machine-readable
  python3 scripts/destination-coverage-check.py --date 2026-08-30
  python3 scripts/destination-coverage-check.py --simulate-dead telegram
  python3 scripts/destination-coverage-check.py --simulate-dead <namespace>

--simulate-dead forces the named destination's yesterday count to 0 (baseline is
still measured) so the alert path can be proven live; 'duckbrain' forces every
active namespace. --date overrides the target UTC day for deterministic checks.

Exit codes: 0 = all destinations OK/BASELINE-UNKNOWN, 1 = at least one ALERT.
"""
import argparse
import datetime
import json
import os
import sqlite3
import subprocess
import sys

DAY = 86400
BASELINE_DAYS = 7
DORMANT_WINDOW_DAYS = BASELINE_DAYS + 1

STATE_DB = os.path.expanduser("~/.hermes/state.db")
SCHED_DB = os.path.expanduser("~/.hermes/coding-hermes/scheduler.db")
NS_ROOT = os.path.expanduser("~/duckbrain/namespaces")


def day_bounds(target):
    """UTC midnight -> (start_epoch, end_epoch) for the target datetime.date."""
    start = datetime.datetime(target.year, target.month, target.day, tzinfo=datetime.timezone.utc)
    return start.timestamp(), start.timestamp() + DAY


def iso_utc(epoch):
    return datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def open_ro(path):
    con = sqlite3.connect("file:%s?mode=ro" % path, uri=True, timeout=2)
    con.execute("PRAGMA busy_timeout=2000")
    return con


def epoch_day(ts, origin):
    return int((ts - origin) // DAY)


def count_telegram(target):
    """Daily telegram message counts for the last BASELINE_DAYS+1 UTC days."""
    end_of_target = day_bounds(target)[1]
    origin = end_of_target - DORMANT_WINDOW_DAYS * DAY
    con = open_ro(STATE_DB)
    try:
        rows = con.execute(
            "SELECT CAST((m.timestamp - ?) / ? AS INTEGER) AS d, COUNT(*) "
            "FROM messages m JOIN sessions s ON s.id = m.session_id "
            "WHERE s.source LIKE 'telegram%' AND m.timestamp >= ? AND m.timestamp < ? "
            "GROUP BY d",
            (origin, DAY, origin, end_of_target),
        ).fetchall()
    finally:
        con.close()
    return bucket(rows, origin, DORMANT_WINDOW_DAYS)


def count_ticks(target):
    """Daily scheduler tick counts. created_at is TEXT (ISO-8601 with offset or
    naive 'YYYY-MM-DD HH:MM:SS' — naive values are treated as UTC)."""
    end_of_target = day_bounds(target)[1]
    origin = end_of_target - DORMANT_WINDOW_DAYS * DAY
    con = open_ro(SCHED_DB)
    try:
        raw = [r[0] for r in con.execute("SELECT created_at FROM ticks").fetchall()]
    finally:
        con.close()
    per_day = {}
    for v in raw:
        if not v:
            continue
        try:
            dt = datetime.datetime.fromisoformat(v.strip())
        except ValueError:
            continue
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=datetime.timezone.utc)
        ts = dt.timestamp()
        if origin <= ts < end_of_target:
            per_day[epoch_day(ts, origin)] = per_day.get(epoch_day(ts, origin), 0) + 1
    return [(d, per_day.get(d, 0)) for d in range(DORMANT_WINDOW_DAYS)]


def bucket(rows, origin, days):
    per_day = {int(d): int(c) for d, c in rows}
    return [(d, per_day.get(d, 0)) for d in range(days)]


def series(rows):
    """Bucket rows (oldest first, target day LAST) -> [yesterday, d1..d7]."""
    counts = [c for _, c in rows]
    return [counts[-1]] + counts[:-1]


def count_duckbrain(target):
    """Per-namespace daily commit counts from the namespace git repos.

    One `git log --format=%ct` sweep per namespace; commits are bucketed into
    UTC days locally. Namespaces with zero commits in the whole window are
    dormant and skipped. Returns (active, dormant_count); active is a list of
    (namespace, daily_counts) with the same shape as the other destinations.
    """
    end_of_target = day_bounds(target)[1]
    origin = end_of_target - DORMANT_WINDOW_DAYS * DAY
    active, dormant = [], 0
    if not os.path.isdir(NS_ROOT):
        return active, dormant, 0
    for ns in sorted(os.listdir(NS_ROOT)):
        repo = os.path.join(NS_ROOT, ns)
        if not os.path.isdir(os.path.join(repo, ".git")) and not os.path.exists(os.path.join(repo, "HEAD")):
            # a namespace dir is a git work tree; worktrees/.git files also qualify
            if not os.path.exists(os.path.join(repo, ".git")):
                dormant += 1
                continue
        try:
            out = subprocess.run(
                ["git", "-C", repo, "log", "--all", "--format=%ct",
                 "--since=" + iso_utc(origin)],
                capture_output=True, text=True, timeout=30, check=True,
            ).stdout
        except (subprocess.SubprocessError, OSError):
            dormant += 1
            continue
        epochs = [int(x) for x in out.split() if x.strip().isdigit()]
        if not epochs:
            dormant += 1
            continue
        per_day = {}
        for ts in epochs:
            if origin <= ts < end_of_target:
                d = epoch_day(ts, origin)
                per_day[d] = per_day.get(d, 0) + 1
        active.append((ns, [(d, per_day.get(d, 0)) for d in range(DORMANT_WINDOW_DAYS)]))
    return active, dormant, 0


def judge(counts):
    """counts = [yesterday, d1..d7] (most recent first). Returns a status string."""
    yesterday = counts[0]
    baseline = sum(counts[1:]) / float(BASELINE_DAYS)
    if yesterday == 0 and baseline >= 1:
        return "ALERT", baseline
    if baseline >= 4 and yesterday < 0.25 * baseline:
        return "ALERT", baseline
    if yesterday == 0 and baseline < 1:
        return "BASELINE-UNKNOWN", baseline
    return "OK", baseline


def check_destination(name, counts):
    status, baseline = judge(counts)
    return {
        "name": name,
        "yesterday": counts[0],
        "baseline7d_avg": round(baseline, 2),
        "status": status,
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--date", help="target UTC day (YYYY-MM-DD); default: yesterday")
    ap.add_argument("--simulate-dead", metavar="DEST",
                    help="force DEST's yesterday count to 0 to prove the alert path "
                         "(telegram | ticks | duckbrain | <namespace>)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    args = ap.parse_args()

    if args.date:
        try:
            target = datetime.date.fromisoformat(args.date)
        except ValueError:
            print("invalid --date (want YYYY-MM-DD): %s" % args.date, file=sys.stderr)
            return 2
    else:
        target = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=1)).date()

    results, errors = [], []

    # telegram
    try:
        results.append(check_destination("telegram", series(count_telegram(target))))
    except (sqlite3.Error, OSError) as exc:
        errors.append("telegram unreadable: %s" % exc)

    # scheduler ticks
    try:
        results.append(check_destination("ticks", series(count_ticks(target))))
    except (sqlite3.Error, OSError) as exc:
        errors.append("ticks unreadable: %s" % exc)

    # duckbrain namespaces: the pipeline-coverage signal is AGGREGATE commit
    # volume. Per-namespace days legitimately swing with the scheduler's
    # cooldowns (paused lanes write nothing for days), so a per-namespace
    # count is a WARN (context, non-fatal), not an ALERT.
    dormant = 0
    ns_results = []
    try:
        active, dormant, _ = count_duckbrain(target)
        for ns, counts in active:
            cs = series(counts)
            r = check_destination("duckbrain/" + ns, cs)
            if r["status"] == "ALERT":
                r["status"] = "WARN"
            if args.simulate_dead in ("duckbrain", ns):
                r["yesterday"] = 0
                r["simulated"] = True
                r["status"], _ = judge([0] + cs[1:])
                r["status"] = "WARN" if r["status"] == "ALERT" else r["status"]
            results.append(r)
            ns_results.append(r)
        if ns_results:
            total_yesterday = sum(r["yesterday"] for r in ns_results)
            total_baseline = sum(r["baseline7d_avg"] for r in ns_results)
            if total_yesterday == 0 and total_baseline >= 1:
                status = "ALERT"
            elif total_baseline >= 4 and total_yesterday < 0.25 * total_baseline:
                status = "ALERT"
            elif total_yesterday == 0 and total_baseline < 1:
                status = "BASELINE-UNKNOWN"
            else:
                status = "OK"
            results.append({
                "name": "duckbrain",
                "yesterday": total_yesterday,
                "baseline7d_avg": round(total_baseline, 2),
                "status": status,
                "scope": "aggregate of %d active namespaces" % len(ns_results),
            })
    except OSError as exc:
        errors.append("duckbrain namespaces unreadable: %s" % exc)

    for r in results:
        if args.simulate_dead == r["name"]:
            r["yesterday"] = 0
            r["simulated"] = True
            # re-judge with the forced-zero count against the untouched baseline
            r["status"] = "ALERT" if r["baseline7d_avg"] >= 1 else "BASELINE-UNKNOWN"

    for err in errors:
        results.append({"name": err.split(":")[0], "yesterday": None,
                        "baseline7d_avg": None, "status": "ALERT", "error": err})

    alerts = [r["name"] for r in results if r["status"] == "ALERT"]

    if args.json:
        print(json.dumps({
            "date": target.isoformat(),
            "destinations": results,
            "dormant_namespaces": dormant,
            "alerts": alerts,
        }, indent=2))
    else:
        for r in results:
            if r["yesterday"] is None:
                print("ALERT %s yesterday=unreadable baseline7d_avg=unreadable" % r["name"])
            else:
                print("%s %s yesterday=%d baseline7d_avg=%.2f%s" % (
                    r["status"], r["name"], r["yesterday"], r["baseline7d_avg"],
                    " [simulated]" if r.get("simulated") else ""))
        if dormant:
            print("dormant namespaces skipped: %d" % dormant)
        if alerts:
            print("ALERT: destination coverage failure: %s" % ", ".join(alerts))
    return 1 if alerts else 0


if __name__ == "__main__":
    sys.exit(main())
