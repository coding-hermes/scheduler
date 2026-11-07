#!/usr/bin/env python3
"""board_stale_base_check.py — row-id extraction + comparison for the
board-commit-guard stale-base rule (RELEASE-046).

The shell guard dumps the STAGED (index) blob and the origin/main blob of one
board file to two temp files and invokes:

    board_stale_base_check.py <staged-file> <base-file>

Row identity is extracted generically, one JSON object per line:

  - tasks.jsonl rows carry string ids:   {"id": "SCHED-GAP-1699", ...}
  - events.jsonl rows carry integer ids: {"id": 42, ...}

Both shapes are handled by one code path (json.loads + str(id)); a line that
does not parse, or parses to something without an id, is skipped — never
fatal. Integer and string forms of the same number compare as their decimal
string, so 42 and "42" are the same identity within one file.

Exit codes:
  0  no row id present in <base-file> is missing from <staged-file>
  1  dropped ids found — printed one per line on stdout (sorted)
  2  usage or read error — message on stderr (the caller must fail closed)
"""

import json
import sys


def row_ids(path):
    ids = set()
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except ValueError:
                continue  # a line that does not parse is skipped, never fatal
            if isinstance(obj, dict) and obj.get("id") is not None:
                ids.add(str(obj["id"]))
    return ids


def main(argv):
    if len(argv) != 3:
        print(
            "usage: board_stale_base_check.py <staged-file> <base-file>",
            file=sys.stderr,
        )
        return 2
    try:
        staged = row_ids(argv[1])
        base = row_ids(argv[2])
    except OSError as exc:
        print(f"board_stale_base_check: cannot read input: {exc}", file=sys.stderr)
        return 2
    dropped = sorted(base - staged)
    if not dropped:
        return 0
    for row_id in dropped:
        print(row_id)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
