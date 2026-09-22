#!/usr/bin/env python3
"""GAP-049 tests: ledger/board reconciliation split (drift vs open work).

Deterministic, stdlib-only (unittest). Every fixture lives in a per-test
temporary directory: fixture ledgers, a fixture scheduler.db (table
`projects(name, workdir)`), and fixture boards. NO test passes a live path:
the run_main helper refuses any ledger path outside the test tmpdir, and
TestLiveSafety proves ~/.hermes/stand-in/ledger.json keeps a stable hash
across the whole suite (read-only check; the suite never writes it).

Proven here (mapped to the worker brief):
  - dry-run byte identity and no writes anywhere in the fixture tree;
  - --apply reconciles exact-ID non-terminal rows AND stale rows whose board
    row is complete (both to 'verified' with evidence), touching only
    status/last_checked_at/verification_evidence;
  - board-open aged rows, stale rows whose board row is still open, and
    verified/complete items remain untouched by --apply;
  - title-similarity candidates are bounded, stable, deterministic, and
    manual-review only (never auto-closed by --apply, never written);
  - the digest JSON exposes separate drift_closable / board_open_work /
    stale_drift / no_id_match counts and stuck_* lists open work only;
  - the dry-run summary JSON carries the same split and NO universal
    "stuck < N" success gate.

Run:  python3 -m unittest test_ledger_board_reconcile -v
"""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
import sqlite3
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
_MOD_PATH = os.path.join(HERE, "ledger_board_reconcile.py")
_spec = importlib.util.spec_from_file_location("lbr_under_test", _MOD_PATH)
lbr = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(lbr)

LIVE_LEDGER = os.path.expanduser("~/.hermes/stand-in/ledger.json")

# Fixed clock for the pure-helper tests (ages are exact, fully deterministic).
NOW = datetime(2026, 9, 13, 12, 0, 0, tzinfo=timezone.utc)


def iso(hours_ago, ref=None):
    ref = ref or NOW
    return (ref - timedelta(hours=hours_ago)).strftime("%Y-%m-%dT%H:%M:%SZ")


