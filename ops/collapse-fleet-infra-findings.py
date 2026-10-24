#!/usr/bin/env python3
"""collapse-fleet-infra-findings.py — collapse the per-project copies of a
fleet-infra finding into ONE shared-infra row (SCHED-GAP-128, migration).

WHAT IT DOES, per run:

  1. Discovers every project board (scheduler DB workdirs + ``<home>/*`` scan;
     linked git worktrees are EXCLUDED — their boards are copies of an owner
     repo's board, and editing another agent's checkout is neither safe nor
     meaningful).
  2. Classifies every row through ``ops/fleet-infra-findings.py``
     (``pattern`` + required substrate co-signal). Rows that classify
     project-local are LEFT ALONE — a repo's own stale binary or port bug is
     real project work, and sweeping it into the fleet bucket would destroy
     findings, which is worse than the noise this removes.
  3. Groups the fleet-infra rows by pattern and, per pattern, files ONE
     ``FLEET-INFRA-N`` row on the shared-infra board. Its ``cross_ref`` names
     every project and row id that was collapsed, so "17 boards saw it" is
     still visible from the single row — the consolidation must lose no
     provenance.
  4. Removes the collapsed rows from the project boards with an atomic
     rewrite (temp + rename) after taking a byte backup, and appends the FULL
     removed rows to an archive JSONL so nothing is destroyed even in a
     mistyped run.

MODES. ``--dry-run`` is the DEFAULT and writes nothing. ``--apply`` performs
 the mutation only with the explicit ``--cross-project`` acknowledgement.
 ``--include-closed`` also collapses closed copies (the default leaves closed
 history alone: it is history, not queued noise — the AC's grep does not care,
 an operator's audit trail does).

IDEMPOTENT. A second ``--apply`` over an already-collapsed fleet finds no
fleet-infra rows on project boards and mints nothing: the run reports
``changed_boards: []``. Re-running is the safe default after a partial
failure.

ARCHIVE. Default ``~/.hermes/coding-hermes/fleet-infra-collapse/<UTC>.jsonl``
(``--archive`` overrides). Every archived line is the original row verbatim
plus ``_collapsed`` provenance (board, line number, consolidated row id).

EXIT CODES. 0 = ran (report printed; a collapse that changed nothing is still
0); 1 = usage/IO error; 2 = no shared-infra board resolvable (nothing was
touched).

USAGE
  python3 ops/collapse-fleet-infra-findings.py                    # dry run
  python3 ops/collapse-fleet-infra-findings.py --apply            # migrate
  python3 ops/collapse-fleet-infra-findings.py --apply --sweep    # tick-safe:
      # suppress consolidation rows + events, print one line, never fail a tick
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
LIB_PATH = os.path.join(HERE, "fleet-infra-findings.py")
DEFAULT_ARCHIVE_DIR = "~/.hermes/coding-hermes/fleet-infra-collapse"
DEFAULT_BACKUP_DIR = "~/.hermes/coding-hermes/fleet-infra-backups"


def _load_lib():
    """Import the library by path (hyphenated filename is not a module name)."""
    spec = importlib.util.spec_from_file_location("fleet_infra_findings", LIB_PATH)
    if spec is None or spec.loader is None:  # pragma: no cover - deployment fault
        raise RuntimeError("cannot load %s" % LIB_PATH)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


lib = _load_lib()


def _now_iso(now: float) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))


def _archive_path(explicit: str | None, now: float) -> str:
    if explicit:
        return os.path.abspath(os.path.expanduser(explicit))
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime(now))
    return os.path.join(os.path.expanduser(DEFAULT_ARCHIVE_DIR),
                        "collapse-%s.jsonl" % stamp)


def collect_candidates(boards: list[str], include_closed: bool = False) -> dict:
    """Group collapsible rows by pattern, keeping each row's origin line.

    Returns ``{pattern: [candidate, ...]}`` where a candidate carries the
    board, the physical LINE INDEX (so the rewriter removes exactly that
    line), the parsed row, and its status bucketing.
    """
    open_statuses = lib.OPEN_STATUSES
    by_pattern: dict[str, list[dict]] = {}
    for board in boards:
        signature = lib.file_signature(board)
        for idx, line in enumerate(lib.read_board_lines(board)):
            if not line.strip().startswith("{"):
                continue
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if not isinstance(row, dict):
                continue
            pattern = lib.row_pattern(row)
            if not pattern:
                continue
            status = str(row.get("status", "")).lower()
            is_open = status in open_statuses
            if not is_open and not include_closed:
                continue
            by_pattern.setdefault(pattern, []).append({
                "board": board, "line_index": idx, "row": row,
                "status": status, "open": is_open, "raw": line,
                "signature": signature,
            })
    return by_pattern


def consolidate(pattern: str, candidates: list[dict], infra_board: str,
                apply: bool, now: float, reserved: set[str] | None = None,
                backup_root: str | None = None,
                infra_backups: dict[str, str] | None = None) -> dict:
    """File (or extend) the ONE shared row for *pattern*. Returns its record.

    ``reserved`` carries ids this RUN has already minted: a dry run appends
    nothing, so without it two patterns would both be told "FLEET-INFRA-1"
    and the report would name a row that does not exist.
    """
    reserved = reserved if reserved is not None else set()
    rows = lib.read_board(infra_board)
    taken = {str(r.get("id", "")) for r in rows} | reserved
    open_rows = [r for r in rows
                 if str(r.get("status", "")).lower() in lib.OPEN_STATUSES
                 and lib.is_infra_carrier(r) and lib.row_pattern(r) == pattern]
    observations = []
    for cand in candidates:
        row = cand["row"]
        observations.append({
            "project": project_of(cand["board"]),
            "board": cand["board"],
            "row_id": str(row.get("id", "")),
            "status": cand["status"],
            "priority": str(row.get("priority") or ""),
            "title": str(row.get("title", ""))[:160],
            "observed_at": str(row.get("created_at") or "")[:40],
        })

    if open_rows:
        target = open_rows[0]
        row_id = str(target.get("id", ""))
        action = "extended"
        if apply:
            if backup_root:
                _ensure_prewrite_backup(infra_board, backup_root, now,
                                        infra_backups if infra_backups is not None else {})
            _extend_cross_ref(infra_board, row_id, pattern, observations, now,
                              backup_root=backup_root)
    else:
        row_id = lib.mint_infra_id(taken)
        action = "created"
        if apply:
            if backup_root:
                _ensure_prewrite_backup(infra_board, backup_root, now,
                                        infra_backups if infra_backups is not None else {})
            _write_consolidated_row(infra_board, row_id, pattern, observations, now)
    return {"pattern": pattern, "row_id": row_id, "action": action,
            "observations": observations, "count": len(observations)}


def project_of(board_path: str) -> str:
    """Project name from a board path: the repo directory's basename."""
    parts = os.path.abspath(board_path).split(os.sep)
    try:
        board_idx = parts.index(".coding-hermes")
    except ValueError:
        return os.path.basename(os.path.dirname(os.path.dirname(os.path.dirname(board_path))))
    return parts[board_idx - 1]


