#!/usr/bin/env python3
"""Validate docs/stuck-matrix.json — the fleet's stuck-state routing table.

The matrix exists so a blocked lane resolves reversible blockers itself instead
of waiting for a human. That property is only real if the table cannot quietly
rot into "ask the owner for everything", so this checker enforces the four things
that make the difference:

  1. every class states a symptom and carries a signature a lane can measure;
  2. every REVERSIBLE class names a non-human resolver AND at least one runnable
     command — prose is not a resolver;
  3. NO reversible class is routed to the owner (that is the regression this
     file exists to catch);
  4. every class states its escalation condition, so "escalate" is a measured
     trigger rather than a default.

Exit codes: 0 clean, 1 matrix is invalid, 2 matrix missing or unparseable.
Run from the repo root: python3 scripts/check_stuck_matrix.py
"""

from __future__ import annotations

import json
import pathlib
import sys

MATRIX = pathlib.Path("docs/stuck-matrix.json")

# Owner names that must never appear as the resolver of a reversible class.
OWNER_TOKENS = ("owner", "bane", "human")


def fail(msg: str) -> "None":
    print(f"stuck-matrix: {msg}", file=sys.stderr)


def main() -> int:
    if not MATRIX.is_file():
        fail(f"missing {MATRIX}")
        return 2
    try:
        data = json.loads(MATRIX.read_text())
    except json.JSONDecodeError as exc:
        fail(f"{MATRIX} is not valid JSON: {exc}")
        return 2

    classes = data.get("classes")
    if not isinstance(classes, list) or not classes:
        fail("no classes[] entries")
        return 1

    errors: list[str] = []
    see = [0]
    for i, entry in enumerate(classes):
        where = f"classes[{i}]"
        if not isinstance(entry, dict):
            errors.append(f"{where}: not an object")
            continue
        symptom = (entry.get("symptom") or "").strip()
        signature = (entry.get("signature") or "").strip()
        resolver = (entry.get("resolver") or "").strip()
        commands = entry.get("commands")
        escalate = (entry.get("escalate_when") or "").strip()
        reversible = entry.get("reversible")

        if not symptom:
            errors.append(f"{where}: empty symptom")
        else:
            where = f"{where} ({symptom[:48]})"
        if not signature:
            errors.append(f"{where}: empty signature — a symptom nobody can measure is not routable")
        if not isinstance(reversible, bool):
            errors.append(f"{where}: 'reversible' must be a boolean")
        if not escalate:
            errors.append(f"{where}: empty escalate_when — escalation must be a measured trigger")
        if not isinstance(commands, list):
            errors.append(f"{where}: 'commands' must be a list")
            commands = []

        if reversible is True:
            if not resolver:
                errors.append(f"{where}: reversible class has no resolver")
            elif any(tok in resolver.lower() for tok in OWNER_TOKENS):
                errors.append(
                    f"{where}: reversible class routed to a human ({resolver!r}) "
                    "— reversible blockers are resolved by the fleet"
                )
            if not commands:
                errors.append(f"{where}: reversible class has no runnable command (prose is not a resolver)")
            for cmd in commands:
                if not isinstance(cmd, str) or not cmd.strip():
                    errors.append(f"{where}: empty command entry")

        if reversible is False:
            if not any(tok in resolver.lower() for tok in OWNER_TOKENS):
                # An irreversible class may be routed to a lane only if that lane
                # holds the policy authority; require the resolver to say so.
                errors.append(
                    f"{where}: irreversible class resolver {resolver!r} does not name the owner "
                    "— irreversible actions escalate to the owner"
                )
            see[0] += 1

    if see[0] == 0:
        errors.append("no irreversible class — every matrix needs the 'do not touch this' column")

    if errors:
        fail(f"{len(errors)} problem(s):")
        for e in errors:
            print(f"  - {e}", file=sys.stderr)
        return 1

    reversible = sum(1 for e in classes if e.get("reversible") is True)
    print(f"stuck-matrix: OK — {len(classes)} classes ({reversible} reversible, {see[0]} owner-only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
