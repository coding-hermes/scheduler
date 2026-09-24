"""SCHED-GAP-147 regression fixtures for check 7b (global-cap parity, the
``--global-cap`` argument) and for :func:`parse_toml_table`.

The GLOBAL --max-concurrent is daemon argv: it has no scheduler.db column to
assert a constant against, so the checker takes the daemon's real flag value
(``--global-cap N``) and parity-checks it against the ``[scheduler]
max_concurrent`` pin in fleet.toml — the key loader.go folds into the daemon's
resolved config (internal/config/loader.go:176) and that the explicit flag
overrides only when present. A stale pin silently re-levels every restart
that omits the flag.

Arms proven:
  * daemon=8 vs TOML pin 10 → exit 1, ``VIOLATION parity [scheduler]`` line;
  * daemon=10 vs TOML pin 10 → exit 0 (agreement);
  * flag omitted → check skipped (back-compat: nothing new fires);
  * a ``[[namespaces]] max_concurrent = 10`` value is NOT the global cap —
    the check must read ``[scheduler]``, not an array-of-table block
    (parse_toml_table vs parse_toml_blocks);
  * a nested ``[scheduler.sub]`` table does not leak its keys into the parent
    body scan.

Run:  python3 -m pytest tests/test_check_fleet_invariants_global_cap_parity.py -v
"""
from __future__ import annotations

import importlib.util
import io
import sqlite3
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _make_db(path: Path) -> Path:
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " command TEXT, prompt TEXT, workdir TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer"):
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))
    con.commit()
    con.close()
    return path


TOML_WITH_PIN = """\
[scheduler]
max_concurrent = 10

[[namespaces]]
id = "coding-hermes"
admission_mode = "tasks"
max_concurrent = 8
"""


def _run_gate(tmp_path: Path, toml_text: str, *extra_args: str) -> tuple[int, str]:
    db = _make_db(tmp_path / "scheduler.db")
    toml = tmp_path / "fleet.toml"
    toml.write_text(toml_text, encoding="utf-8")
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main(["--db", str(db), "--toml", str(toml),
                        "--board", str(tmp_path / "no-board.jsonl"), *extra_args])
    return rc, buf.getvalue()


def test_parse_toml_table_reads_single_bracket_tables():
    """parse_toml_blocks only matches [[...]]; the root [scheduler] table is
    single-bracket. The new parser must see it — this is the RED-proof for
    the would-be-silent no-op."""
    body = gate.parse_toml_table(TOML_WITH_PIN, "scheduler")
    assert "max_concurrent = 10" in body, body
    # ...and must NOT see an array-of-table block under that name.
    assert gate.parse_toml_blocks(TOML_WITH_PIN, "scheduler") == {}


def test_parse_toml_table_nested_child_stays_in_body():
    text = "[scheduler]\nmax_concurrent = 10\n\n[scheduler.blackout]\nstart = \"01:00\"\n\n[other]\nkey = 1\n"
    body = gate.parse_toml_table(text, "scheduler")
    assert "max_concurrent = 10" in body, body
    assert "start = \"01:00\"" in body, body
    assert "key = 1" not in body, body


def test_global_cap_disagreement_fires(tmp_path):
    rc, out = _run_gate(tmp_path, TOML_WITH_PIN, "--global-cap", "8")

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    got = [l for l in out.splitlines() if l.startswith("VIOLATION parity")]
    assert got == ["VIOLATION parity [scheduler]: global max_concurrent: daemon=8 toml=10 — a stale "
                   "[scheduler] max_concurrent pin overrides every restart that omits the flag"], got
    other = [l for l in out.splitlines()
             if l.startswith("VIOLATION ") and not l.startswith("VIOLATION parity")]
    assert other == [], f"unexpected extra violations:\n{out}"


def test_global_cap_agreement_passes(tmp_path):
    rc, out = _run_gate(tmp_path, TOML_WITH_PIN, "--global-cap", "10")

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_global_cap_omitted_is_skipped(tmp_path):
    """Without --global-cap the checker cannot see the daemon's argv: the
    check is skipped, never guessed."""
    rc, out = _run_gate(tmp_path, TOML_WITH_PIN)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_global_cap_ignores_namespace_array_blocks(tmp_path):
    """A [[namespaces]] max_concurrent = 10 is a per-namespace cap, not the
    global one. The namespace here is deliberately unknown to the DB (so the
    namespace-parity loop has no row to compare it against): the fleet must
    stay clean at daemon=8, proving the global check read nothing from the
    array-of-table block."""
    toml = ('[[namespaces]]\nid = "some-unknown-ns"\nadmission_mode = "cooldown"\n'
            'max_concurrent = 10\n')
    rc, out = _run_gate(tmp_path, toml, "--global-cap", "8")

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_global_cap_absent_toml_table_is_skipped(tmp_path):
    """fleet.toml without a [scheduler] table pins nothing — nothing to drift."""
    rc, out = _run_gate(tmp_path, "[other]\nkey = 1\n", "--global-cap", "8")

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
