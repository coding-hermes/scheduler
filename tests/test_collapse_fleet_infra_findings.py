"""SCHED-GAP-128 — the migration that collapses per-project copies.

Drives ``ops/collapse-fleet-infra-findings.py`` in-process against tmp boards.
The dangerous failures for a migration are silent: it can delete a
project-local finding, lose provenance, or fail to be re-runnable after a
partial crash. Each of those has an arm here.

Covered:

  * dry-run writes NOTHING (boards byte-identical, no archive);
  * apply removes the fleet-infra copies, KEEPS project-local rows, and files
    one ``FLEET-INFRA-N`` row per pattern carrying every collapsed
    (project, row id) in ``cross_ref.observations``;
  * the removed rows survive verbatim in the archive, and each touched board
    has a byte backup;
  * a second apply is a no-op (idempotency — the re-run after a partial
    failure);
  * closed copies are left alone unless ``--include-closed``;
  * linked git worktrees and unregistered directories are not discovered.

Run:  python3 -m pytest tests/test_collapse_fleet_infra_findings.py -v
"""
from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
LIB_PATH = REPO_ROOT / "ops" / "fleet-infra-findings.py"
COLLAPSE_PATH = REPO_ROOT / "ops" / "collapse-fleet-infra-findings.py"
SHIPPED_PATTERNS = REPO_ROOT / "ops" / "fleet-infra-patterns.json"


def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None, f"cannot load {path}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


lib = _load("fleet_infra_findings", LIB_PATH)
collapse = _load("collapse_fleet_infra_findings", COLLAPSE_PATH)

PORT_ROW = {
    "id": "QA-CONSENSUS-1-R8", "status": "pending", "priority": "P1",
    "title": "[P1] run_battery FAIL — bunkerd port-pool exhaustion on bunker-las-03",
    "created_at": "2026-09-14T00:00:00Z",
}
PORT_ROW_2 = {
    "id": "QA-WARPFS-1-7", "status": "pending", "priority": "P1",
    "title": "[P1] Bunker battery could not run — port pool is one range, held",
    "created_at": "2026-09-15T00:00:00Z",
}
STALE_ROW = {
    "id": "QA-UHLP-7", "status": "pending", "priority": "P1",
    "title": "[P1] Fix not deployed — 18-day-old bunker binary predates QA-BUNKER-4",
    "created_at": "2026-09-16T00:00:00Z",
}
LOCAL_ROW = {
    "id": "CHT-057", "status": "complete", "priority": "P3",
    "title": "DOGFOOD: probe defaults TOOLSD_BIN to ../build/toolsd (a stale-binary trap)",
    "reasoning": "the probe's default is a STALE-BINARY trap",
}
LOCAL_ROW_2 = {
    "id": "RELEASE-010", "status": "complete",
    "title": "Release readiness sweep: v0.4.0 blocked",
    "reasoning": "DF-WARPFS-108 14th stale-binary cycle on ~/.cargo/bin/hilo",
}


def _board(root: Path, project: str, rows: list[dict]) -> Path:
    board = root / project / ".coding-hermes" / "board"
    board.mkdir(parents=True, exist_ok=True)
    path = board / "tasks.jsonl"
    path.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    return path


def _fixture(tmp_path: Path):
    """Two project boards + the shared-infra board, isolated from the live fleet."""
    home = tmp_path / "home"
    consensus = _board(home, "consensus", [PORT_ROW, LOCAL_ROW_2])
    warpfs = _board(home, "warpfs", [PORT_ROW_2, STALE_ROW, LOCAL_ROW])
    infra = _board(home, "bunker", [
        {"id": "BUNKER-1", "status": "pending", "title": "host maintenance"},
    ])
    pats = tmp_path / "patterns.json"
    pats.write_text(SHIPPED_PATTERNS.read_text(encoding="utf-8"), encoding="utf-8")
    return home, [str(consensus), str(warpfs)], str(infra), str(pats)


def _run(tmp_path, home, boards, infra, pats, **kw):
    lib._PATTERN_CACHE.clear()
    kw.setdefault("archive", str(tmp_path / "archive.jsonl"))
    # Direct run() callers opt into the fixture's cross-project mutation; the
    # shell wrapper tests separately pin the operator-facing acknowledgement.
    kw.setdefault("cross_project", True)
    return collapse.run(boards=boards, infra_board=infra, home=str(home),
                        include_worktrees=False, now=1000.0,
                        backup_dir=str(tmp_path / "backups"), **kw)