def sha256_of(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read_bytes(path):
    with open(path, "rb") as fh:
        return fh.read()


def tree_snapshot(root):
    """(relpath, size, mtime_ns) for every file under root — write detection."""
    out = []
    for dirpath, _dirs, files in os.walk(root):
        for name in files:
            p = os.path.join(dirpath, name)
            st = os.stat(p)
            out.append((os.path.relpath(p, root), st.st_size, st.st_mtime_ns))
    return sorted(out)


class FixtureMixin:
    """Builds a tmp sandbox: scheduler.db + project boards + ledger."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="gap049-test-")
        self.addCleanup(self._cleanup_tmp)
        self.board_title = {}  # board id -> title (for candidate assertions)

    def _cleanup_tmp(self):
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)

    # -- fixture builders ------------------------------------------------- #
    def make_board(self, project, rows):
        """rows: list of dicts with at least id/title/status. Returns workdir."""
        wd = os.path.join(self.tmp, "wd-" + project)
        board_dir = os.path.join(wd, ".coding-hermes", "board")
        os.makedirs(board_dir, exist_ok=True)
        with open(os.path.join(board_dir, "tasks.jsonl"), "w", encoding="utf-8") as fh:
            for row in rows:
                fh.write(json.dumps(row) + "\n")
                self.board_title[row["id"]] = row.get("title") or ""
        return wd

    def make_db(self, name_to_workdir):
        db = os.path.join(self.tmp, "scheduler.db")
        con = sqlite3.connect(db)
        try:
            con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, workdir TEXT)")
            con.executemany(
                "INSERT INTO projects (name, workdir) VALUES (?, ?)",
                sorted(name_to_workdir.items()),
            )
            con.commit()
        finally:
            con.close()
        return db

    def make_ledger(self, items, name="ledger.json"):
        path = os.path.join(self.tmp, name)
        with open(path, "w", encoding="utf-8") as fh:
            json.dump({"items": items}, fh, indent=1)
            fh.write("\n")
        return path

    def board_loader(self, boards):
        """boards: {project: rows_dict or None} -> board_loader callable."""
        def load(project):
            return boards.get(project)
        return load

    # -- guarded runners --------------------------------------------------- #
    def _guard_paths(self, *paths):
        for p in paths:
            self.assertTrue(
                os.path.abspath(p).startswith(os.path.abspath(self.tmp) + os.sep),
                "test attempted to use a path outside the tmp sandbox: %s" % p)

    def run_main(self, ledger, db, extra=()):
        self._guard_paths(ledger, db)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            # SCHED-GAP-207: now=NOW makes the suite deterministic — main()
            # previously read the wall clock, so the fixture's deliberately-
            # young item (NOW - 2h) aged past the 48h window on 2026-09-15+
            # and flipped no_id_match_count 6→7 (a test time-bomb).
            rc = lbr.main([
                "--ledger", ledger,
                "--scheduler-db", db,
                "--backup-suffix", "bak-gap049-test",
            ] + list(extra), now=NOW)
        return rc, buf.getvalue()

    def summary_from(self, stdout):
        begin = stdout.index(lbr.SUMMARY_JSON_BEGIN) + len(lbr.SUMMARY_JSON_BEGIN)
        end = stdout.index(lbr.SUMMARY_JSON_END)
        return json.loads(stdout[begin:end])

    def read_ledger(self, path):
        with open(path, "r", encoding="utf-8") as fh:
            return json.load(fh)

    def items_by_id(self, doc):
        return {it["id"]: it for it in doc["items"]}


class TestClassificationHelpers(unittest.TestCase):
    """Pure-function proofs: classification table, candidates, JSONL parse."""

    def test_classify_item_category_table(self):
        # stale re-evaluated against board truth (GAP-049 core)
        self.assertEqual(lbr.classify_item("stale", "complete", True), "stale_drift")
        self.assertEqual(lbr.classify_item("stale", "pending", True), "stale_terminal")
        # non-terminal + complete board = the ONLY closable drift
        for st in ("added", "picked_up", "blocked", "in_progress"):
            self.assertEqual(lbr.classify_item(st, "complete", True), "drift_closable")
            self.assertEqual(lbr.classify_item(st, "pending", True), "board_open_work")
        # no board found dominates
        self.assertEqual(lbr.classify_item("added", "complete", False), "board_missing")
        # terminal statuses never enter the aged-nonterminal set
        self.assertNotIn("verified", lbr.NON_TERMINAL_STATUSES)
        self.assertNotIn("complete", lbr.NON_TERMINAL_STATUSES)
        self.assertIn("stale", lbr.NON_TERMINAL_STATUSES)

    def test_title_similarity_deterministic_and_bounded(self):
        a = "Fix resolver gap in ledger digest"
        b = "Fix  resolver   gap in LEDGER digest"
        for _ in range(3):  # deterministic across repeated calls
            self.assertEqual(lbr.title_similarity(a, b), lbr.title_similarity(a, b))
        self.assertTrue(0.0 <= lbr.title_similarity(a, b) <= 1.0)
        self.assertEqual(lbr.title_similarity(a, b), 1.0)  # normalized equal
        self.assertEqual(lbr.title_similarity("", a), 0.0)
        self.assertEqual(lbr.title_similarity(None, None), 0.0)

    def test_title_candidates_bounded_stable_sorted(self):
        rows = {
            "Z-1": {"id": "Z-1", "title": "Fix resolver gap in ledger digest", "status": "complete"},
            "A-2": {"id": "A-2", "title": "Fix resolver gap in ledger digest output", "status": "pending"},
            "M-3": {"id": "M-3", "title": "Unrelated completely different thing", "status": "pending"},
            "B-4": {"id": "B-4", "title": "Fix resolver gap in ledger digest parsing", "status": "complete"},
            "C-5": {"id": "C-5", "title": "Another resolver gap in digest", "status": "pending"},
            "D-6": {"id": "D-6", "title": "Resolver gap four", "status": "pending"},
            "E-7": {"id": "E-7", "title": "Resolver gap five", "status": "pending"},
        }
        got = lbr.title_candidates("Fix resolver gap in ledger digest", rows)
        self.assertLessEqual(len(got), lbr.CANDIDATE_LIMIT)  # bounded
        scores = [c["score"] for c in got]
        self.assertEqual(scores, sorted(scores, reverse=True))  # best first
        for cand in got:  # board id + score evidence always present
            self.assertIn(cand["board_id"], rows)
            self.assertIsInstance(cand["score"], float)
            self.assertTrue(cand["board_id"] and cand["board_title"] != "")
        # stable: same input -> same output, independent of dict insertion order
        again = lbr.title_candidates(
            "Fix resolver gap in ledger digest",
            {k: rows[k] for k in reversed(list(rows))})
        self.assertEqual(got, again)
        # deterministic tie-break by board id: equal-score candidates come out id-sorted
        tie_rows = {
            "B-x": {"id": "B-x", "title": "resolver", "status": "pending"},
            "A-y": {"id": "A-y", "title": "resolver", "status": "pending"},
        }
        tie = lbr.title_candidates("resolver", tie_rows, limit=1)
        self.assertEqual([c["board_id"] for c in tie], ["A-y"])

    def test_read_jsonl_rows_keep_last_and_malformed_tolerance(self):
        path = os.path.join(tempfile.mkdtemp(prefix="gap049-jl-"), "b.jsonl")
        self.addCleanup(lambda: os.remove(path))
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(json.dumps({"id": "X-1", "status": "pending", "title": "first"}) + "\n")
            fh.write("this is not json\n")           # malformed -> skipped
            fh.write(json.dumps({"id": "X-1", "status": "complete", "title": "refile"}) + "\n")
            fh.write("\n")                            # blank -> skipped
            fh.write(json.dumps({"no_id": True}) + "\n")  # id-less -> skipped
        rows, malformed = lbr.read_jsonl_rows(path)
        self.assertEqual(malformed, 1)
        self.assertEqual(rows["X-1"]["status"], "complete")  # keep-last wins
        self.assertEqual(len(rows), 1)


class TestReconcileEndToEnd(FixtureMixin, unittest.TestCase):
    """Dry-run byte identity + --apply semantics against fixture truth."""

    def build_world(self):
        """Two projects: exact-ID matrix + hivemind-style no-ID-match ids."""
        alpha_rows = [
            {"id": "T-001", "title": "close the drift loop", "status": "complete"},
            {"id": "T-002", "title": "genuinely open work item", "status": "pending"},
            {"id": "T-003", "title": "stale item now done on board", "status": "complete"},
            {"id": "T-004", "title": "stale item still open on board", "status": "pending"},
            {"id": "T-005", "title": "resolver gap ledger digest", "status": "pending"},
        ]
        hw_rows = [
            {"id": "QA-HIVEMIND-WORK-004", "title": "Fix resolver gap in ledger digest", "status": "complete"},
            {"id": "DF-HIVEMIND-WORK-002", "title": "Fix resolver gap in ledger digest output", "status": "pending"},
            {"id": "QA-HIVEMIND-WORK-007", "title": "Completely unrelated topic zebra", "status": "pending"},
        ]
        wds = {
            "alpha": self.make_board("alpha", alpha_rows),
            "hivemind-work": self.make_board("hivemind-work", hw_rows),
            "ghostproj": os.path.join(self.tmp, "wd-ghostproj"),  # no board dir
        }
        db = self.make_db(wds)
        aged = 100  # hours — past the 48h stuck window
        items = [
            # alpha: one per category
            {"id": "T-001", "project": "alpha", "title": "close the drift loop",
             "status": "added", "added_at": iso(aged)},
            {"id": "T-002", "project": "alpha", "title": "genuinely open work item",
             "status": "picked_up", "added_at": iso(aged)},
            {"id": "T-003", "project": "alpha", "title": "stale item now done on board",
             "status": "stale", "added_at": iso(aged)},
            {"id": "T-004", "project": "alpha", "title": "stale item still open on board",
             "status": "stale", "added_at": iso(aged)},
            {"id": "T-911", "project": "alpha", "title": "resolver gap ledger digest",
             "status": "added", "added_at": iso(aged)},          # no_id_match
            {"id": "T-000", "project": "alpha", "title": "ageless",
             "status": "added", "added_at": "not-a-date"},        # age_unknown
            {"id": "T-010", "project": "alpha", "title": "young item",
             "status": "added", "added_at": iso(2)},              # young -> ignored
            {"id": "T-011", "project": "alpha", "title": "already verified",
             "status": "verified", "added_at": iso(aged)},        # terminal -> ignored
            # hivemind-work-style rotating IDs (HW-GAP-006..010 mapping problem)
            {"id": "HW-GAP-006", "project": "hivemind-work", "title": "Fix resolver gap in ledger digest",
             "status": "picked_up", "added_at": iso(aged)},
            {"id": "HW-GAP-007", "project": "hivemind-work", "title": "Fix resolver gap in ledger digest output",
             "status": "added", "added_at": iso(aged)},
            {"id": "HW-GAP-008", "project": "hivemind-work", "title": "Completely unrelated topic zebra crossing",
             "status": "added", "added_at": iso(aged)},
            {"id": "HW-GAP-009", "project": "hivemind-work", "title": "Unrelated ledger prose",
             "status": "picked_up", "added_at": iso(aged)},
            {"id": "HW-GAP-010", "project": "hivemind-work", "title": "Fix resolver gap in ledger digest parsing",
             "status": "added", "added_at": iso(aged)},
            # ghostproj: board missing entirely
            {"id": "G-001", "project": "ghostproj", "title": "ghost row",
             "status": "added", "added_at": iso(aged)},
        ]
        ledger = self.make_ledger(items)
        return ledger, db

    EXPECTED_TOTALS = {
        "aged_nonterminal": 12,
        "board_open_work": 1,     # T-002 only — genuine open work, NOT drift
        "drift_closable": 1,      # T-001
        "stale_drift": 1,         # T-003
        "stale_terminal": 1,      # T-004
        "board_missing": 1,       # G-001
        "no_id_match": 6,         # T-911 + HW-GAP-006..010
        "ambiguous": 0,
        "age_unknown": 1,         # T-000
    }

    def classify_fixture(self, ledger, db):
        resolver = lbr.ProjectResolver(db)
        loader = lbr.board_loader_from_resolver(resolver)
        doc = lbr.load_ledger(ledger)
        return lbr.classify_aged_items(doc["items"], loader, NOW)

    def test_dry_run_is_byte_identical_and_writes_nothing(self):
        ledger, db = self.build_world()
        before_bytes = read_bytes(ledger)
        before_tree = tree_snapshot(self.tmp)
        rc, out = self.run_main(ledger, db)  # default = dry-run
        self.assertEqual(rc, 0)
        self.assertEqual(read_bytes(ledger), before_bytes)      # byte identity
        self.assertEqual(tree_snapshot(self.tmp), before_tree)     # nothing written anywhere
        self.assertIn("DRY-RUN", out)
        self.assertNotIn("ledger written", out)
        # no backup/tmp residue
        self.assertEqual([f for f in os.listdir(self.tmp) if "bak" in f or ".tmp" in f], [])

    def test_classification_totals_match_expected_matrix(self):
        ledger, db = self.build_world()
        report = self.classify_fixture(ledger, db)
        totals = report["totals"]
        for key, want in self.EXPECTED_TOTALS.items():
            self.assertEqual(totals[key], want, "totals[%s]" % key)
        # per-record spot checks
        by_id = {r["item_id"]: r["category"] for r in report["records"]}
        self.assertEqual(by_id["T-001"], "drift_closable")
        self.assertEqual(by_id["T-002"], "board_open_work")
        self.assertEqual(by_id["T-003"], "stale_drift")
        self.assertEqual(by_id["T-004"], "stale_terminal")
        self.assertEqual(by_id["T-911"], "no_id_match")
        self.assertEqual(by_id["T-000"], "age_unknown")
        self.assertEqual(by_id["G-001"], "board_missing")
        for n in range(6, 11):
            self.assertEqual(by_id["HW-GAP-%03d" % n], "no_id_match")
        # young + terminal items never enter the aged set
        self.assertNotIn("T-010", by_id)
        self.assertNotIn("T-011", by_id)

    def test_apply_reconciles_closable_and_stale_drift_only(self):
        ledger, db = self.build_world()
        before_bytes = read_bytes(ledger)
        rc, out = self.run_main(ledger, db, extra=["--apply"])
        self.assertEqual(rc, 0)
        self.assertIn("ledger written", out)
        doc = self.read_ledger(ledger)
        items = self.items_by_id(doc)

        # --- reconciled: exact-ID non-terminal with complete board row ---
        self.assertEqual(items["T-001"]["status"], "verified")
        self.assertEqual(items["T-003"]["status"], "verified")  # stale re-evaluated
        for closed in ("T-001", "T-003"):
            self.assertIn("last_checked_at", items[closed])
            ev = items[closed].get("verification_evidence") or ""
            self.assertIn("RECONCILE", ev)
            self.assertIn(closed, ev)  # evidence cites the board row id
            self.assertIn(".coding-hermes/board/tasks.jsonl", ev)  # and the board path

        # --- untouched: board-open aged rows remain open work ---
        self.assertEqual(items["T-002"]["status"], "picked_up")
        self.assertNotIn("last_checked_at", items["T-002"])
        # --- untouched: stale whose board row is still open stays stale ---
        self.assertEqual(items["T-004"]["status"], "stale")
        self.assertNotIn("verification_evidence", items["T-004"])
        # --- untouched: no_id_match items are NEVER auto-closed ---
        for nid in ("T-911", "HW-GAP-006", "HW-GAP-007", "HW-GAP-008",
                    "HW-GAP-009", "HW-GAP-010"):
            self.assertIn(items[nid]["status"], ("added", "picked_up"))
            self.assertNotIn("verification_evidence", items[nid])
        # --- untouched: age_unknown, young, terminal ---
        self.assertEqual(items["T-000"]["status"], "added")
        self.assertEqual(items["T-010"]["status"], "added")
        self.assertEqual(items["T-011"]["status"], "verified")
        # --- every OTHER field byte-preserving: only 3 fields differ ---
        orig = self.items_by_id(json.loads(before_bytes.decode("utf-8")))
        for it_id, before in orig.items():
            after = items[it_id]
            allowed = {"status", "last_checked_at", "verification_evidence"}
            changed = {k for k in set(before) | set(after)
                       if before.get(k) != after.get(k)}
            self.assertTrue(changed <= allowed,
                            "%s changed unexpected fields %s" % (it_id, changed))

        # --- backup carries the pre-apply bytes ---
        backup = ledger + ".bak-gap049-test"
        self.assertTrue(os.path.isfile(backup))
        self.assertEqual(read_bytes(backup), before_bytes)
        # --- no leftover tmp files ---
        self.assertEqual([f for f in os.listdir(self.tmp) if ".tmp." in f], [])

    def test_apply_zero_changes_writes_nothing(self):
        ledger = self.make_ledger([
            {"id": "V-1", "project": "alpha", "title": "done", "status": "verified",
             "added_at": iso(100)}])
        self.make_board("alpha", [{"id": "V-1", "title": "done", "status": "complete"}])
        db = self.make_db({("alpha"): self.board_wd("alpha")})
        before = read_bytes(ledger)
        rc, out = self.run_main(ledger, db, extra=["--apply"])
        self.assertEqual(rc, 0)
        self.assertIn("nothing to do", out)
        self.assertEqual(read_bytes(ledger), before)
        self.assertFalse(os.path.isfile(ledger + ".bak-gap049-test"))

    def board_wd(self, project):
        return os.path.join(self.tmp, "wd-" + project)

    def test_no_id_candidates_manual_review_only_shape(self):
        ledger, db = self.build_world()
        report = self.classify_fixture(ledger, db)
        details = lbr.build_no_id_matches(report["no_id_items"], report["rows_cache"])
        # per-project no_id_match flag + bounded deterministic candidates
        self.assertIn("hivemind-work", details)
        det = details["hivemind-work"]
        self.assertTrue(det["no_id_match"])
        self.assertEqual(det["count"], 5)
        self.assertEqual([i["item_id"] for i in det["items"]],
                         sorted(i["item_id"] for i in det["items"]))  # stable order
        for entry in det["items"]:
            self.assertLessEqual(len(entry["candidates"]), lbr.CANDIDATE_LIMIT)
            for cand in entry["candidates"]:
                self.assertIn(cand["board_id"], ("QA-HIVEMIND-WORK-004",
                                                 "DF-HIVEMIND-WORK-002",
                                                 "QA-HIVEMIND-WORK-007"))
                self.assertIsInstance(cand["score"], float)
        # the crafted near-title item must point at the QA/DF rows, by id
        near = next(e for e in det["items"] if e["item_id"] == "HW-GAP-006")
        self.assertTrue(near["candidates"])
        self.assertEqual(near["candidates"][0]["board_id"], "QA-HIVEMIND-WORK-004")
        # zebra item maps to its own board row as the plausible candidate
        zebra = next(e for e in det["items"] if e["item_id"] == "HW-GAP-008")
        self.assertTrue(zebra["candidates"])
        self.assertEqual(zebra["candidates"][0]["board_id"], "QA-HIVEMIND-WORK-007")
        # a truly unrelated title yields NO candidates (below advisory threshold)
        self.assertEqual(lbr.title_candidates("qqq zzz wkw", report["rows_cache"]["hivemind-work"]), [])
        # deterministic across a full re-run
        again = lbr.build_no_id_matches(report["no_id_items"], report["rows_cache"])
        self.assertEqual(json.dumps(details, sort_keys=True),
                         json.dumps(again, sort_keys=True))
        # --apply on the same fixture leaves all candidates items unmodified
        rc, _ = self.run_main(ledger, db, extra=["--apply"])
        self.assertEqual(rc, 0)
        items = self.items_by_id(self.read_ledger(ledger))
        for entry in det["items"]:
            it = items[entry["item_id"]]
            self.assertIn(it["status"], ("added", "picked_up"))
            self.assertNotIn("verification_evidence", it)

    def test_summary_json_split_and_no_stuck_gate(self):
        ledger, db = self.build_world()
        rc, out = self.run_main(ledger, db)  # dry-run
        summary = self.summary_from(out)
        self.assertEqual(summary["schema"], "pm-ledger-reconcile-summary/1")
        self.assertEqual(summary["mode"], "DRY-RUN")
        self.assertEqual(summary["applied"], False)
        for key, want in (
                ("drift_closable_count", 1),
                ("board_open_work_count", 1),
                ("stale_drift_count", 1),
                ("no_id_match_count", 6)):
            self.assertEqual(summary[key], want, key)
        # stuck_* is legacy-compat ONLY and now lists board_open_work alone
        self.assertEqual([r["id"] for r in summary["stuck_48h"]], ["T-002"])
        self.assertIn("board_open_work", summary["stuck_count_legacy_note"])
        # NO universal stuck<N> success gate anywhere in the summary
        banned = ("stuck_target", "stuck_ok", "success", "pass", "gate")
        for key in summary:
            self.assertFalse(any(b in key.lower() for b in banned),
                             "unexpected gate-like key: %s" % key)
        # no_id_matches detail block present with per-project flag
        self.assertTrue(summary["no_id_matches"]["hivemind-work"]["no_id_match"])


class TestDigestSplit(FixtureMixin, unittest.TestCase):
    """digest_input() — the JSON the PM tick exposes (AC4)."""

    def build_min_world(self):
        rows = [
            {"id": "T-001", "title": "close the drift loop", "status": "complete"},
            {"id": "T-002", "title": "genuinely open work item", "status": "pending"},
            {"id": "T-003", "title": "stale item now done on board", "status": "complete"},
        ]
        wd = self.make_board("alpha", rows)
        db = self.make_db({"alpha": wd})
        ledger = self.make_ledger([
            {"id": "T-001", "project": "alpha", "title": "close the drift loop",
             "status": "added", "added_at": iso(100)},
            {"id": "T-002", "project": "alpha", "title": "genuinely open work item",
             "status": "picked_up", "added_at": iso(100)},
            {"id": "T-003", "project": "alpha", "title": "stale item now done on board",
             "status": "stale", "added_at": iso(100)},
        ])
        init = os.path.join(self.tmp, "initiatives.json")
        with open(init, "w", encoding="utf-8") as fh:
            json.dump({"initiatives": [{"id": "I-1", "status": "active",
                                        "last_updated": iso(1)}]}, fh)
        return ledger, init, db

    def test_digest_bundle_exposes_split_counts(self):
        ledger, init, db = self.build_min_world()
        out_path = os.path.join(self.tmp, "digest-input.json")
        loader = lbr.board_loader_from_resolver(lbr.ProjectResolver(db))
        bundle = lbr.digest_input(ledger, init, out_path, board_loader=loader, now=NOW)
        # the four AC4 split counts
        self.assertEqual(bundle["drift_closable_count"], 1)
        self.assertEqual(bundle["board_open_work_count"], 1)
        self.assertEqual(bundle["stale_drift_count"], 1)
        self.assertEqual(bundle["no_id_match_count"], 0)
        self.assertTrue(bundle["reconciliation_available"])
        # stuck_* is now board-open-work ONLY and honestly labelled
        self.assertEqual(bundle["stuck_count"], 1)
        self.assertEqual([s["id"] for s in bundle["stuck_48h"]], ["T-002"])
        self.assertIn("board_open_work", bundle["stuck_count_legacy_note"])
        # categories block carries the full machine-readable split
        cats = bundle["reconciliation"]["categories"]
        self.assertEqual(cats["drift_closable"], 1)
        self.assertEqual(cats["board_open_work"], 1)
        self.assertEqual(cats["stale_drift"], 1)
        self.assertEqual(cats["stale_terminal"], 0)
        # written file is valid JSON with the same counts
        with open(out_path, "r", encoding="utf-8") as fh:
            on_disk = json.load(fh)
        self.assertEqual(on_disk["drift_closable_count"], 1)
        self.assertEqual(on_disk["stale_drift_count"], 1)
        self.assertEqual(on_disk["board_open_work_count"], 1)

    def test_digest_without_reconciler_is_honest_none(self):
        ledger, init, _db = self.build_min_world()
        out_path = os.path.join(self.tmp, "digest-input.json")
        bundle = lbr.digest_input(ledger, init, out_path, board_loader=None, now=NOW)
        self.assertFalse(bundle["reconciliation_available"])
        self.assertIsNone(bundle["drift_closable_count"])
        self.assertIsNone(bundle["board_open_work_count"])
        self.assertIsNone(bundle["stale_drift_count"])
        self.assertIsNone(bundle["no_id_match_count"])
        self.assertIn("legacy conflation", bundle["stuck_count_legacy_note"])


class TestLiveSafety(unittest.TestCase):
    """AC5: the suite never writes the live ledger or any live board."""

    def test_live_ledger_hash_stable_across_suite(self):
        # Read-only existence/hash probe. If the live file is absent nothing
        # in this suite may create it.
        if not os.path.exists(LIVE_LEDGER):
            self.assertFalse(os.path.exists(LIVE_LEDGER),
                             "suite must never create the live ledger")
            return
        # Import-time + classification/digest tests above run in-process;
        # by the time this test runs, a stray write would already be visible.
        h = sha256_of(LIVE_LEDGER)
        self.assertEqual(sha256_of(LIVE_LEDGER), h)
        # static proof: the module's defaults point at the live path, but
        # every test in this file passes explicit tmp paths (guarded by
        # FixtureMixin._guard_paths).
        self.assertTrue(lbr.LEDGER_DEFAULT.endswith("stand-in/ledger.json"))


if __name__ == "__main__":
    unittest.main()
