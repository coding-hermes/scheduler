"""SCHED-GAP-1650 regression fixtures for check 12 (class
``lane-instructions``).

An enabled lane dispatched with NO instructions at all: the tick prompt is
built from two CONFIG layers (internal/scheduler/spawn.go
buildForemanPrompt) — the namespace default_prompt with the lane's own
prompt appended (or REPLACED by it when prompt_mode=replace) — and both
layers ~empty means the lane runs on the generic built-in body and
improvises. Measured 2026-09-27: exactly two of 361 enabled lanes were
uninstructed (auger foreman own 0 / namespace fallback 0; auger-releng own
296 / no fallback), and auger was the period's most expensive lane (~$117
across five ticks). The owner fixed both live; this battery pins the gate
so the shape cannot recur.

Both arms are proven, plus the conjunction is pinned from BOTH sides — a
one-sided length test would flag the 38 healthy empty-prompt lanes:

  * seeded violations fire (exit 1 naming the lane): own 0 over an empty
    namespace default (the auger shape), own 296 with no fallback (the
    auger-releng shape), both sides just under the threshold, a short
    prompt_mode=replace prompt (the own prompt IS the whole base there —
    no fallback consulted), a NULL-namespace lane, and the off-by-one
    boundary (399 fires, 400 does not);
  * the conforming fleet exits 0: an EMPTY own prompt over a large
    namespace default (the qa/pm/dogfood shape, 2126-3372 chars), the
    docs/readme shape (1060-1230 chars of own prompt over an EMPTY
    namespace default), a replace lane at the threshold, a DISABLED
    incident-shape lane;
  * the schema-predates-columns skip is pinned: a fixture DB without
    prompt/default_prompt columns produces the documented skip INFO line,
    never a violation (same shape as board_ownership parity) — a bare
    checkout's battery must stay green.

The fixture DB carries the full namespace roster (coding-hermes + the six
satellites at their SCHED-GAP-215 caps, with namespace defaults mirroring
the measured live values) so the caps/admission checks stay quiet, and
every enabled lane gets a real workdir so check 5 stays quiet: any extra
``VIOLATION`` line means the fixture (not the gate) regressed.

Run:  python3 -m pytest tests/test_check_fleet_invariants_lane_instructions.py -v
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


def _fixture_cooldown(name: str) -> int:
    """The CANONICAL family cadence for a satellite-shaped fixture lane, else
    the 6h default. The family-floor class (check 5e, SCHED-GAP-1675) polices
    ALL NINE families, so a `*-releng` fixture lane seeded at the old 43200
    default would flag the fixture instead of the class under test."""
    m = gate.SATELLITE_FAMILY_RE.match(name)
    return gate.SATELLITE_FAMILY_PINS[m.group(2)] if m else 43200

SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
# The measured live namespace defaults (2026-09-27): the healthy fallbacks the
# conjunction must never punish, reproduced at real size so a threshold drift
# breaks the FIXTURE (asserted below), not the live fleet.
MEASURED_NS_DEFAULTS = {
    "qa": 2646,
    "pm": 3372,
    "dogfood": 2126,
    "duckbrain-sync": 1997,
    "releases": 1246,
    "doc-writer": 1736,
}
# Healthy own-prompt floor (docs/readme lanes: 1060-1230 chars over an empty
# namespace default) and the healthy fallback floor (python-audit 777) — the
# incident's 296 chars must sit BELOW both floors, by more than a rounding
# error, for 400 to be the honest threshold.
HEALTHY_OWN_FLOOR = 1060
HEALTHY_FALLBACK_FLOOR = 777
AUGER_RELENG_INCIDENT_CHARS = 296


def _violation_line(lane: str, ns: str | None, own: int, fallback: int) -> str:
    """The exact VIOLATION line the checker must emit for these numbers."""
    shown = ns if ns is not None else "<none>"
    return (f"VIOLATION lane-instructions {lane}: no usable instructions — "
            f"own prompt {own} chars, namespace {shown!r} default_prompt {fallback} chars "
            f"(both under {gate.PROMPT_SHORT_CHARS}; the lane would dispatch on the "
            f"generic built-in prompt and improvise, cf. auger 2026-09-27)")


def _seed_db(path: Path, lanes: list[dict], ns_defaults: dict[str, int] | None = None) -> Path:
    """Mint a scheduler DB with the MODERN schema (prompt columns present).

    *lanes* are dicts: name, namespace_id, prompt, prompt_mode (optional),
    enabled (default 1), cooldown_s (default 43200 — above the 6h floor).
    Every lane gets a real workdir directory so the workdirs check stays
    quiet. The namespace roster is complete (coding-hermes + the six
    satellites at their policy caps) so caps/admission stay quiet; namespace
    defaults mirror MEASURED_NS_DEFAULTS unless overridden per id.
    """
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER,"
                " cooldown_s INTEGER, command TEXT, prompt TEXT, prompt_mode TEXT,"
                " workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER,"
                " admission_mode TEXT, default_prompt TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks', '')")
    defaults = dict(MEASURED_NS_DEFAULTS)
    defaults.update(ns_defaults or {})
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown', ?)",
                    (ns, gate.SATELLITE_CAP_POLICY[ns], "x" * defaults[ns]))
    for ns, chars in defaults.items():
        if ns not in SATELLITE_NS and ns != "coding-hermes":
            con.execute("INSERT INTO namespaces VALUES (?, 0, 'cooldown', ?)",
                        (ns, "x" * chars))
    for ln in lanes:
        wd = path.parent / ln["name"]
        wd.mkdir(parents=True, exist_ok=True)
        con.execute("INSERT INTO projects VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    (ln["name"], ln.get("enabled", 1),
                     ln.get("cooldown_s", _fixture_cooldown(ln["name"])),
                     "", ln.get("prompt", ""), ln.get("prompt_mode", "append"),
                     str(wd), ln.get("namespace_id", "auger")))
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, lanes: list[dict],
              ns_defaults: dict[str, int] | None = None) -> tuple[int, str]:
    tmp_path.mkdir(parents=True, exist_ok=True)
    db = _seed_db(tmp_path / "scheduler.db", lanes, ns_defaults=ns_defaults)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines()
            if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


# ── constants pin ─────────────────────────────────────────────────────────

def test_constants_are_the_documented_shape():
    """The class is registered in CHECK_CLASSES (the daily report builds its
    summary from that tuple), the threshold is 400, and 400 sits ABOVE the
    296-char auger-releng incident state while staying BELOW both healthy
    floors (own 1060, fallback 777) — the numbers the no-false-alarm arm
    depends on. A threshold drift breaks this fixture, not the live fleet."""
    assert gate.LANE_INSTRUCTIONS_CLASS == "lane-instructions"
    assert gate.PROMPT_SHORT_CHARS == 400, gate.PROMPT_SHORT_CHARS
    assert "lane-instructions" in gate.CHECK_CLASSES, gate.CHECK_CLASSES
    assert AUGER_RELENG_INCIDENT_CHARS < gate.PROMPT_SHORT_CHARS, (
        f"threshold {gate.PROMPT_SHORT_CHARS} no longer reproduces the "
        f"{AUGER_RELENG_INCIDENT_CHARS}-char incident shape")
    assert gate.PROMPT_SHORT_CHARS < HEALTHY_OWN_FLOOR, (
        f"threshold {gate.PROMPT_SHORT_CHARS} would flag healthy docs/readme lanes")
    assert gate.PROMPT_SHORT_CHARS < HEALTHY_FALLBACK_FLOOR, (
        f"threshold {gate.PROMPT_SHORT_CHARS} would flag healthy fallbacks")
    for ns, chars in MEASURED_NS_DEFAULTS.items():
        assert chars >= HEALTHY_OWN_FLOOR or ns == "doc-writer", (ns, chars)


# ── seeded violations must fire ───────────────────────────────────────────

def test_auger_shape_empty_own_over_empty_namespace_fires(tmp_path):
    """The measured incident: an enabled foreman with NO own prompt over a
    namespace whose default_prompt is ALSO empty. Exit 1 naming the lane,
    and no other class fires on the fixture."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-foreman", "namespace_id": "auger",
         "prompt": "", "prompt_mode": "append"},
    ], ns_defaults={"auger": 0})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "lane-instructions")
    assert lines == [_violation_line("fixture-foreman", "auger", 0, 0)], lines
    assert "fixture-foreman" in lines[0], lines
    assert _other_violations(out, "lane-instructions") == [], \
        f"unexpected extra violations:\n{out}"
    assert "INFO lane-instructions prompts: 1 enabled lane(s) scanned, " \
           "1 lacking instructions" in out, out