def _backup_stamp(now: float) -> str:
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime(now))


def _ensure_prewrite_backup(board: str, backup_root: str, now: float,
                            backups: dict[str, str]) -> str:
    """Create one project-scoped, byte-identical snapshot before the first write."""
    if board in backups:
        return backups[board]
    target_dir = os.path.join(os.path.expanduser(backup_root), project_of(board))
    os.makedirs(target_dir, exist_ok=True)
    stamp_time = now
    target = os.path.join(target_dir, "tasks.jsonl.bak-%s" % _backup_stamp(stamp_time))
    while os.path.exists(target):
        stamp_time += 1
        target = os.path.join(target_dir, "tasks.jsonl.bak-%s" % _backup_stamp(stamp_time))
    with open(board, "rb") as src:
        data = src.read()
    with open(target, "xb") as dst:
        dst.write(data)
    backups[board] = target
    return target


def _write_consolidated_row(infra_board: str, row_id: str, pattern: str,
                            observations: list[dict], now: float) -> None:
    projects = sorted({o["project"] for o in observations})
    open_count = sum(1 for o in observations if o["status"] in lib.OPEN_STATUSES)
    # Priority is the STRONGEST the copies carried: several boards filed the
    # same host condition as P1, and consolidating must not demote it.
    priorities = [str(o.get("priority") or "") for o in observations if o.get("priority")]
    priority = "P1" if "P1" in priorities else (priorities[0] if priorities else "P2")
    row = {
        "id": row_id,
        "title": ("[fleet-infra:%s] Shared-substrate condition consolidated from "
                  "%d project board row(s) across %d project(s)"
                  % (pattern, len(observations), len(projects))),
        "status": "pending",
        "priority": priority,
        "origin": lib.ORIGIN_CONSOLIDATED,
        "classification": lib.FLEET + lib.LABEL_SEP + pattern,
        "created_at": _now_iso(now),
        "reasoning": (
            "SCHED-GAP-128 consolidation: the same %s condition was filed as a "
            "per-project row on %d board(s) (%s). It is a property of the SHARED "
            "substrate, so it is carried here ONCE; the per-project copies were "
            "collapsed into cross_ref.observations (full rows archived) and "
            "%d of them were open at collapse time. One fix, one row."
            % (pattern, len(projects), ", ".join(projects[:12]) +
               ("…" if len(projects) > 12 else ""), open_count)),
        "cross_ref": {"pattern": pattern, "consolidated_at": _now_iso(now),
                      "observations": observations},
    }
    lib.append_jsonl(infra_board, [row])
    events = lib.events_path_for(infra_board)
    if os.path.isdir(os.path.dirname(events)):
        try:
            lib.append_jsonl(events, [{
                "id": lib.next_event_id(events), "kind": "task_added",
                "task_id": row_id, "source": lib.ORIGIN_CONSOLIDATED,
                "detail": "%s consolidated from %d project row(s)"
                          % (pattern, len(observations)),
                "ts": _now_iso(now),
            }])
        except OSError:
            pass


