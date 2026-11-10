#!/usr/bin/env python3
"""Repair stale project failure counters from a bounded tick window.

The default is a read-only report. --apply is explicit and only lowers a
counter to the number of failed/timeout ticks in the selected window whose
failure_reason is not a transport class. This utility never raises a counter.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import sqlite3
from pathlib import Path

TRANSPORT_REASONS = {"gateway_transport", "gateway_drain"}


def _utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def _parse_time(value: str) -> str:
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")


def _connect(path: Path, apply: bool) -> sqlite3.Connection:
    if apply:
        return sqlite3.connect(str(path))
    uri = f"file:{path}?mode=ro"
    return sqlite3.connect(uri, uri=True)


def repair(path: Path, *, apply: bool = False, since: str | None = None,
           until: str | None = None, window_hours: float = 72.0) -> dict:
    if window_hours <= 0:
        raise ValueError("window-hours must be positive")
    end = _parse_time(until) if until else _utc_now().strftime("%Y-%m-%dT%H:%M:%S")
    start = _parse_time(since) if since else (
        _utc_now() - dt.timedelta(hours=window_hours)
    ).strftime("%Y-%m-%dT%H:%M:%S")
    conn = _connect(path, apply)
    try:
        rows = conn.execute(
            """
            SELECT p.name, p.consecutive_failures,
                   COUNT(t.id) AS eligible_failures
              FROM projects AS p
              LEFT JOIN ticks AS t
                ON t.project_name = p.name
               AND t.status IN ('failed', 'timeout')
               AND COALESCE(t.completed_at, t.spawned_at, t.created_at) >= ?
               AND COALESCE(t.completed_at, t.spawned_at, t.created_at) <= ?
               AND COALESCE(t.failure_reason, '') NOT IN (?, ?)
             GROUP BY p.name, p.consecutive_failures
            HAVING p.consecutive_failures > eligible_failures
             ORDER BY p.name
            """,
            (start, end, *TRANSPORT_REASONS),
        ).fetchall()
        changes = [
            {"project": name, "before": counter, "after": eligible,
             "eligible_failures": eligible}
            for name, counter, eligible in rows
        ]
        if apply and changes:
            with conn:
                for change in changes:
                    conn.execute(
                        "UPDATE projects SET consecutive_failures = ? "
                        "WHERE name = ? AND consecutive_failures > ?",
                        (change["after"], change["project"], change["after"]),
                    )
        return {"mode": "apply" if apply else "dry-run", "db": str(path),
                "since": start, "until": end, "changes": changes,
                "changed": len(changes)}
    finally:
        conn.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("db", type=Path)
    parser.add_argument("--window-hours", type=float, default=72.0)
    parser.add_argument("--since")
    parser.add_argument("--until")
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args(argv)
    if not args.db.is_file():
        parser.error(f"database does not exist: {args.db}")
    try:
        result = repair(args.db, apply=args.apply, since=args.since,
                        until=args.until, window_hours=args.window_hours)
    except (OSError, sqlite3.Error, ValueError) as exc:
        parser.error(str(exc))
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
