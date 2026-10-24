"""DOC-25 parity gate: the ``ops/README.md`` class table vs ``CHECK_CLASSES``.

``ops/README.md`` states its own rule — "A class missing from the table is a
gap in the gate" — and then violated it: three live classes
(``namespace-membership``, ``board-id-slot``, ``cooldown-pin-pacing``) had no
row, so an operator could not tell whether they were covered or vacuous. Prose
cannot hold that invariant, so this module pins it the way
``internal/mcp/agents_endpoint_parity_test.go`` pins the route table:

  * every class in ``CHECK_CLASSES`` has exactly ONE table row, and every row
    names a class that still exists (drift fails in BOTH directions — a new
    check cannot land undocumented, and a removed check cannot leave a ghost
    row claiming coverage);
  * each row's fixture column resolves to files that actually exist, so a row
    cannot promise a fixture module that was deleted or renamed;
  * the flag-only rows (``parity`` with ``--global-cap``) are listed
    EXPLICITLY, never inferred, so a variant is not mistaken for a class;
  * the rows DOC-25 added state their ``--board-only`` (CI) behavior — a
    reader must never mistake a CI skip for coverage;
  * an INFO line may carry a name that is neither a class nor one of the
    documented METER aliases (``open-rows`` is ``board-id-slot``'s census) —
    checked against a REAL gate run, so a meter can never masquerade as a class.

Run:  python3 -m pytest tests/test_check_fleet_invariants_class_table.py -v
"""
from __future__ import annotations

import importlib.util
import io
import json
import re
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"
README_PATH = REPO_ROOT / "ops" / "README.md"

# Rows in the table that are NOT ``CHECK_CLASSES`` entries: a named FLAG
# variant of an existing class. Explicitly listed (never inferred) so the
# parity math below stays exact: the table may carry these rows IN ADDITION to
# one row per class, and nothing else.
FLAG_ONLY_ROWS = ("`parity` (global cap)",)

# INFO lines may carry a class name only when that name is a real class or one
# of these documented METER aliases — they are deliberately NOT in
# ``CHECK_CLASSES`` (a meter reports over the whole board, it asserts nothing
# per subject).
INFO_METER_ALIASES = ("open-rows",)

# The classes DOC-25 added. Their rows must say what the CI shape does with
# them (asserts / skips), because all three are silent-by-design in some mode.
BOARD_ONLY_DOCUMENTED = (
    "namespace-membership",
    "board-id-slot",
    "cooldown-pin-pacing",
)


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _cells(row: str) -> list[str]:
    return [c.strip() for c in row.strip().strip("|").split("|")]


def _is_separator(cells: list[str]) -> bool:
    return all(set(c) <= set("-: ") for c in cells)


def _table_rows() -> list[list[str]]:
    """The CLASS TABLE rows of ops/README.md, header dropped.

    Scoped to the ``### Class → fixture table`` section so a table elsewhere in
    the README (the shared-infra piece/role table) can never satisfy a class
    row.
    """
    lines = README_PATH.read_text(encoding="utf-8").splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith("### Class")]
    assert starts, "ops/README.md lost its '### Class → fixture table' section"
    rows: list[list[str]] = []
    for line in lines[starts[0] + 1:]:
        if line.startswith("### "):
            break
        stripped = line.strip()
        if not stripped.startswith("|"):
            continue
        cells = _cells(stripped)
        if len(cells) < 2 or _is_separator(cells):
            continue
        rows.append(cells)
    assert rows, "the class table section carries no rows"
    return rows[1:]  # drop the header row


def _class_of(cell: str) -> str | None:
    """The class name a table row names: its FIRST backticked token."""
    match = re.search(r"`([^`]+)`", cell)
    return match.group(1) if match else None


def _row_for(class_name: str) -> list[str]:
    for cells in _table_rows():
        if _class_of(cells[0]) == class_name:
            return cells
    raise AssertionError(f"no table row for class {class_name!r}")


def test_every_check_class_has_a_table_row():
    """The defect DOC-25 fixes: a live class with no row is a gap in the gate."""
    documented = {_class_of(cells[0]) for cells in _table_rows()}
    missing = [c for c in gate.CHECK_CLASSES if c not in documented]
    assert missing == [], (
        "CHECK_CLASSES entries with no row in ops/README.md's class table "
        f"(a class missing from the table is a gap in the gate): {missing}")