def test_dry_run_writes_nothing(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    before = {b: Path(b).read_text(encoding="utf-8") for b in boards + [infra]}
    report = _run(tmp_path, home, boards, infra, pats, apply=False)
    assert report["mode"] == "dry-run"
    assert report["collapsed_rows"] == 3
    assert report["changed_boards"] == []
    assert {b: Path(b).read_text(encoding="utf-8") for b in boards + [infra]} == before
    assert not (tmp_path / "archive.jsonl").exists()


def test_apply_collapses_copies_and_keeps_project_local_rows(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    report = _run(tmp_path, home, boards, infra, pats, apply=True)

    assert report["collapsed_rows"] == 3
    assert sorted(p["pattern"] for p in report["patterns"]) == ["port_pool", "stale_binary"]
    ids = {p["pattern"]: p["row_id"] for p in report["patterns"]}
    assert ids["port_pool"] != ids["stale_binary"], "one row per pattern, distinct ids"

    # Project boards lost exactly the fleet-infra rows and kept their own work.
    consensus_rows = lib.read_board(boards[0])
    assert [r["id"] for r in consensus_rows] == ["RELEASE-010"]
    warpfs_rows = lib.read_board(boards[1])
    assert [r["id"] for r in warpfs_rows] == ["CHT-057"]
    # A local row that MENTIONS the vocabulary survives — the discriminator.
    assert "stale-binary" in warpfs_rows[0]["reasoning"].lower()

    # The consolidated row carries every collapsed observation.
    infra_rows = {r["id"]: r for r in lib.read_board(infra)}
    port_row = infra_rows[ids["port_pool"]]
    assert port_row["classification"] == "fleet-infra:port_pool"
    assert port_row["origin"] == lib.ORIGIN_CONSOLIDATED
    assert port_row["status"] == "pending"
    projects = {o["project"] for o in port_row["cross_ref"]["observations"]}
    assert projects == {"consensus", "warpfs"}
    row_ids = {o["row_id"] for o in port_row["cross_ref"]["observations"]}
    assert row_ids == {"QA-CONSENSUS-1-R8", "QA-WARPFS-1-7"}
    stale_row = infra_rows[ids["stale_binary"]]
    assert [o["row_id"] for o in stale_row["cross_ref"]["observations"]] == ["QA-UHLP-7"]

    # Nothing was destroyed: archive holds the originals, boards are backed up.
    archived = [json.loads(ln) for ln in Path(report["archive"]).read_text().splitlines() if ln.strip()]
    assert {a["id"] for a in archived} == {"QA-CONSENSUS-1-R8", "QA-WARPFS-1-7", "QA-UHLP-7"}
    assert all(a["_collapsed"]["consolidated_row"] for a in archived)
    assert len(report["changed_boards"]) == 2
    for changed in report["changed_boards"]:
        assert Path(changed["backup"]).exists()


def test_second_apply_is_a_noop(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    first = _run(tmp_path, home, boards, infra, pats, apply=True)
    assert first["collapsed_rows"] == 3
    after_first = {b: Path(b).read_text(encoding="utf-8") for b in boards + [infra]}
    second = _run(tmp_path, home, boards, infra, pats, apply=True)
    assert second["collapsed_rows"] == 0
    assert second["changed_boards"] == []
    assert {b: Path(b).read_text(encoding="utf-8") for b in boards + [infra]} == after_first


def test_closed_copies_are_left_alone_unless_asked(tmp_path):
    home = tmp_path / "home"
    closed = dict(PORT_ROW, status="complete", id="QA-OLD-1")
    board = _board(home, "consensus", [closed, PORT_ROW_2])
    infra = _board(home, "bunker", [{"id": "BUNKER-1", "title": "x", "status": "pending"}])
    pats = tmp_path / "patterns.json"
    pats.write_text(SHIPPED_PATTERNS.read_text(encoding="utf-8"), encoding="utf-8")

    report = _run(tmp_path, home, [str(board)], str(infra), str(pats), apply=True)
    assert report["collapsed_rows"] == 1
    assert [r["id"] for r in lib.read_board(str(board))] == ["QA-OLD-1"]

    report2 = _run(tmp_path, home, [str(board)], str(infra), str(pats), apply=True,
                   include_closed=True)
    assert report2["collapsed_rows"] == 1
    assert lib.read_board(str(board)) == []


def test_an_existing_open_carrier_is_extended_not_duplicated(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    existing = {
        "id": "FLEET-INFRA-1", "status": "pending", "origin": lib.ORIGIN_CONSOLIDATED,
        "classification": "fleet-infra:port_pool",
        "title": "already carried", "cross_ref": {"pattern": "port_pool", "observations": []},
    }
    lib.append_jsonl(infra, [existing])
    report = _run(tmp_path, home, boards, infra, pats, apply=True)
    port = [p for p in report["patterns"] if p["pattern"] == "port_pool"][0]
    assert port["row_id"] == "FLEET-INFRA-1" and port["action"] == "extended"
    rows = [r for r in lib.read_board(infra) if r.get("id") == "FLEET-INFRA-1"]
    assert len(rows) == 1
    assert {o["project"] for o in rows[0]["cross_ref"]["observations"]} == {"consensus", "warpfs"}


def test_unroutable_infra_board_touches_nothing(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    before = {b: Path(b).read_text(encoding="utf-8") for b in boards}
    report = _run(tmp_path, home, boards, str(tmp_path / "missing.jsonl"), pats, apply=True)
    assert report["infra_board"] == ""
    assert report["collapsed_rows"] == 0
    assert {b: Path(b).read_text(encoding="utf-8") for b in boards} == before


def test_discovery_skips_linked_worktrees_and_unregistered_dirs(tmp_path):
    home = tmp_path / "home"
    _board(home, "consensus", [PORT_ROW])
    # A linked worktree: .git is a FILE carrying "gitdir:".
    wt_board = home / "consensus-wt-1" / ".coding-hermes" / "board"
    wt_board.mkdir(parents=True)
    (wt_board / "tasks.jsonl").write_text(json.dumps(PORT_ROW) + "\n", encoding="utf-8")
    (home / "consensus-wt-1" / ".git").write_text(
        "gitdir: /home/kara/consensus/.git/worktrees/consensus-wt-1\n", encoding="utf-8")
    # An unregistered scratch clone.
    _board(home, "qa9-frozen-before", [PORT_ROW])

    assert lib.is_linked_worktree(str(home / "consensus-wt-1")) is True
    assert lib.is_linked_worktree(str(home / "consensus")) is False
    found = lib.discover_boards(home=str(home), db_path=str(tmp_path / "no.db"),
                               include_unregistered=False)
    assert found == []  # DB is unreachable and the glob is opt-in
    found = lib.discover_boards(home=str(home), db_path=str(tmp_path / "no.db"),
                               include_unregistered=True)
    assert [Path(p).parts[-4] for p in found] == ["consensus", "qa9-frozen-before"]


def test_concurrent_lane_append_blocks_the_rewrite(tmp_path):
    """A lane that appends mid-run must not have its row dropped by our rename."""
    home = tmp_path / "home"
    board = _board(home, "consensus", [PORT_ROW, LOCAL_ROW])
    by_pattern = collapse.collect_candidates([str(board)], include_closed=False)
    assert by_pattern  # the row is a candidate …
    # … and a sibling lane appends while we classify.
    lib.append_jsonl(str(board), [{"id": "NEW-1", "status": "pending", "title": "lane row"}])
    with __import__("pytest").raises(collapse.lib.BoardChangedError):
        collapse.rewrite_board(str(board), {0}, expect=by_pattern["port_pool"][0]["signature"])
    # Nothing was written: the lane's row and the original both survive.
    assert [r["id"] for r in lib.read_board(str(board))] == [PORT_ROW["id"], LOCAL_ROW["id"], "NEW-1"]


def test_sweep_mode_prints_one_line_and_never_fails(tmp_path):
    home, boards, infra, pats = _fixture(tmp_path)
    lib._PATTERN_CACHE.clear()
    out = []
    old_stdout = sys.stdout
    try:
        import io
        sys.stdout = io.StringIO()
        rc = collapse.main(["--apply", "--cross-project", "--sweep", "--boards", ",".join(boards),
                            "--infra-board", infra, "--home", str(home),
                            "--archive", str(tmp_path / "a.jsonl")])
        out.append(sys.stdout.getvalue())
    finally:
        sys.stdout = old_stdout
    assert rc == 0
    text = out[0].strip()
    assert text.startswith("FLEET-INFRA-COLLAPSE apply collapsed=")
    assert "wrote=" in text
    assert "FLEET-INFRA-1" in text and "FLEET-INFRA-2" in text
    assert "\n" not in text


def _run_wrapper(tmp_path, *args):
    """Run the shipped wrapper against only temporary fixture boards."""
    import os
    import subprocess

    home, boards, infra, _ = _fixture(tmp_path)
    env = dict(os.environ, HOME=str(tmp_path / "operator-home"))
    result = subprocess.run(
        [str(REPO_ROOT / "ops" / "fleet-infra-sweep.sh"),
         *args, "--boards", ",".join(boards), "--infra-board", infra,
         "--home", str(home), "--archive", str(tmp_path / "archive.jsonl")],
        cwd=REPO_ROOT, env=env, text=True, capture_output=True, check=False,
    )
    return result, boards, infra


def _snapshot(paths):
    return {
        str(path): (Path(path).read_bytes(), Path(path).stat().st_size,
                    Path(path).stat().st_mtime_ns)
        for path in paths
    }


def test_wrapper_default_is_read_only_and_apply_names_writes(tmp_path):
    _, boards, infra = _run_wrapper(tmp_path)
    paths = [Path(board) for board in boards + [infra]]
    before = _snapshot(paths)
    # The fixture was created inside _run_wrapper; snapshot it before invoking
    # the wrapper a second time, which is the documented no-flag command.
    result = subprocess.run(
        [str(REPO_ROOT / "ops" / "fleet-infra-sweep.sh"), "--boards", ",".join(boards),
         "--infra-board", infra, "--home", str(tmp_path / "home"),
         "--archive", str(tmp_path / "default-archive.jsonl")],
        cwd=REPO_ROOT, env=dict(os.environ, HOME=str(tmp_path / "operator-home")),
        text=True, capture_output=True, check=False,
    )
    assert result.returncode == 0
    assert "dry-run" in result.stdout and "wrote nothing" in result.stdout
    assert _snapshot(paths) == before
    assert not (tmp_path / "default-archive.jsonl").exists()

    applied = subprocess.run(
        [str(REPO_ROOT / "ops" / "fleet-infra-sweep.sh"), "--apply", "--cross-project",
         "--boards", ",".join(boards), "--infra-board", infra,
         "--home", str(tmp_path / "home"), "--archive", str(tmp_path / "apply-archive.jsonl")],
        cwd=REPO_ROOT, env=dict(os.environ, HOME=str(tmp_path / "operator-home")),
        text=True, capture_output=True, check=False,
    )
    assert applied.returncode == 0
    assert "apply" in applied.stdout and "wrote=" in applied.stdout
    assert "FLEET-INFRA-1" in applied.stdout and "FLEET-INFRA-2" in applied.stdout
    assert str(boards[0]) in applied.stdout


def test_wrapper_refuses_cross_project_apply_without_ack(tmp_path):
    import os
    import subprocess

    home, boards, infra, _ = _fixture(tmp_path)
    paths = [Path(board) for board in boards + [infra]]
    before = _snapshot(paths)
    result = subprocess.run(
        [str(REPO_ROOT / "ops" / "fleet-infra-sweep.sh"), "--apply",
         "--boards", ",".join(boards), "--infra-board", infra,
         "--home", str(home), "--archive", str(tmp_path / "refused.jsonl")],
        cwd=REPO_ROOT, env=dict(os.environ, HOME=str(tmp_path / "operator-home")),
        text=True, capture_output=True, check=False,
    )
    assert result.returncode == 0
    assert "REFUSED" in result.stdout
    assert "wrote nothing" in result.stdout
    assert _snapshot(paths) == before
    assert not (tmp_path / "refused.jsonl").exists()
