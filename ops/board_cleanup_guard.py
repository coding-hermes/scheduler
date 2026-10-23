#!/usr/bin/env python3
"""Board cleanup guard — refuse a mass-close that drops rows with no acceptance criteria.

Why this exists (SCHED-GAP-1757). On 2026-10-22 a foreman tick ran a board
cleanup that closed 43 rows with the blanket note

    "Closed as stale: pending with zero acceptance criteria. No actionable work."

and ``status="stale"``. Three things were wrong at once:

  1. ``stale`` is not a legal board status (``BOARD_ALLOWED_STATUSES`` is
     pending / in_progress / complete / duplicate, mirrored by
     ``internal/scheduler/board_vocab.go``), so the rows read OPEN to every
     reader — ``board_awareness.go`` counts ``status == "pending"`` — but only
     ``pending`` is dispatchable. Work was parked visibly and picked by nobody,
     and ``ops/check-fleet-invariants.py --board-only`` went red.
  2. The premise was false: a census over ``git show <cleanup>^`` proves 42 of
     the 43 rows carried a non-empty acceptance criteria field BEFORE the
     cleanup. The sweep closed criteria-bearing, dispatchable work on evidence
     its own commit contradicted.
  3. The doctrine it violated is fleet law: a row without acceptance criteria
     gets criteria BACKFILLED (board-dispatchability-backfill), not deleted. A
     gap must get acceptance criteria, not a silent close.

This module is the mechanical half of that law. A cleanup helper that is about
to flip N rows to a closed status must call :func:`evaluate` first; the guard
refuses when any row in the selection carries no criteria, and refuses a
single-row close that carries neither criteria nor completion evidence. The
override is a human deciding per row, not a bulk flag.

Usage (CLI — the shape a foreman/supervisor cleanup should call):

    python3 ops/board_cleanup_guard.py --board .coding-hermes/board/tasks.jsonl \\
        --ids-file /tmp/stale-ids.txt --status stale

Exit codes: 0 = allowed, 3 = refused by policy, 2 = cannot evaluate (fail
closed: unreadable board, or a selection that matched no open row).

The guard is read-only. It never writes the board.
"""
from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field

# Board writer vocabulary (check 8 of ops/check-fleet-invariants.py). Kept in
# lockstep by hand with internal/scheduler/board_vocab.go; the fixture battery
# test_check_fleet_invariants_board_vocab.py pins the same spellings.
BOARD_ALLOWED_STATUSES = ("pending", "in_progress", "complete", "duplicate")

# Statuses that take a row OUT of the open set.
CLOSED_STATUSES = ("complete", "duplicate")

# A close of this many rows is a MASS close: the shape that needs criteria on
# every row, because nobody will review them one by one.
MASS_CLOSE_MIN = 2

# Field names that carry acceptance criteria across the fleet's boards. The
# scheduler board uses ``acceptance``; other projects use
# ``acceptance_criteria`` as a string or a list of strings.
ACCEPTANCE_KEYS = ("acceptance", "acceptance_criteria", "acceptance_criteria_text")

# Field names that carry completion evidence. A single-row close may skip
# criteria ONLY when it names what was done; a bare status flip may not.
EVIDENCE_KEYS = ("worker_summary", "commit_hash", "foreman_note", "evidence")

# verdict codes
ALLOWED = "allowed"
REFUSED_ILLEGAL_STATUS = "refused-status-not-in-vocabulary"
REFUSED_MASS_CRITERIA_LESS = "refused-mass-close-of-criteria-less-rows"
REFUSED_SINGLE_NO_SUPPORT = "refused-single-close-without-criteria-or-evidence"
NO_SELECTION = "no-open-row-selected"


def _as_text(value) -> str:
    """Flatten a criteria/evidence field to non-empty text ('' when empty).

    Boards in the fleet store these as a string (``acceptance``) or a list of
    strings (``acceptance_criteria``); both count as criteria. An empty list, a
    list of blank strings, ``None`` and a missing key all mean "no criteria".
    """
    if value is None:
        return ""
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, (list, tuple)):
        parts = [str(v).strip() for v in value if str(v).strip()]
        return " ".join(parts)
    if isinstance(value, dict):
        parts = [str(v).strip() for v in value.values() if str(v).strip()]
        return " ".join(parts)
    return str(value).strip()


