#!/usr/bin/env python3
"""Regenerate docs/fleet.md from the live schedulerd API.

Usage:
    python3 docs/regenerate_fleet.py [--api http://127.0.0.1:9090] [--out docs/fleet.md]

Queries GET /api/v1/status and GET /api/v1/projects and rewrites the fleet
status document so it never drifts from the running daemon again.
"""

import argparse
import json
import sys
import urllib.request
from datetime import datetime, timezone


def get_json(url):
    with urllib.request.urlopen(url, timeout=10) as resp:
        return json.load(resp)


def fetch_all_projects(base):
    """Fetch every project across the paginated /api/v1/projects endpoint.

    The endpoint is paginated (default limit 200); fetching one page silently
    truncates the fleet document (judge finding, README-17 tick 2026-10-14).
    """
    base = base.rstrip("/")
    projects = []
    offset = 0
    limit = 500
    while True:
        page = get_json(f"{base}/api/v1/projects?limit={limit}&offset={offset}")
        batch = page["projects"]
        projects.extend(batch)
        total = page.get("total")
        offset += len(batch)
        if not batch or (total is not None and offset >= total):
            break
    return projects


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://127.0.0.1:9090")
    ap.add_argument("--out", default="docs/fleet.md")
    args = ap.parse_args()

    status = get_json(args.api.rstrip("/") + "/api/v1/status")
    projects = fetch_all_projects(args.api)

    enabled = sorted(
        (p for p in projects if p["enabled"]),
        key=lambda p: (-p["priority"], p["name"].lower()),
    )
    disabled = sorted(
        (p for p in projects if not p["enabled"]),
        key=lambda p: p["name"].lower(),
    )

    now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    outcomes = status.get("recent_outcomes", {})
    duck = status.get("duckbrain", {})

    lines = []
    lines.append("# Coding Hermes Fleet — Live Status")
    lines.append("")
    lines.append(
        f"**Generated {now} from the live schedulerd API** "
        f"(`GET {args.api}/api/v1/status` + `/api/v1/projects`). "
        "Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh."
    )
    lines.append("")
    lines.append("## Settings (live)")
    lines.append("")
    lines.append("| Setting | Value |")
    lines.append("|---------|-------|")
    lines.append(f"| Active projects (enabled) | {status.get('active_projects', len(enabled))} |")
    lines.append(f"| Total projects (incl. disabled) | {len(projects)} |")
    lines.append(f"| Active ticks | {status.get('active_ticks', 0)} |")
    lines.append(f"| Budget | {status.get('budget_total', 100)} |")
    lines.append(f"| Last evaluation | {status.get('last_evaluation', 'n/a')} |")
    lines.append(
        "| Recent outcomes | "
        f"completed={outcomes.get('completed', 0)}, failed={outcomes.get('failed', 0)}, "
        f"timeout={outcomes.get('timeout', 0)} |"
    )
    if duck:
        lines.append(f"| DuckBrain sync | reachable={duck.get('reachable')}, "
                     f"spooled_pending={duck.get('spooled_pending', 0)} |")
    lines.append("")
    lines.append(f"## Fleet ({len(projects)} projects, {len(enabled)} enabled)")
    lines.append("")
    lines.append(f"### Enabled ({len(enabled)})")
    lines.append("")
    lines.append("| Project | Priority | Weight | Cooldown | Namespace |")
    lines.append("|---------|----------|--------|----------|-----------|")
    for p in enabled:
        lines.append(
            f"| {p['name']} | {p['priority']} | {p['weight']} | {p['cooldown_s']}s "
            f"| {p.get('namespace_id') or '-'} |"
        )
    lines.append("")
    lines.append(f"### Disabled ({len(disabled)})")
    lines.append("")
    lines.append("| Project | Priority | Weight | Cooldown | Namespace |")
    lines.append("|---------|----------|--------|----------|-----------|")
    for p in disabled:
        lines.append(
            f"| {p['name']} | {p['priority']} | {p['weight']} | {p['cooldown_s']}s "
            f"| {p.get('namespace_id') or '-'} |"
        )
    lines.append("")
    lines.append("## Live Dashboard")
    lines.append("")
    lines.append(f"Point a browser at {args.api}/ for the live HTML dashboard "
                 "(auto-refreshes; per-project detail, queue, tick history, health).")
    lines.append("")

    with open(args.out, "w") as f:
        f.write("\n".join(lines))

    print(f"wrote {args.out}: {len(projects)} projects, {len(enabled)} enabled, "
          f"{len(disabled)} disabled")
    return 0


if __name__ == "__main__":
    sys.exit(main())
