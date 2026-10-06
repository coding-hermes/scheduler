"""SCHED-GAP-1675 — the canonical satellite cadence matrix (check 5e) as a gate.

Bane's 2026-09-29 ruling gives every satellite family ONE canonical time —
qa/sync 21600, pm/releng 86400, dogfood 259200, perf/review/readme/docs 604800
— and sanctions a SECOND value for -releng (259200). This fixture battery drives
check 5e (class ``family-floor``) plus the two constants the same alignment
touched (class ``cooldown``):

  * the matrix is the nine-suffix one, and the name matcher is DERIVED from it —
    the old gate matched only ``(qa|pm|dogfood|sync)``, so the other five
    families could drift unobserved. That blind spot is the row.
  * every one of the nine families PASSES at its canonical value and FIRES when
    its ``cooldown_s`` drifts, including the five the old regex never looked at
    (releng/perf/review/readme/docs — previously unchecked);
  * -releng passes at BOTH sanctioned values (86400 or 259200) and fires off
    both;
  * a legacy off-family ``cooldown_floor_s`` is TOLERATED — satellites never arm
    adaptive cooldown and fleet-cooldown-policy.py aligns a family lane before
    its REDUCE rules, so the floor cannot change the cadence — and REPORTED as
    INFO rather than silently dropped, while a floor BELOW the 6h law still
    fires (the one floor value that could ever speed a satellite up);
  * ``release-engineer`` sits at its newly aligned 86400, accepts the releng
    role's 259200, and FIRES on the stale 604800 the old tier documented — the
    bogus violation the alignment created;
  * ``hermes-dagger`` (900) and ``coding-hermes-tools`` (3600) are Bane's
    sanctioned sub-floor operator pins, not violations — while a drift off
    either sanctioned value still fires (600 is a different lane, not a pin);
  * the gate's matrix EQUALS ``FAMILY_CANONICAL`` / ``FAMILY_ALSO_ALLOWED`` in
    the policy writer (``~/.hermes/scripts/fleet-cooldown-policy.py``, outside
    this repo — the source of truth the alignment wrote). The test SKIPS when
    that user-level file is absent, the same honest shape the SCHED-GAP-178
    mirror test uses.

The fixture is a minimal SQLite DB seeded per case; ``--toml`` / ``--board``
point at absent paths (the documented skip for checks 7/8) and ``--skills-root``
at an absent directory (check 10's documented skip), so every other class is
clean by construction and any extra ``VIOLATION`` line fails loudly.

Run:  python3 -m pytest tests/test_check_fleet_invariants_family_floor.py -v
"""
from __future__ import annotations

import importlib.util
import io
import itertools
import re
import sqlite3
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"

SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"

# The canonical matrix, written out HERE as an independent expectation instead
# of read from the gate: a fixture that imports the constant it is testing
# cannot detect that constant being wrong.
CANONICAL = {
    "qa": 43200, "pm": 86400, "sync": 21600, "dogfood": 259200,
    "releng": 86400, "perf": 604800, "review": 604800,
    "readme": 604800, "docs": 604800,
}
RELENG_ALSO_ALLOWED = 259200
SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")
NS_FOR_SUFFIX = {s: ("duckbrain-sync" if s == "sync" else s) for s in CANONICAL}

_CASES = itertools.count()


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


def _run(base: Path, rows: list[dict]) -> tuple[int, str]:
    """Seed *rows* into a fresh DB under *base* and run the checker on it."""
    case = base / f"case{next(_CASES)}"
    case.mkdir(parents=True, exist_ok=True)
    db = case / "scheduler.db"
    con = sqlite3.connect(db)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 8, 'tasks')")
    for ns in SATELLITE_NS:
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))
    for r in rows:
        wd = case / r["name"]
        (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
        if r["name"].endswith("-sync"):
            # check 10: an enabled *-sync lane needs both orientation facts or
            # the FIXTURE (not the class under test) produces a violation.
            base_name = r["name"][:-len("-sync")]
            (wd / "README.md").write_text(
                f"# {r['name']} — lane workdir\n\nTarget namespace: `{base_name}`.\n"
                f"Contract: skill `{base_name}-sync-data`.\n", encoding="utf-8")
        con.execute("INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command,"
                    " prompt, workdir, namespace_id) VALUES (?, ?, ?, ?, ?, '', ?, ?)",
                    (r["name"], r.get("enabled", 1), r["cooldown_s"], r["cooldown_floor_s"],
                     r.get("command", SUPPORTED_COMMAND), str(wd), r.get("namespace_id", "fixture-ns")))
    con.commit()
    con.close()

    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(case / "no-fleet.toml"),
            "--board", str(case / "no-board.jsonl"),
            "--skills-root", str(case / "no-skills-root"),
        ])
    return rc, buf.getvalue()