def _extend_cross_ref(infra_board: str, row_id: str, pattern: str,
                      observations: list[dict], now: float,
                      backup_root: str | None = None) -> None:
    """Append observations to an existing open consolidated row (one line rewrite)."""
    lines = lib.read_board_lines(infra_board)
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
            cr = {"pattern": pattern}
            row["cross_ref"] = cr
        obs = cr.get("observations")
        if not isinstance(obs, list):
            obs = []
            cr["observations"] = obs
        known = {(o.get("project"), o.get("row_id")) for o in obs if isinstance(o, dict)}
        for observation in observations:
            if (observation["project"], observation["row_id"]) in known:
                continue
            obs.append(observation)
        cr["last_collapsed_at"] = _now_iso(now)
        lines[idx] = json.dumps(row, ensure_ascii=False)
        break
    else:  # pragma: no cover - the row vanished between read and write
        return
    backup_root = backup_root or DEFAULT_BACKUP_DIR
    backup_dir = os.path.join(os.path.expanduser(backup_root), project_of(infra_board))
    lib.write_board_lines(infra_board, lines, backup_dir=backup_dir, make_backup=False)


def rewrite_board(board: str, drop: set[int], backup_dir: str | None = None,
                  expect: tuple[int, int] | None = None) -> str:
    """Remove the physical lines at *drop*; return the backup path.

    Every untouched line is re-emitted byte-identically: a board carries other
    agents' just-appended rows and a rewrite that re-serialises them would
    reformat (and could reorder) work this tool does not own.

    The backup goes to a CENTRAL directory by default, not beside the board: a
    ``tasks.jsonl.bak-*`` dropped into 15 live repos is untracked noise the next
    board-commit sweeps up.
    """
    lines = lib.read_board_lines(board)
    kept = [line for idx, line in enumerate(lines) if idx not in drop]
    if len(kept) == len(lines):
        return ""
    if backup_dir is None:
        project = project_of(board)
        backup_dir = os.path.join(os.path.expanduser(DEFAULT_BACKUP_DIR), project)
        os.makedirs(backup_dir, exist_ok=True)
    return lib.write_board_lines(board, kept, backup_dir=backup_dir, expect=expect)


