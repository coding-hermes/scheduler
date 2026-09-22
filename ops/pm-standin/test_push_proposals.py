"""SCHED-GAP-207 worker tests: unique id per filed finding (push_proposals.py).

Proves the PM stand-in push leg can no longer reuse a row id as a recurring
SLOT, on temp fixtures only (no live board/ledger is ever touched):

  * the same proposal re-pushed while the previous row is open must NOT
    append a sibling row under the same (or any) id — RED on the pre-fix
    inline leg, which minted PM-NNN unconditionally;
  * a NEW (dissimilar) proposal gets a fresh, never-before-used id and a
    normal task_added event + ledger entry;
  * a re-push AFTER the previous row is closed files a fresh id (the
    legitimate cycle — closure protocol intact);
  * a computed id that already exists on the board (any status) is skipped,
    never appended;
  * the dry run writes nothing (byte-identical board).

Run:  python3 -m pytest ops/pm-standin/test_push_proposals.py -v
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("push_proposals", os.path.join(HERE, "push_proposals.py"))
pp = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pp)


def _make_tree(tmpdir, project, board_rows=()):
    """Workdir-shaped fixture: <tmp>/<project>/… board + events + ledger.

    The workdir-existence check in push() resolves <home>/<project>, so the
    fixture workdir is named exactly like the live ~/. layout and the tmpdir
    itself is the ``home`` seam for push().
    """
    wd = os.path.join(tmpdir, project)
    board_dir = os.path.join(wd, ".coding-hermes", "board")
    os.makedirs(board_dir, exist_ok=True)
    board = os.path.join(board_dir, "tasks.jsonl")
    events = os.path.join(board_dir, "events.jsonl")
    ledger = os.path.join(tmpdir, "ledger.json")
    with open(board, "w", encoding="utf-8") as fh:
        for r in board_rows:
            fh.write(json.dumps(r) + "\n")
    with open(events, "w", encoding="utf-8") as fh:
        pass
    with open(ledger, "w", encoding="utf-8") as fh:
        json.dump({"items": []}, fh)
    return wd, board, events, ledger, str(tmpdir)


def _scratch(tmpdir, proposals):
    path = os.path.join(tmpdir, "push-scratch.jsonl")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(json.dumps({"proposals": proposals}) + "\n")
    return path


def _proposal(project, title):
    return {"project": project, "title": title, "priority": "P2", "gap": "port pool"}


def _board_ids(board):
    return [str(json.loads(l)["id"]) for l in open(board, encoding="utf-8") if l.strip()]


def _board_titles(board):
    return [str(json.loads(l).get("title", "")) for l in open(board, encoding="utf-8") if l.strip()]


def test_same_open_condition_refiled_under_same_id_never_appends_a_sibling(tmpdir):
    """THE load-bearing RED: the pre-fix inline leg re-minted the id (or the
    next PM-NNN) for a re-worded copy of the same open condition — the 29%
    id-slot class. The fixed writer suppresses the re-file and annotates."""
    wd, board, events, ledger, home = _make_tree(
        tmpdir, "alpha",
        board_rows=[{"id": "PM-004", "title": "port pool exhausted on bunker",
                     "status": "pending"}])
    scratch = _scratch(tmpdir, [
        _proposal("alpha", "port pool exhausted on bunker [day 3 re-check]")])

    res = pp.push(board, events, scratch, ledger, "alpha", apply=True, home=home)

    # No sibling row: the board still holds exactly ONE row, still PM-004.
    assert _board_ids(board) == ["PM-004"], _board_ids(board)
    assert res["filed"] == []
    assert len(res["suppressed"]) == 1
    # The suppression is annotated (closure-protocol audit trail).
    ev = [json.loads(l) for l in open(events, encoding="utf-8") if l.strip()]
    assert any(e.get("kind") == "refile_suppressed" for e in ev), ev


def test_new_finding_gets_fresh_id_after_highest_ever_suffix(tmpdir):
    """A genuinely NEW finding gets the next unused id and the full
    INSERT-before-event + ledger contract the inline leg honoured."""
    wd, board, events, ledger, home = _make_tree(
        tmpdir, "alpha",
        board_rows=[{"id": "PM-004", "title": "port pool exhausted",
                     "status": "complete"}])  # closed history: suffix 4 ever used
    scratch = _scratch(tmpdir, [_proposal("alpha", "dashboard auth loop fails")])

    res = pp.push(board, events, scratch, ledger, "alpha", apply=True, home=home)

    assert _board_ids(board) == ["PM-004", "PM-005"], _board_ids(board)
    assert len(res["filed"]) == 1 and res["filed"][0]["id"] == "PM-005"
    rows = [json.loads(l) for l in open(board, encoding="utf-8") if l.strip()]
    assert rows[-1]["status"] == "pending"
    ev = [json.loads(l) for l in open(events, encoding="utf-8") if l.strip()]
    assert any(e.get("kind") == "task_added" and e.get("id") == "PM-005" for e in ev)
    led = json.load(open(ledger, encoding="utf-8"))
    assert any(i["id"] == "alpha:PM-005" for i in led["items"])


def test_refile_after_closing_previous_row_files_fresh_id(tmpdir):
    """The legitimate cycle: once the previous row is complete AND the new
    title is dissimilar, a fresh row files under the next id — the closure
    protocol (close, then refile) is not blocked by the suppression rule."""
    wd, board, events, ledger, home = _make_tree(tmpdir, "alpha")
    scratch = _scratch(tmpdir, [_proposal("alpha", "port pool exhausted on bunker")])
    pp.push(board, events, scratch, ledger, "alpha", apply=True, home=home)
    assert _board_ids(board) == ["PM-001"]

    # close the row, then refile the SAME condition
    rows = [json.loads(l) for l in open(board, encoding="utf-8") if l.strip()]
    rows[0]["status"] = "complete"
    with open(board, "w", encoding="utf-8") as fh:
        for r in rows:
            fh.write(json.dumps(r) + "\n")
    scratch2 = _scratch(tmpdir, [_proposal("alpha", "port pool exhausted on bunker [recurred]")])

    res = pp.push(board, events, scratch2, ledger, "alpha", apply=True, home=home)

    assert _board_ids(board) == ["PM-001", "PM-002"], _board_ids(board)
    assert len(res["filed"]) == 1


def test_mint_loop_skips_occupied_ids_on_handwritten_board(tmpdir):
    """Structural uniqueness: a board that already holds PM-005 (closed,
    hand-written shape) can never gain a SECOND row under PM-005 — the mint
    loop skips past occupied ids and the new row lands on PM-006."""
    wd, board, events, ledger, home = _make_tree(
        tmpdir, "alpha",
        board_rows=[{"id": "PM-005", "title": "something else", "status": "complete"}])
    scratch = _scratch(tmpdir, [_proposal("alpha", "brand new distinct condition xyz")])

    res = pp.push(board, events, scratch, ledger, "alpha", apply=True, home=home)

    assert _board_ids(board) == ["PM-005", "PM-006"], _board_ids(board)
    assert res["filed"] == [{"project": "alpha", "id": "PM-006",
                             "title": "brand new distinct condition xyz"}]


def test_dry_run_writes_nothing(tmpdir):
    """No --apply: the board, events and ledger stay byte-identical."""
    wd, board, events, ledger, home = _make_tree(
        tmpdir, "alpha",
        board_rows=[{"id": "PM-001", "title": "port pool exhausted",
                     "status": "pending"}])
    before = {p: open(p, "rb").read() for p in (board, events, ledger)}
    scratch = _scratch(tmpdir, [_proposal("alpha", "brand new distinct condition xyz")])

    res = pp.push(board, events, scratch, ledger, "alpha", apply=False, home=home)

    assert len(res["filed"]) == 1  # WOULD file
    for p, data in before.items():
        assert open(p, "rb").read() == data, f"{p} mutated in dry-run"


def test_title_overlap_threshold_rejects_dissimilar_titles():
    """The suppression matcher is token-overlap based, not exact — but a
    genuinely different finding must score BELOW the threshold so the
    suppression rule never eats new work."""
    assert pp.title_overlap(
        "port pool exhausted on bunker",
        "port pool exhausted on bunker [day 3 re-check]") >= pp.TITLE_DUP_THRESHOLD
    assert pp.title_overlap(
        "port pool exhausted on bunker",
        "dashboard auth loop fails after upgrade") < pp.TITLE_DUP_THRESHOLD