def _violations(out: str, cls: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith(f"VIOLATION {cls} ")]


def _other_violations(out: str, *classes: str) -> list[str]:
    prefixes = tuple(f"VIOLATION {c} " for c in classes)
    return [l for l in out.splitlines()
            if l.startswith("VIOLATION ") and not l.startswith(prefixes)]


def _primary(name: str = "fixture-primary") -> dict:
    """A primary OUTSIDE the coding-hermes namespace: the coverage class (5d)
    is a different row's subject, so it stays out of scope here."""
    return {"name": name, "cooldown_s": 43200, "cooldown_floor_s": 43200,
            "namespace_id": "fixture-ns"}


def _sat(suffix: str, cd: int | None = None, fl: int | None = None,
         primary: str = "fixture-primary") -> dict:
    """A satellite of *suffix* on its canonical values unless overridden."""
    return {"name": f"{primary}-{suffix}",
            "cooldown_s": CANONICAL[suffix] if cd is None else cd,
            "cooldown_floor_s": CANONICAL[suffix] if fl is None else fl,
            "namespace_id": NS_FOR_SUFFIX[suffix]}


# ── the matrix and its matcher ────────────────────────────────────────────

def test_family_pins_are_the_nine_suffix_canonical_matrix():
    """The constant is the canonical matrix (SCHED-GAP-1675, 2026-09-29) — all
    nine suffixes, not the old four (qa/sync 43200 was the stale table)."""
    assert gate.SATELLITE_FAMILY_PINS == CANONICAL, gate.SATELLITE_FAMILY_PINS
    assert gate.SATELLITE_FAMILY_ALSO_ALLOWED == {"releng": (RELENG_ALSO_ALLOWED,)}, \
        gate.SATELLITE_FAMILY_ALSO_ALLOWED


def test_family_re_matches_every_suffix_and_no_primary():
    """The name matcher is DERIVED from the pin table, so a family can never be
    pinned and left unpoliceable (the old regex's exact defect)."""
    for suffix in CANONICAL:
        m = gate.SATELLITE_FAMILY_RE.match(f"fixture-primary-{suffix}")
        assert m is not None, f"suffix {suffix!r} is not matched by SATELLITE_FAMILY_RE"
        assert (m.group(1), m.group(2)) == ("fixture-primary", suffix), m.groups()
    for not_a_satellite in ("fixture-primary", "release-engineer", "hermes-dagger",
                            "coding-hermes-tools", "qa-audit"):
        assert gate.SATELLITE_FAMILY_RE.match(not_a_satellite) is None, not_a_satellite


# ── canonical value passes / drift fires, for EVERY family ────────────────

def test_every_family_passes_at_its_canonical_value(tmp_path):
    failures = []
    for suffix in sorted(CANONICAL):
        rc, out = _run(tmp_path, [_primary(), _sat(suffix)])
        if rc != 0 or "VIOLATION" in out:
            failures.append((suffix, rc, out))
    assert failures == [], failures


def test_every_family_fires_when_cooldown_s_drifts(tmp_path):
    """Drift per family: each family's stale pre-ruling value (qa 21600 —
    the 6h the 2026-10-02 ruling replaced; sync 43200) and the 6h default for
    the rest."""
    drift = {"qa": 21600, "sync": 43200, "pm": 21600, "releng": 21600,
             "dogfood": 21600, "perf": 21600, "review": 21600,
             "readme": 21600, "docs": 21600}
    failures = []
    for suffix in sorted(CANONICAL):
        rc, out = _run(tmp_path, [_primary(), _sat(suffix, cd=drift[suffix])])
        got = _violations(out, "family-floor")
        allowed = (CANONICAL[suffix],) + ((RELENG_ALSO_ALLOWED,) if suffix == "releng" else ())
        expects_text = " or ".join(str(v) for v in allowed)
        ok = (rc == 1 and len(got) == 1
              and got[0].startswith(f"VIOLATION family-floor fixture-primary-{suffix}: ")
              and f"cooldown_s={drift[suffix]} " in got[0]
              and f"expects {expects_text} for {suffix} " in got[0]
              and "SATELLITE_FAMILY_PINS" in got[0])
        others = _other_violations(out, "family-floor")
        if not ok or others:
            failures.append((suffix, rc, got, others))
    assert failures == [], failures


