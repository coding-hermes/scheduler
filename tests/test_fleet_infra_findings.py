"""SCHED-GAP-128 — classification, routing and the 30-minute filing throttle.

Drives ``ops/fleet-infra-findings.py`` in-process against tmp boards. Both arms
of every rule are pinned, because a classifier's dangerous failure is silent:
it does not crash, it just sweeps real work into the wrong bucket (or leaves
the noise where it was).

Covered here:

  * ``classify`` — pattern + required substrate co-signal; a bare keyword is
    NOT enough (a project's own stale binary is project-local).
  * ``classify_row`` — the pattern must be in the TITLE while the co-signal may
    come from the detail; a body-only mention never classifies (the DOC-4 /
    TR-238 shape measured on the live fleet).
  * ``is_excluded`` — the tracking row that NAMES the class to fix it is not a
    member of the class.
  * ``file_finding`` — local passthrough; infra row filed with
    ``classification`` + ``cross_ref``; a second project observing the SAME
    pattern extends the open row instead of minting a second one; the
    observing board gets a ``fleet_infra_diverted`` event and no task row.
  * ``BoardFilingThrottle`` — the 30-minute window on the (project, pattern)
    key, independent per pattern, with a durable read-back.
  * ``append_jsonl`` — the one-object-per-line invariant on an unterminated tail.

Run:  python3 -m pytest tests/test_fleet_infra_findings.py -v
"""
from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
LIB_PATH = REPO_ROOT / "ops" / "fleet-infra-findings.py"


