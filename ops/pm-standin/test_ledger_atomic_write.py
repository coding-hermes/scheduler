"""SCHED-GAP-1645 tests: atomic ledger write (push_proposals.atomic_json_write).

Proves the ledger writer can no longer produce a truncated or glued tail on
temp fixtures only (no live board/ledger is ever touched):

  * the write round-trips: json.load of the on-disk file equals the value
    written (the old writer's ``except OSError: pass`` hid a serialisation or
    mid-write crash that left a glued tail);
  * the file ends with a real trailing newline (so a subsequent append never
    glues onto the previous object);
  * the write is atomic: it goes through a ``.tmp`` file in the same
    directory + ``os.replace``, so no ``.tmp`` residue is left and the target
    is never observed half-written;
  * a previous file is preserved in a ``.bak`` copy, and a failing write
    restores the original and exits non-zero with a stderr message (never
    silently swallowed).

Run:  python3 ops/pm-standin/test_ledger_atomic_write.py
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "push_proposals", os.path.join(HERE, "push_proposals.py"))
pp = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pp)

_failures = []


def _check(name, cond):
    if cond:
        print("ok - {}".format(name))
    else:
        print("FAIL - {}".format(name))
        _failures.append(name)


def _read_bytes(path):
    with open(path, "rb") as fh:
        return fh.read()


def test_roundtrip_and_trailing_newline():
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "ledger.json")
        obj = {"items": [{"id": "alpha:PM-001", "project": "alpha",
                          "status": "added"}]}

        pp.atomic_json_write(path, obj)

        data = _read_bytes(path)
        _check("write ends with a real newline", data.endswith(b"\n"))
        _check("round-trip parse equals the written value",
               json.loads(data) == obj)
        _check("no .tmp residue left behind",
               not os.path.exists(path + ".tmp"))
        _check("no .bak on first write (no prior content)",
               not os.path.exists(path + ".bak"))


def test_atomic_no_partial_target_and_bak_preserved():
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "ledger.json")
        original = {"items": [{"id": "alpha:PM-001"}]}
        pp.atomic_json_write(path, original)

        # A second write over a prior file must leave the previous content in a
        # .bak copy and produce a clean, fully-formed target (no partial state).
        updated = {"items": [{"id": "alpha:PM-001"}, {"id": "alpha:PM-002"}]}
        pp.atomic_json_write(path, updated)

        _check("target holds the updated value", json.loads(_read_bytes(path)) == updated)
        _check("previous content preserved in .bak",
               json.loads(_read_bytes(path + ".bak")) == original)
        _check("no .tmp residue after overwrite",
               not os.path.exists(path + ".tmp"))


def test_failure_restores_original_and_exits_nonzero():
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "ledger.json")
        original = {"items": [{"id": "alpha:PM-001"}]}
        pp.atomic_json_write(path, original)
        before = _read_bytes(path)

        # A non-JSON-serialisable value (a set) makes json.dump raise mid-write.
        bad = {"items": [{"id": "alpha:PM-002", "when": {1, 2, 3}}]}
        try:
            pp.atomic_json_write(path, bad)
            _check("failing write exits non-zero", False)
        except SystemExit as exc:
            _check("failing write exits non-zero", exc.code == 1)

        _check("original content restored on failure", _read_bytes(path) == before)
        _check("no .tmp residue after failure", not os.path.exists(path + ".tmp"))


def test_push_uses_atomic_writer():
    """Integration: a real --apply push writes a newline-terminated, parseable
    ledger through the atomic writer (not the old open(..., \"w\") path)."""
    with tempfile.TemporaryDirectory() as d:
        project = "alpha"
        wd = os.path.join(d, project)
        board_dir = os.path.join(wd, ".coding-hermes", "board")
        os.makedirs(board_dir, exist_ok=True)
        board = os.path.join(board_dir, "tasks.jsonl")
        events = os.path.join(board_dir, "events.jsonl")
        ledger = os.path.join(d, "ledger.json")
        with open(board, "w", encoding="utf-8") as fh:
            fh.write(json.dumps({"id": "PM-004", "title": "old",
                                 "status": "complete"}) + "\n")
        with open(events, "w", encoding="utf-8") as fh:
            pass
        with open(ledger, "w", encoding="utf-8") as fh:
            json.dump({"items": []}, fh)
        scratch = os.path.join(d, "scratch.jsonl")
        with open(scratch, "w", encoding="utf-8") as fh:
            fh.write(json.dumps({"proposals": [
                {"project": project, "title": "brand new distinct condition xyz",
                 "priority": "P2", "gap": "port pool"}]}) + "\n")

        pp.push(board, events, scratch, ledger, project, apply=True, home=d)

        data = _read_bytes(ledger)
        _check("push ledger ends with a real newline", data.endswith(b"\n"))
        led = json.loads(data)
        _check("push ledger is round-trip parseable",
               any(i["id"] == "alpha:PM-005" for i in led["items"]))


def main():
    test_roundtrip_and_trailing_newline()
    test_atomic_no_partial_target_and_bak_preserved()
    test_failure_restores_original_and_exits_nonzero()
    test_push_uses_atomic_writer()
    if _failures:
        print("\n{} check(s) FAILED".format(len(_failures)))
        return 1
    print("\nall checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