def test_releng_passes_at_both_sanctioned_values(tmp_path):
    for value in (CANONICAL["releng"], RELENG_ALSO_ALLOWED):
        rc, out = _run(tmp_path, [_primary(), _sat("releng", cd=value)])
        assert rc == 0, f"releng at {value}: gate exited {rc} (stdout:\n{out})"
        assert "VIOLATION" not in out, out


def test_releng_off_both_sanctioned_values_fires(tmp_path):
    for value in (43200, 604800):
        rc, out = _run(tmp_path, [_primary(), _sat("releng", cd=value)])
        got = _violations(out, "family-floor")
        assert rc == 1 and len(got) == 1, (value, rc, got, out)
        assert f"cooldown_s={value} " in got[0], got[0]
        assert "expects 86400 or 259200 for releng" in got[0], got[0]
        assert _other_violations(out, "family-floor") == [], out


# ── cooldown_floor_s: tolerated legacy, reported, sub-6h still fires ──────

def test_legacy_off_family_floor_is_tolerated_and_reported(tmp_path):
    """The alignment wrote cooldown_s only, so live satellites carry a legacy
    floor (measured 2026-09-29: 81 enabled lanes). It is INERT — satellites
    never arm adaptive cooldown (check 5b) and the policy's canonical-
    alignment branch precedes its REDUCE rules — so it must not fail the gate,
    but it must not vanish either: the count rides an INFO line."""
    rows = [_primary(),
            _sat("pm", fl=21600),      # the fleet's legacy 6h floor on a daily lane
            _sat("qa", fl=86400)]      # a floor ABOVE the lane's own cadence
    rc, out = _run(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    info = [l for l in out.splitlines() if l.startswith("INFO cooldown-legacy-floor ")]
    assert len(info) == 1, out
    assert "2 enabled satellite lane(s)" in info[0], info[0]
    # The INFO class must not borrow the violated class's name: a reader (or a
    # grep) counting "family-floor" must never see a tolerated residue.
    assert "family-floor" not in info[0], info[0]


def test_satellite_sub_floor_cooldown_floor_still_fires(tmp_path):
    """The one floor value that could ever speed a satellite up: below the 6h
    law. Its cadence is on-pin, so the violation must name the FLOOR."""
    rc, out = _run(tmp_path, [_primary(), _sat("qa", fl=1800)])

    got = _violations(out, "family-floor")
    assert rc == 1 and len(got) == 1, (rc, got, out)
    assert "cooldown_floor_s=1800" in got[0], got[0]
    assert "below the 21600s (6h) law" in got[0], got[0]
    assert "cooldown_s=" not in got[0], got[0]


def test_floor_aligned_with_the_pin_is_not_reported(tmp_path):
    """A floor ON the family pin is not legacy residue — no INFO line."""
    rc, out = _run(tmp_path, [_primary(), _sat("dogfood"), _sat("perf")])

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "INFO cooldown-legacy-floor" not in out, out


# ── the named tiers (class ``cooldown``) ──────────────────────────────────

def _lane(name: str, cd: int, fl: int | None = None) -> dict:
    return {"name": name, "cooldown_s": cd, "cooldown_floor_s": cd if fl is None else fl,
            "namespace_id": "fixture-ns"}


def test_release_engineer_daily_tier_aligned(tmp_path):
    """SCHED-GAP-1675 re-aligned this primary lane 21600 -> 86400 (it has no
    -releng suffix, so the family view never saw it and it swept 4x/day). The
    tier must accept 86400 AND the releng role's 259200, and must NOT accept
    the retired 604800 or the 6h default the alignment moved it off."""
    for value, fires in ((86400, False), (259200, False), (604800, True), (21600, True)):
        rc, out = _run(tmp_path, [_lane("release-engineer", value)])
        got = _violations(out, "cooldown")
        if fires:
            assert rc == 1 and len(got) == 1, (value, rc, got, out)
            assert "!= documented tier 86400 / 259200" in got[0], got[0]
        else:
            assert rc == 0 and "VIOLATION" not in out, (value, rc, out)


def test_qa_audit_tier_unchanged(tmp_path):
    for value, fires in ((86400, False), (604800, True)):
        rc, out = _run(tmp_path, [_lane("qa-audit", value)])
        got = _violations(out, "cooldown")
        if fires:
            assert rc == 1 and len(got) == 1, (value, rc, got, out)
            assert "!= documented tier 86400" in got[0], got[0]
        else:
            assert rc == 0 and "VIOLATION" not in out, (value, rc, out)


# ── sanctioned sub-floor operator pins ────────────────────────────────────

def test_sanctioned_subfloor_pins_are_not_violations(tmp_path):
    """hermes-dagger 900 (Bane 2026-09-15 dagger speed ruling) and
    coding-hermes-tools 3600 (fast tier) are operator decisions living in
    fleet-cooldown-policy.py — the 6h law must not flag them."""
    assert gate.SANCTIONED_SUBFLOOR_PINS == {"hermes-dagger": 900, "coding-hermes-tools": 3600}, \
        gate.SANCTIONED_SUBFLOOR_PINS
    rows = [_lane("hermes-dagger", 900, fl=21600), _lane("coding-hermes-tools", 3600)]
    rc, out = _run(tmp_path, rows)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert "VIOLATION" not in out, out
    for name in ("hermes-dagger", "coding-hermes-tools"):
        assert re.search(rf"VIOLATION cooldown {re.escape(name)}:", out) is None, out


def test_sanctioned_pin_drift_still_fires(tmp_path):
    """A sanctioned pin is a VALUE, not an exemption: 900 that quietly becomes
    600 is a different lane."""
    for name, drifted, sanctioned in (("hermes-dagger", 600, 900),
                                      ("coding-hermes-tools", 1800, 3600)):
        rc, out = _run(tmp_path, [_lane(name, drifted)])
        got = _violations(out, "cooldown")
        assert rc == 1 and len(got) == 1, (name, rc, got, out)
        assert f"cooldown_s={drifted} below the 21600s (6h) floor" in got[0], got[0]
        assert f"must sit at {sanctioned}" in got[0], got[0]


# ── the cross-store invariant: the gate mirrors the policy writer ─────────

def test_gate_matrix_equals_policy_family_canonical():
    """MUST stay equal to FAMILY_CANONICAL (#releng: FAMILY_ALSO_ALLOWED) in
    ``~/.hermes/scripts/fleet-cooldown-policy.py`` — the policy writer that
    OWNS the alignment and lives outside this repo. SKIPS when that user-level
    file is absent (a CI runner, a fork), the same honest shape the
    SCHED-GAP-178 mirror test uses, so a real drift in a steady-state policy
    script is the only failure this test can produce."""
    import pytest
    policy = Path.home() / ".hermes" / "scripts" / "fleet-cooldown-policy.py"
    if not policy.is_file():
        pytest.skip(f"policy writer not present at {policy} (user-level file)")
    text = policy.read_text(encoding="utf-8", errors="replace")
    if "FAMILY_CANONICAL" not in text:
        pytest.skip("policy writer carries no FAMILY_CANONICAL block (pre-2026-09-29 state)")

    block = re.search(r"(?s)FAMILY_CANONICAL\s*=\s*\{(.*?)\}", text)
    assert block is not None, "FAMILY_CANONICAL block does not parse"
    # The writer spells the keys dashed ('-qa'); the gate spells them bare.
    pairs = dict(re.findall(r"'(-[a-z]+)'\s*:\s*(\d+)", block.group(1)))
    got = {k.lstrip("-"): int(v) for k, v in pairs.items()}
    assert got == CANONICAL, f"policy FAMILY_CANONICAL={got} vs gate {CANONICAL}"

    also = re.search(r"(?s)FAMILY_ALSO_ALLOWED\s*=\s*\{(.*?)\}", text)
    assert also is not None, "FAMILY_ALSO_ALLOWED block does not parse"
    assert re.search(rf"'-releng'\s*:\s*\{{\s*{RELENG_ALSO_ALLOWED}\b", also.group(1)), also.group(1)