def criteria_text(row: dict) -> str:
    """Return the row's acceptance criteria as non-empty text, or ''."""
    for key in ACCEPTANCE_KEYS:
        text = _as_text(row.get(key))
        if text:
            return text
    return ""


def evidence_text(row: dict) -> str:
    """Return the row's completion evidence as non-empty text, or ''."""
    for key in EVIDENCE_KEYS:
        text = _as_text(row.get(key))
        if text:
            return text
    return ""


def row_id(row: dict) -> str:
    """Return the row's id (``id`` with ``task_id`` as the legacy alias)."""
    return str(row.get("id") or row.get("task_id") or "")


def is_closed(row: dict) -> bool:
    """True when the row already sits at a closed status."""
    return str(row.get("status") or "") in CLOSED_STATUSES


def load_board(path: str) -> list[dict]:
    """Read a line-wise JSONL board and return its row objects.

    Blank lines are skipped. A line that is not a JSON object is a hard error:
    a damaged board must be repaired before any cleanup, never guessed at.
    """
    rows: list[dict] = []
    with open(path, "r", encoding="utf-8", errors="replace") as handle:
        for lineno, line in enumerate(handle, 1):
            text = line.strip()
            if not text:
                continue
            try:
                obj = json.loads(text)
            except json.JSONDecodeError as exc:
                raise ValueError(f"{path}:{lineno}: not JSON ({exc})") from exc
            if not isinstance(obj, dict):
                raise ValueError(f"{path}:{lineno}: not a JSON object")
            rows.append(obj)
    return rows


@dataclass
class Verdict:
    """The guard's answer for one proposed close."""

    code: str
    target_status: str
    candidates: list[str] = field(default_factory=list)
    criteria_less: list[str] = field(default_factory=list)
    unsupported_single: list[str] = field(default_factory=list)
    detail: str = ""

    @property
    def allowed(self) -> bool:
        """True when the close may proceed."""
        return self.code == ALLOWED

    @property
    def refused(self) -> bool:
        """True when policy refuses the close."""
        return self.code.startswith("refused")

    @property
    def exit_code(self) -> int:
        """0 allowed, 3 refused, 2 cannot evaluate (fail closed)."""
        if self.allowed:
            return 0
        if self.refused:
            return 3
        return 2

    def as_dict(self) -> dict:
        """Machine-readable verdict (``--json``)."""
        return {
            "code": self.code,
            "allowed": self.allowed,
            "target_status": self.target_status,
            "candidates": self.candidates,
            "criteria_less": self.criteria_less,
            "unsupported_single": self.unsupported_single,
            "detail": self.detail,
            "exit_code": self.exit_code,
        }

    def lines(self) -> list[str]:
        """Human-readable verdict lines, one fact per line."""
        head = "GUARD: %s (%s, target=%s, candidates=%d, criteria-less=%d)" % (
            "ALLOWED" if self.allowed else "REFUSED",
            self.code,
            self.target_status,
            len(self.candidates),
            len(self.criteria_less),
        )
        out = [head]
        if self.criteria_less:
            out.append("GUARD:   criteria-less: " + ", ".join(self.criteria_less))
        if self.unsupported_single:
            out.append("GUARD:   no criteria and no completion evidence: "
                       + ", ".join(self.unsupported_single))
        if self.detail:
            out.append("GUARD:   " + self.detail)
        return out