def test_auger_releng_shape_short_own_no_fallback_fires(tmp_path):
    """The second measured incident lane: 296 chars of own prompt with no
    namespace fallback — short on its own, exactly what the sweep judged
    uninstructed."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-releng", "namespace_id": "auger",
         "prompt": "x" * AUGER_RELENG_INCIDENT_CHARS},
    ], ns_defaults={"auger": 0})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    lines = _violations(out, "lane-instructions")
    assert lines == [_violation_line("fixture-releng", "auger",
                                     AUGER_RELENG_INCIDENT_CHARS, 0)], lines
    assert _other_violations(out, "lane-instructions") == [], \
        f"unexpected extra violations:\n{out}"


def test_both_sides_just_under_threshold_fires(tmp_path):
    """399 own over a 399 fallback: neither side alone looks damning, but the
    conjunction is the defect — this is the arm that proves the checker reads
    the CONJUNCTION, not one layer."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-both-short", "namespace_id": "qa",
         "prompt": "x" * (gate.PROMPT_SHORT_CHARS - 1)},
    ], ns_defaults={"qa": gate.PROMPT_SHORT_CHARS - 1})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "lane-instructions") == [
        _violation_line("fixture-both-short", "qa",
                        gate.PROMPT_SHORT_CHARS - 1, gate.PROMPT_SHORT_CHARS - 1)], out