def run(boards: list[str] | None = None, infra_board: str | None = None,
        apply: bool = False, include_closed: bool = False,
        archive: str | None = None, home: str | None = None,
        include_worktrees: bool = False, now: float | None = None,
        backup_dir: str | None = None, cross_project: bool = False) -> dict:
    """Collapse the fleet. Returns the report dict (also printed by the CLI).

    ``backup_dir`` overrides where the pre-rewrite board copies land (default
    ``~/.hermes/coding-hermes/fleet-infra-backups/<project>``). Tests pass a
    tmp directory — a fixture run must never leave artefacts in the operator's
    backup store.
    """
    now = time.time() if now is None else now
    boards = boards if boards is not None else lib.discover_boards(
        home=home, include_worktrees=include_worktrees)
    # An EXPLICIT board must resolve to a real file: a mistyped path would be
    # CREATED by the first append and every consolidated row would land in a
    # file nobody reads.
    infra_board = (lib.resolve_infra_board(infra_board) if infra_board
                   else lib.resolve_infra_board(home=home))
    report: dict = {
        "mode": "apply" if apply else "dry-run",
        "boards_scanned": len(boards),
        "infra_board": infra_board or "",
        "collapsed_rows": 0,
        "patterns": [],
        "changed_boards": [],
        "writes": [],
        "infra_backup": "",
        "archive": "",
        "cross_project": False,
        "skipped": {"local_rows": 0, "closed_rows": 0},
        "errors": [],
    }

    if not infra_board:
        report["errors"].append("no shared-infra board resolvable — nothing touched")
        return report

    by_pattern = collect_candidates(boards, include_closed=include_closed)
    if not by_pattern:
        return report

    # Applying a collapse rewrites project boards as well as the shared board.
    # Require an explicit acknowledgement before even constructing a mutating
    # consolidation plan; this keeps a refused run byte- and mtime-stable.
    candidate_boards = {
        cand["board"]
        for candidates in by_pattern.values()
        for cand in candidates
        if os.path.realpath(cand["board"]) != os.path.realpath(infra_board)
    }
    cross_project_write = bool(candidate_boards)
    report["cross_project"] = cross_project_write
    if apply and cross_project_write and not cross_project:
        report["errors"].append(
            "REFUSED: cross-project board writes require explicit --cross-project acknowledgement"
        )
        return report

    archive_path = _archive_path(archive, now)
    backup_root = os.path.expanduser(backup_dir or DEFAULT_BACKUP_DIR)
    infra_backups: dict[str, str] = {}
    archived: list[dict] = []
    reserved: set[str] = set()
    # Drops are collected across ALL patterns and applied in ONE rewrite per
    # board. Rewriting inside the pattern loop removed line indices that the
    # previous pattern's rewrite had already shifted — measured: the second
    # pattern's collapse deleted the wrong line and left its own row behind.
    drop_by_board: dict[str, set[int]] = {}
    signature_by_board: dict[str, tuple[int, int] | None] = {}
    for pattern in sorted(by_pattern):
        candidates = by_pattern[pattern]
        # Never collapse a row that already lives on the infra board.
        candidates = [c for c in candidates
                      if os.path.realpath(c["board"]) != os.path.realpath(infra_board)]
        if not candidates:
            continue
        record = consolidate(pattern, candidates, infra_board, apply, now,
                             reserved=reserved, backup_root=backup_root,
                             infra_backups=infra_backups)
        reserved.add(record["row_id"])
        report["patterns"].append(record)
        report["collapsed_rows"] += len(candidates)
        for cand in candidates:
            drop_by_board.setdefault(cand["board"], set()).add(cand["line_index"])
            signature_by_board[cand["board"]] = cand.get("signature")
            archived.append(dict(cand["row"], _collapsed={
                "consolidated_row": record["row_id"], "pattern": pattern,
                "board": cand["board"], "line": cand["line_index"],
                "at": _now_iso(now),
            }))
    if apply:
        for board, drop in sorted(drop_by_board.items()):
            try:
                backup = rewrite_board(board, drop, expect=signature_by_board.get(board),
                                       backup_dir=(os.path.join(backup_dir, project_of(board))
                                                   if backup_dir else None))
            except lib.BoardChangedError as exc:
                # A live lane appended while we were classifying. Its row is
                # not in our line map, so writing would drop it.
                report["errors"].append("SKIPPED (concurrent write): %s" % exc)
                continue
            except (OSError, ValueError) as exc:
                report["errors"].append("%s: %s" % (board, exc))
                continue
            report["changed_boards"].append({
                "board": board, "removed": len(drop), "backup": backup,
            })

    if infra_backups:
        report["infra_backup"] = infra_backups.get(infra_board, "")

    if apply and archived:
        archive_path = os.path.abspath(os.path.expanduser(archive_path))
        os.makedirs(os.path.dirname(archive_path), exist_ok=True)
        lib.append_jsonl(archive_path, archived)
        report["archive"] = archive_path
    elif archived:
        report["archive"] = "(dry-run: %d row(s) would be archived to %s)" % (
            len(archived), archive_path)
    report["archived_rows"] = len(archived)
    return report


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and argv[0] in ("-h", "--help"):
        print(__doc__ or "")
        return 0
    opts = lib._parse_opts(argv, {"--infra-board", "--archive", "--home", "--boards"})
    boards = [b for b in opts["--boards"].split(",") if b] if opts.get("--boards") else None
    apply = "--apply" in argv
    sweep = "--sweep" in argv
    report = run(boards=boards, infra_board=opts.get("--infra-board"), apply=apply,
                 include_closed="--include-closed" in argv,
                 archive=opts.get("--archive"), home=opts.get("--home"),
                 include_worktrees="--include-worktrees" in argv,
                 cross_project="--cross-project" in argv)
    if not report["infra_board"]:
        print(json.dumps(report, indent=1))
        return 2
    if sweep:
        # Tick-safe shape: one line, never an error exit for a no-op run.
        if report["changed_boards"]:
            written_items = [
                "%s[%s]" % (
                    report["infra_board"],
                    ",".join(record["row_id"] for record in report["patterns"]),
                )
            ]
            written_items.extend(
                "%s[%s]" % (
                    item["board"],
                    ",".join(
                        record["row_id"] for record in report["patterns"]
                        if any(obs["board"] == item["board"]
                               for obs in record["observations"])
                    ) or "none",
                ) for item in report["changed_boards"]
            )
            detail = " wrote=%s" % ",".join(written_items)
        else:
            detail = " wrote nothing"
        if report["errors"]:
            detail += " error=%s" % "; ".join(report["errors"])
        print("FLEET-INFRA-COLLAPSE %s collapsed=%d patterns=%d boards=%d%s"
              % (report["mode"], report["collapsed_rows"],
                 len(report["patterns"]), len(report["changed_boards"]), detail))
        return 0
    print(json.dumps(report, indent=1))
    return 1 if report["errors"] and not report["changed_boards"] else 0


if __name__ == "__main__":
    sys.exit(main())
