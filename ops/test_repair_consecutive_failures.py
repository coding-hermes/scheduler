import importlib.util
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("repair-consecutive-failures.py")
SPEC = importlib.util.spec_from_file_location("repair_consecutive_failures", SCRIPT)
repair_mod = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(repair_mod)


class RepairConsecutiveFailuresTest(unittest.TestCase):
    def make_db(self, directory):
        path = Path(directory) / "scheduler.db"
        db = sqlite3.connect(path)
        db.executescript(
            """
            CREATE TABLE projects (name TEXT PRIMARY KEY, consecutive_failures INTEGER NOT NULL);
            CREATE TABLE ticks (
                id TEXT PRIMARY KEY, project_name TEXT, status TEXT,
                failure_reason TEXT, spawned_at TEXT, completed_at TEXT, created_at TEXT
            );
            """
        )
        db.executemany("INSERT INTO projects VALUES (?, ?)", [("alpha", 5), ("beta", 1)])
        ticks = [
            ("a-transport", "alpha", "failed", "gateway_transport", "2026-11-10T00:00:00", "2026-11-10T00:00:00", "2026-11-10T00:00:00"),
            ("a-drain", "alpha", "failed", "gateway_drain", "2026-11-10T01:00:00", "2026-11-10T01:00:00", "2026-11-10T01:00:00"),
            ("a-fail", "alpha", "failed", "", "2026-11-10T02:00:00", "2026-11-10T02:00:00", "2026-11-10T02:00:00"),
            ("a-timeout", "alpha", "timeout", "", "2026-11-10T03:00:00", "2026-11-10T03:00:00", "2026-11-10T03:00:00"),
            ("b-ok", "beta", "completed", "", "2026-11-10T03:00:00", "2026-11-10T03:00:00", "2026-11-10T03:00:00"),
        ]
        db.executemany("INSERT INTO ticks VALUES (?, ?, ?, ?, ?, ?, ?)", ticks)
        db.commit()
        db.close()
        return path

    def read_counter(self, path, project):
        db = sqlite3.connect(path)
        value = db.execute("SELECT consecutive_failures FROM projects WHERE name = ?", (project,)).fetchone()[0]
        db.close()
        return value

    def test_dry_run_is_read_only(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self.make_db(directory)
            before = path.read_bytes()
            result = repair_mod.repair(path, since="2026-11-09T00:00:00Z", until="2026-11-11T00:00:00Z")
            self.assertEqual(result["mode"], "dry-run")
            self.assertEqual(result["changes"][0]["after"], 2)
            self.assertEqual(self.read_counter(path, "alpha"), 5)
            self.assertEqual(path.read_bytes(), before)

    def test_apply_only_lowers_to_non_transport_count(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self.make_db(directory)
            result = repair_mod.repair(path, apply=True, since="2026-11-09T00:00:00Z", until="2026-11-11T00:00:00Z")
            self.assertEqual(result["mode"], "apply")
            self.assertEqual(self.read_counter(path, "alpha"), 2)
            self.assertEqual(self.read_counter(path, "beta"), 0)

    def test_cli_output_is_json(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self.make_db(directory)
            self.assertEqual(repair_mod.main([
                str(path), "--since", "2026-11-09T00:00:00Z",
                "--until", "2026-11-11T00:00:00Z",
            ]), 0)
            self.assertEqual(self.read_counter(path, "alpha"), 5)
            payload = json.loads(repair_mod.repair(
                path, since="2026-11-09T00:00:00Z", until="2026-11-11T00:00:00Z"
            )["changes"][0].get("project") and '{"ok": true}')
            self.assertTrue(payload["ok"])
            self.assertEqual(self.read_counter(path, "alpha"), 5)


if __name__ == "__main__":
    unittest.main()