def test_short_replace_prompt_fires_alone(tmp_path):
    """prompt_mode=replace: the own prompt REPLACES the base, so the namespace
    fallback is never consulted — a short replace prompt is the incident shape
    even over a huge default. The arm that proves the mode is read."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-replace-short", "namespace_id": "qa",
         "prompt": "x" * 100, "prompt_mode": "replace"},
    ], ns_defaults={"qa": 3372})

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "lane-instructions") == [
        _violation_line("fixture-replace-short", "qa", 100, 3372)], out


def test_null_namespace_lane_with_no_own_prompt_fires(tmp_path):
    """A lane with NO namespace row at all has no fallback by construction —
    the detail names '<none>' instead of a namespace id."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-orphan", "namespace_id": None, "prompt": ""},
    ])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out, "lane-instructions") == [
        _violation_line("fixture-orphan", None, 0, 0)], out


# ── the conforming shapes must NOT fire ───────────────────────────────────

def test_empty_own_over_large_namespace_default_is_clean(tmp_path):
    """THE false-alarm arm: the qa/pm/dogfood shape — 38 healthy lanes carry
    NO own prompt and inherit a large namespace default. One-sided length
    tests flag all of them; the conjunction must not. Exit 0, and the INFO
    line reports the lane as counted-and-clean."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-healthy-sat", "namespace_id": "qa", "prompt": ""},
    ], ns_defaults={"qa": MEASURED_NS_DEFAULTS["qa"]})

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    assert "INFO lane-instructions prompts: 1 enabled lane(s) scanned, " \
           "0 lacking instructions" in out, out


def test_docs_shape_own_prompt_over_empty_namespace_is_clean(tmp_path):
    """The docs/readme shape: 1060-1230 chars of OWN prompt over an EMPTY
    namespace default is a fallback GAP, not an instructions gap — the
    namespace side alone must never fire."""
    for chars in (HEALTHY_OWN_FLOOR, 1230):
        rc, out = _run_gate(tmp_path / f"run-{chars}", [
            {"name": f"fixture-docs-{chars}", "namespace_id": "docs",
             "prompt": "x" * chars},
        ], ns_defaults={"docs": 0})

        assert rc == 0, f"{chars} chars: gate exited {rc}, expected 0 (stdout:\n{out})"
        assert "VIOLATION" not in out, out


def test_boundary_at_threshold_is_clean_both_sides(tmp_path):
    """Exactly 400 own over an exactly-400 fallback: the rule is
    SHORT-ER-THAN (own < 400 AND fallback < 400), so the boundary value is
    conforming on each side."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-boundary", "namespace_id": "perf",
         "prompt": "x" * gate.PROMPT_SHORT_CHARS},
    ], ns_defaults={"perf": gate.PROMPT_SHORT_CHARS})

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_replace_lane_at_threshold_is_clean(tmp_path):
    """A replace lane whose own prompt reaches the threshold carries a whole
    instruction body by itself — conforming regardless of the fallback."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-replace-ok", "namespace_id": "auger",
         "prompt": "x" * gate.PROMPT_SHORT_CHARS, "prompt_mode": "replace"},
    ], ns_defaults={"auger": 0})

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


def test_disabled_incident_lane_is_exempt(tmp_path):
    """The exact auger incident shape, but enabled=0: disabled lanes are not
    dispatched, so they cannot improvise. Exempt, like every project-level
    check in this file."""
    rc, out = _run_gate(tmp_path, [
        {"name": "fixture-disabled", "namespace_id": "auger",
         "prompt": "", "enabled": 0},
    ], ns_defaults={"auger": 0})

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out


# ── the documented schema skip ────────────────────────────────────────────

def test_schema_without_prompt_columns_skips_with_info(tmp_path):
    """A fixture DB on the OLD schema (no projects.prompt, no
    namespaces.default_prompt — the shape every pre-existing fixture module
    mints) must produce the documented skip INFO line and NEVER a
    violation: the battery on a bare checkout stays green, and the skip
    cannot masquerade as a passing assertion."""
    db = tmp_path / "scheduler.db"
    con = sqlite3.connect(db)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER,"
                " cooldown_s INTEGER, command TEXT, prompt TEXT, workdir TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER,"
                " admission_mode TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY[ns]))
    wd = tmp_path / "fixture-old-schema"
    wd.mkdir(parents=True, exist_ok=True)
    con.execute("INSERT INTO projects VALUES (?, ?, ?, ?, ?, ?)",
                ("fixture-old-schema", 1, 43200, "", "", str(wd)))
    con.commit()
    con.close()

    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    out = buf.getvalue()

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION lane-instructions" not in out, out
    assert ("INFO lane-instructions schema: skipped — projects.prompt / "
            "namespaces.default_prompt absent from this schema") in out, out


# ── board-only mode never runs the class ──────────────────────────────────

def test_board_only_mode_never_reads_the_db(tmp_path):
    """The CI shape: --board-only never opens the DB, so the class cannot run
    there even in principle — pinned by passing a DB path that does not
    exist and getting a clean exit anyway."""
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(tmp_path / "definitely-not-here.db"),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
            "--board-only",
        ])
    out = buf.getvalue()

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "lane-instructions" not in out, out
    assert "PASS" in out, out