def evaluate(rows: list[dict], ids=None, target_status: str = "complete") -> Verdict:
    """Judge a proposed close of ``ids`` (or every open row) to ``target_status``.

    Refusal rules, in order:

    * the target status must be in the board vocabulary — a close that writes
      an unknown spelling parks work invisibly and reds the invariant gate;
    * a MASS close (``MASS_CLOSE_MIN`` rows or more) is refused when ANY
      selected row has no acceptance criteria, and every offender is named;
    * a single-row close without criteria is refused unless the row carries
      completion evidence (a summary, a commit, a foreman note or an evidence
      blob) — "closed, no reason" is the silent close the doctrine forbids.

    Rows already at a closed status are not candidates: the guard judges the
    new close, not the board's history.
    """
    wanted = None if ids is None else {str(i) for i in ids}
    candidates = [
        row
        for row in rows
        if not is_closed(row) and (wanted is None or row_id(row) in wanted)
    ]
    # Deterministic order keeps the printed offender list stable.
    candidates.sort(key=row_id)
    cand_ids = [row_id(row) for row in candidates]

    if str(target_status) not in BOARD_ALLOWED_STATUSES:
        return Verdict(
            code=REFUSED_ILLEGAL_STATUS,
            target_status=str(target_status),
            candidates=cand_ids,
            detail="legal statuses are: " + ", ".join(BOARD_ALLOWED_STATUSES),
        )

    if not candidates:
        return Verdict(
            code=NO_SELECTION,
            target_status=str(target_status),
            detail="no open row matched the selection — nothing would be closed",
        )

    criteria_less = [row_id(row) for row in candidates if not criteria_text(row)]

    if len(candidates) >= MASS_CLOSE_MIN and criteria_less:
        return Verdict(
            code=REFUSED_MASS_CRITERIA_LESS,
            target_status=str(target_status),
            candidates=cand_ids,
            criteria_less=criteria_less,
            detail=(
                "a mass close must not drop rows with zero acceptance criteria; "
                "backfill the criteria (board-dispatchability-backfill) or close "
                "each row individually with completion evidence"
            ),
        )

    if len(candidates) < MASS_CLOSE_MIN and criteria_less:
        unsupported = [
            row_id(row)
            for row in candidates
            if not criteria_text(row) and not evidence_text(row)
        ]
        if unsupported:
            return Verdict(
                code=REFUSED_SINGLE_NO_SUPPORT,
                target_status=str(target_status),
                candidates=cand_ids,
                criteria_less=criteria_less,
                unsupported_single=unsupported,
                detail=(
                    "a single row may be closed without criteria only when it "
                    "names completion evidence (worker_summary/commit_hash/"
                    "foreman_note/evidence)"
                ),
            )

    return Verdict(
        code=ALLOWED,
        target_status=str(target_status),
        candidates=cand_ids,
        criteria_less=criteria_less,
    )


def _read_ids(args) -> list[str] | None:
    """Resolve the ``--ids`` / ``--ids-file`` selection (None = all open rows)."""
    ids: list[str] = []
    for chunk in args.ids or []:
        ids.extend(part.strip() for part in chunk.split(",") if part.strip())
    for path in args.ids_file or []:
        with open(path, "r", encoding="utf-8") as handle:
            for line in handle:
                line = line.split("#", 1)[0].strip()
                if line:
                    ids.append(line)
    return ids or None


def main(argv=None) -> int:
    """CLI entry point — judge a proposed close and print the verdict."""
    parser = argparse.ArgumentParser(
        description="Refuse a board mass-close that drops rows with no acceptance criteria."
    )
    parser.add_argument("--board", required=True, help="path to the JSONL board")
    parser.add_argument("--ids", action="append",
                        help="row id(s) being closed (repeatable, comma-separated)")
    parser.add_argument("--ids-file", action="append",
                        help="file with one row id per line ('#' comments allowed)")
    parser.add_argument("--status", default="complete",
                        help="status the cleanup intends to write (default: complete)")
    parser.add_argument("--json", action="store_true", help="print the verdict as JSON")
    args = parser.parse_args(argv)

    try:
        rows = load_board(args.board)
        verdict = evaluate(rows, ids=_read_ids(args), target_status=args.status)
    except (OSError, ValueError) as exc:
        print(f"GUARD: CANNOT-EVALUATE {exc}", file=sys.stderr)
        return 2

    if args.json:
        print(json.dumps(verdict.as_dict(), indent=2))
    else:
        for line in verdict.lines():
            print(line)
    return verdict.exit_code


if __name__ == "__main__":
    sys.exit(main())
