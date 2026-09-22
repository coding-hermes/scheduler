#!/usr/bin/env python3
"""SCHED-GAP-207 AC2 demo: the same id can no longer be re-filed as a new
row without first closing the previous one.

Runs the REAL push_proposals.push() twice against one fixture board with the
same (re-worded) condition, then once more after closing the row:
  pass 1 — files PM-005 (fresh finding);
  pass 2 — the same condition re-worded is SUPPRESSED (no new row; the
           board still holds exactly one open row; a refile_suppressed
           event is annotated);
  pass 3 — after the previous row is marked complete, a genuinely new
           condition files a fresh unique id (the legitimate cycle).
Prints the board state after each pass. Exits non-zero if any invariant
breaks (a sibling open row under the same condition appearing without a
closure).
"""
import importlib.util
import json
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("push_proposals", os.path.join(HERE, "push_proposals.py"))
pp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pp)

tmp = tempfile.mkdtemp()
proj = "consensus"
os.makedirs(os.path.join(tmp, proj, ".coding-hermes", "board"), exist_ok=True)
board = os.path.join(tmp, proj, ".coding-hermes", "board", "tasks.jsonl")
events = os.path.join(tmp, proj, ".coding-hermes", "board", "events.jsonl")
open(board, "w").close()   # the writer requires an existing board file (ErrNoBoard shape)
open(events, "w").close()
ledger = os.path.join(tmp, "ledger.json")
with open(ledger, "w") as fh:
    json.dump({"items": []}, fh)


def board_state():
    rows = [json.loads(l) for l in open(board) if l.strip()]
    open_rows = [r for r in rows if r.get("status") in ("pending", "in_progress")]
    return rows, open_rows


def scratch_with(title):
    path = os.path.join(tmp, "scratch.jsonl")
    with open(path, "w") as fh:
        fh.write(json.dumps({"proposals": [
            {"project": proj, "title": title, "priority": "P1", "gap": "port pool"}]}) + "\n")
    return path


def push(title):
    return pp.push(board, events, scratch_with(title), ledger, proj, apply=True, home=tmp)


print("== pass 1: fresh finding ==")
res1 = pp.push(board, events, scratch_with("port pool exhausted on bunker"),
               ledger, proj, apply=True, home=tmp)
rows, open_rows = board_state()
print("  filed:", [f["id"] for f in res1["filed"]], "| open rows:", len(open_rows))
assert len(open_rows) == 1 and open_rows[0]["id"] == "PM-001"

print("== pass 2: SAME condition re-worded (the old defect) ==")
res2 = pp.push(board, events, scratch_with("port pool exhausted on bunker [recheck day 2]"),
               ledger, proj, apply=True, home=tmp)
rows, open_rows = board_state()
print("  filed:", len(res2["filed"]), "| suppressed:", len(res2["suppressed"]),
      "| open rows:", len(open_rows))
ev = [json.loads(l) for l in open(events) if l.strip()]
suppressed = [e for e in ev if e.get("kind") == "refile_suppressed"]
print("  refile_suppressed events:", len(suppressed))
assert not res2["filed"], "DEFECT REGRESSED: a re-file was appended"
assert len(open_rows) == 1, "DEFECT REGRESSED: two open rows for one finding"
assert len(suppressed) == 1, "closure annotation missing"

print("== pass 3: previous row CLOSED, then a new distinct finding ==")
rows, _ = board_state()
rows[0]["status"] = "complete"
with open(board, "w") as fh:
    for r in rows:
        fh.write(json.dumps(r) + "\n")
res3 = pp.push(board, events, scratch_with("dashboard auth loop fails after upgrade"),
               ledger, proj, apply=True, home=tmp)
rows, open_rows = board_state()
ids = [r["id"] for r in rows]
print("  filed:", [f["id"] for f in res3["filed"]], "| board:", ids,
      "| open rows:", len(open_rows))
assert res3["filed"] and res3["filed"][0]["id"] == "PM-002"
assert all(r["id"] != "PM-001" or r["status"] == "complete" for r in rows)

print("\nDEMO OK: an open condition can no longer be re-filed as a new row;")
print("a re-file requires closing the previous row first (pass 3).")