def test_no_table_row_names_a_class_that_does_not_exist():
    """The other direction: a row for a removed class claims coverage nothing
    provides."""
    live = set(gate.CHECK_CLASSES)
    dead = sorted({_class_of(cells[0]) for cells in _table_rows()} - live)
    assert dead == [], (
        f"class table rows naming classes absent from CHECK_CLASSES: {dead}")


def test_each_class_has_exactly_one_row():
    """Two rows for one class make "which row is authoritative" ambiguous."""
    seen: dict[str, int] = {}
    for cells in _table_rows():
        name = _class_of(cells[0])
        assert name, f"table row names no backticked class: {cells[0]!r}"
        seen[name] = seen.get(name, 0) + 1
    dupes = {name: n for name, n in seen.items() if n > 1}
    # The ONLY legitimate duplicate is the explicitly listed flag-only variant,
    # which still names its parent class in cell 1.
    assert dupes == {"parity": 2}, f"unexpected duplicate class rows: {dupes}"


def test_flag_only_rows_are_explicitly_listed():
    """``parity (global cap)`` is a ``--global-cap`` VARIANT, not a class. It is
    allowed only because it is listed here — never inferred from prose."""
    labels = [cells[0] for cells in _table_rows()
              if cells[0] != f"`{_class_of(cells[0])}`"]
    assert sorted(labels) == sorted(FLAG_ONLY_ROWS), (
        "table rows whose first cell is not a bare class name must be listed in "
        f"FLAG_ONLY_ROWS — got {labels}, listed {list(FLAG_ONLY_ROWS)}")
    for label in FLAG_ONLY_ROWS:
        assert _class_of(label) in gate.CHECK_CLASSES, (
            f"flag-only row {label!r} names a class that does not exist")


def test_every_row_names_a_fixture_that_exists():
    """A row promising a fixture module that was deleted is the same lie as a
    missing row: the coverage claim has nothing behind it."""
    for cells in _table_rows():
        name = _class_of(cells[0])
        row_text = " | ".join(cells)
        paths = re.findall(r"`(tests/[^`]+\.(?:py|sh))`", row_text)
        assert paths, f"row {name!r} names no fixture path under tests/: {cells}"
        for rel in paths:
            assert (REPO_ROOT / rel).is_file(), (
                f"row {name!r} names fixture {rel} which does not exist")


def test_added_classes_state_their_board_only_behavior():
    """AC-4: a reader must be able to tell a CI skip from coverage. Each
    DOC-25 row says what the ``--board-only`` (CI) shape does with it."""
    for name in BOARD_ONLY_DOCUMENTED:
        cells = _row_for(name)
        row_text = " | ".join(cells)
        assert "board-only" in row_text.lower(), (
            f"row {name!r} does not state its --board-only (CI) behavior")
        assert re.search(r"(?i)\b(asserts|skips)\b", row_text), (
            f"row {name!r} must say whether the class ASSERTS or SKIPS in "
            "--board-only, so a CI skip cannot read as coverage")


def test_open_rows_info_tag_is_a_documented_alias_not_a_class():
    """``board-id-slot``'s census rides an ``open-rows`` INFO line. It is a
    METER alias, not a class: both facts are pinned (not in CHECK_CLASSES, and
    documented on the board-id-slot row)."""
    assert "open-rows" not in gate.CHECK_CLASSES, gate.CHECK_CLASSES
    assert "open-rows" in INFO_METER_ALIASES
    row_text = " | ".join(_row_for("board-id-slot"))
    assert "open-rows" in row_text, (
        "the board-id-slot row must document that its census INFO line is "
        "tagged `open-rows` (an alias meter), or a reader filtering the JSON "
        "by class cannot find it")


def test_emitted_info_classes_are_classes_or_documented_meter_aliases(tmp_path):
    """Behavioural half of the alias rule: run the gate for real and check what
    it actually tags its INFO lines with. An undeclared tag would be a class
    name an operator can never look up in the table."""
    board = tmp_path / "tasks.jsonl"
    board.write_text(json.dumps({
        "id": "DOC-25", "title": "one open row", "status": "pending",
        "priority": "P3",
    }) + "\n", encoding="utf-8")
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "no-scheduler.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(board),
            "--board-only",
            "--json",
        ])
    assert rc == 0, f"gate exited {rc} on a clean one-row board:\n{buf.getvalue()}"
    payload = json.loads(buf.getvalue())
    allowed = set(gate.CHECK_CLASSES) | set(INFO_METER_ALIASES)
    undeclared = sorted({i["class"] for i in payload["info"]} - allowed)
    assert undeclared == [], (
        f"INFO lines tagged with names that are neither a class nor a documented "
        f"meter alias: {undeclared}")