def _load_lib():
    spec = importlib.util.spec_from_file_location("fleet_infra_findings", LIB_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {LIB_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


lib = _load_lib()

SHIPPED_PATTERNS = REPO_ROOT / "ops" / "fleet-infra-patterns.json"

PORT_TITLE = "[P1] run_battery FAIL — bunkerd port-pool exhaustion on bunker-las-03"
STALE_TITLE = "[P1] Fix not deployed — bunker-las-02 runs 18-day-old binary predating QA-BUNKER-4"


def _write_patterns(tmp_path: Path) -> Path:
    """Copy the SHIPPED table — tests pin production behaviour, not a mock.

    A synthetic table would pass while the deployed one drifted (a pattern
    typo'd into never matching is exactly how a classifier silently becomes a
    no-op).
    """
    path = tmp_path / "patterns.json"
    path.write_text(SHIPPED_PATTERNS.read_text(encoding="utf-8"), encoding="utf-8")
    return path


def test_shipped_pattern_table_parses():
    table = json.loads(SHIPPED_PATTERNS.read_text(encoding="utf-8"))
    names = [p["name"] for p in table["patterns"]]
    assert names == ["port_pool", "dropped_config", "stale_binary", "gateway_auth"]
    for entry in table["patterns"]:
        assert entry.get("pattern"), entry
        assert entry.get("why"), "every pattern must justify its requires half"
    assert table["exclude"], "the tracking-row exclusion must not be empty"


def _boards(tmp_path: Path, rows_by_project: dict[str, list[dict]]):
    """Build ``<home>/<project>/.coding-hermes/board/tasks.jsonl`` fixtures."""
    home = tmp_path / "home"
    for project, rows in rows_by_project.items():
        board = home / project / ".coding-hermes" / "board"
        board.mkdir(parents=True)
        (board / "tasks.jsonl").write_text(
            "".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    return home


# ---------------------------------------------------------------------------
# classification
# ---------------------------------------------------------------------------
def test_classify_requires_the_substrate_cosignal(tmp_path):
    pats = _write_patterns(tmp_path)
    # Both halves: fleet-infra.
    assert lib.classify(PORT_TITLE, str(pats))["pattern"] == "port_pool"
    assert lib.classify(STALE_TITLE, str(pats))["pattern"] == "stale_binary"
    # Keyword alone — a PROJECT's own stale binary — is project-local.
    local = "P1 — Deployed binaries stale vs HEAD: repo ./dagger and ~/.local/bin/dagger"
    assert lib.classify(local, str(pats))["route"] == "local"
    # Keyword alone — a project's own port allocation bug.
    assert lib.classify("Service binds :8080 twice — port pool misuse",
                        str(pats))["route"] == "local"
    # Absence of a finding is never an infra finding.
    assert lib.classify("", str(pats))["route"] == "local"
    assert lib.classify("   ", str(pats))["route"] == "local"


def test_classify_row_needs_the_pattern_in_the_title(tmp_path):
    pats = _write_patterns(tmp_path)
    # Body-only mention: the DOC-4 / TR-238 shape — NOT a member.
    prose = {"id": "DOC-4",
             "title": "[docs] 24 config keys are absent from the env table",
             "reasoning": "the five P1 rows are all bunkerd port-pool exhaustion"}
    assert lib.classify_row(prose, str(pats))["route"] == "local"
    # Pattern in the title, co-signal only in the detail: IS a member.
    split = {"id": "QA-KOBAYASHI-MARU-2-R8",
             "title": "[P2] Pool-exhaustion class recurs despite 2c67b47 fix",
             "detail": "Live: ls /run/bunker=12 dirs, bunker-las-03 pool held"}
    got = lib.classify_row(split, str(pats))
    assert got["route"] == "infra" and got["pattern"] == "port_pool"
    # The chatty audit fields never supply the co-signal.
    chatty = {"id": "DF-98", "title": "workspace ephemeral: verified on stale binary",
              "foreman_note": "meanwhile bunker-las-03 is stale"}
    assert lib.classify_row(chatty, str(pats))["route"] == "local"


def test_excluded_tracking_row_is_not_a_member(tmp_path):
    pats = _write_patterns(tmp_path)
    tracking = {"id": "SCHED-GAP-128",
                "title": "Fleet-wide infra alert spam: bunker port-pool findings duplicated"}
    assert lib.is_excluded(tracking, str(pats)) is True
    assert lib.classify_row(tracking, str(pats))["route"] == "local"
    # …but a stamp does not rescue it either: the row is the FIX, not a copy.
    stamped = dict(tracking, classification="fleet-infra:port_pool")
    assert lib.classify_row(stamped, str(pats))["route"] == "local"


def test_stamped_classification_survives_a_pattern_table_change(tmp_path):
    pats = _write_patterns(tmp_path)
    row = {"id": "FLEET-INFRA-7", "title": "consolidated",
           "classification": "fleet-infra:port_pool"}
    assert lib.row_pattern(row, str(pats)) == "port_pool"
    assert lib.is_infra_carrier(row) is True
    # A board's own native row that merely discusses the host is not a carrier.
    assert lib.is_infra_carrier({"id": "NET-BUNKER-009",
                                 "title": "port-pool leak"}) is False


# ---------------------------------------------------------------------------
# board io
# ---------------------------------------------------------------------------
def test_append_jsonl_terminates_an_unterminated_tail(tmp_path):
    board = tmp_path / "tasks.jsonl"
    board.write_text('{"id": "A"}', encoding="utf-8")  # no trailing newline
    lib.append_jsonl(str(board), [{"id": "B"}])
    lines = board.read_text(encoding="utf-8").splitlines()
    assert lines == ['{"id": "A"}', '{"id": "B"}']
    assert board.read_text(encoding="utf-8").endswith("\n")


def test_mint_infra_id_continues_past_the_highest_suffix():
    assert lib.mint_infra_id(["FLEET-INFRA-1", "FLEET-INFRA-9"]) == "FLEET-INFRA-10"
    assert lib.mint_infra_id([]) == "FLEET-INFRA-1"
    assert lib.mint_infra_id(["FLEET-INFRA-2"]) == "FLEET-INFRA-3"


# ---------------------------------------------------------------------------
# filing the finding once
# ---------------------------------------------------------------------------
def _filing_fixture(tmp_path):
    pats = _write_patterns(tmp_path)
    home = _boards(tmp_path, {
        "consensus": [{"id": "QA-CONSENSUS-1", "title": "battery", "status": "pending"}],
        "bunker": [{"id": "BUNKER-1", "title": "host pool", "status": "pending"}],
    })
    source = home / "consensus" / ".coding-hermes" / "board" / "tasks.jsonl"
    infra = home / "bunker" / ".coding-hermes" / "board" / "tasks.jsonl"
    throttle = lib.BoardFilingThrottle(str(tmp_path / "throttle.json"), window_s=1800)
    return pats, home, source, infra, throttle


def test_local_finding_passes_through_untouched(tmp_path):
    pats, home, source, infra, throttle = _filing_fixture(tmp_path)
    before = infra.read_text(encoding="utf-8")
    got = lib.file_finding("lint failure in pkg/foo", "consensus", infra_board=str(infra),
                           board=str(source), apply=True, throttle=throttle,
                           patterns_path=str(pats))
    assert got["route"] == "local" and got["action"] == "passthrough"
    assert infra.read_text(encoding="utf-8") == before


def test_fleet_infra_finding_is_filed_once_with_cross_ref(tmp_path):
    pats, home, source, infra, throttle = _filing_fixture(tmp_path)
    lib._PATTERN_CACHE.clear()
    got = lib.file_finding(PORT_TITLE, "consensus", infra_board=str(infra),
                           detail="8/8 users busy, 1 range", board=str(source),
                           source="qa", apply=True, throttle=throttle, now=1000.0,
                           patterns_path=str(pats))
    assert got["route"] == "infra" and got["action"] == "filed"
    assert got["row_id"] == "FLEET-INFRA-1"

    rows = lib.read_board(str(infra))
    assert rows[-1]["id"] == "FLEET-INFRA-1"
    assert rows[-1]["classification"] == "fleet-infra:port_pool"
    assert rows[-1]["origin"] == lib.ORIGIN_ROUTED
    obs = rows[-1]["cross_ref"]["observations"]
    assert obs[0]["project"] == "consensus"
    # The observing board got a diverted EVENT and no task row.
    assert len(lib.read_board(str(source))) == 1
    events = lib.read_board(lib.events_path_for(str(source)))
    assert events[-1]["kind"] == "fleet_infra_diverted"
    assert "FLEET-INFRA-1" in events[-1]["detail"]
    # And a task_added event on the infra board.
    infra_events = lib.read_board(lib.events_path_for(str(infra)))
    assert infra_events[-1]["task_id"] == "FLEET-INFRA-1"


def test_second_project_extends_the_open_row_instead_of_minting(tmp_path):
    pats, home, source, infra, throttle = _filing_fixture(tmp_path)
    other = home / "warpfs" / ".coding-hermes" / "board"
    other.mkdir(parents=True)
    other_board = other / "tasks.jsonl"
    other_board.write_text('{"id": "QA-WARPFS-1", "status": "pending"}\n', encoding="utf-8")

    lib.file_finding(PORT_TITLE, "consensus", infra_board=str(infra), board=str(source),
                     apply=True, throttle=throttle, now=1000.0, patterns_path=str(pats))
    rows = lib.read_board(str(infra))
    got = lib.file_finding("[P1] run_battery FAIL — port range pool exhausted on bunker-las-03",
                           "warpfs", infra_board=str(infra), board=str(other_board),
                           apply=True, throttle=throttle, now=1100.0, patterns_path=str(pats))
    assert got["action"] == "observed" and got["row_id"] == "FLEET-INFRA-1"
    after = lib.read_board(str(infra))
    assert len(after) == len(rows)  # no second row
    projects = [o["project"] for o in after[-1]["cross_ref"]["observations"]]
    assert projects == ["consensus", "warpfs"]


def test_throttle_suppresses_a_refile_inside_thirty_minutes(tmp_path):
    pats, home, source, infra, throttle = _filing_fixture(tmp_path)
    lib.file_finding(PORT_TITLE, "consensus", infra_board=str(infra), board=str(source),
                     apply=True, throttle=throttle, now=1000.0, patterns_path=str(pats))
    rows = lib.read_board(str(infra))
    # Same project, same pattern, 5 minutes later → throttled, no write.
    again = lib.file_finding(
        "[P1] bunkerd port-pool exhaustion again on bunker-las-02", "consensus",
        infra_board=str(infra), board=str(source), apply=True, throttle=throttle,
        now=1300.0, patterns_path=str(pats))
    assert again["action"] == "throttled"
    assert len(lib.read_board(str(infra))) == len(rows)
    # A DIFFERENT pattern is an independent cadence → files.
    other = lib.file_finding(STALE_TITLE, "consensus", infra_board=str(infra),
                             board=str(source), apply=True, throttle=throttle,
                             now=1300.0, patterns_path=str(pats))
    assert other["action"] == "filed"
    # Past the window, the same pattern files again (a new row, since the first
    # is now reported by the same project).
    later = lib.file_finding(
        "[P1] bunkerd port-pool exhaustion again on bunker-las-02", "consensus",
        infra_board=str(infra), board=str(source), apply=True, throttle=throttle,
        now=1000.0 + 1801, patterns_path=str(pats))
    assert later["action"] in ("filed", "observed")


def test_throttle_window_defaults_to_thirty_minutes():
    thr = lib.BoardFilingThrottle("/tmp/never-written-throttle.json")
    assert thr.window_s == 1800


def test_throttle_key_separates_the_halves():
    assert lib.throttle_key("a", "b_c") != lib.throttle_key("a_b", "c")
    assert lib.throttle_key("p", "port_pool") == "p\x00port_pool"


def test_unroutable_infra_board_degrades_to_local(tmp_path):
    pats, home, source, infra, throttle = _filing_fixture(tmp_path)
    got = lib.file_finding(PORT_TITLE, "consensus", infra_board=str(tmp_path / "nope.jsonl"),
                           board=str(source), apply=True, throttle=throttle,
                           now=1000.0, patterns_path=str(pats))
    assert got["route"] == "local" and got["action"] == "unroutable"
    assert "not lost" in got["reason"]
