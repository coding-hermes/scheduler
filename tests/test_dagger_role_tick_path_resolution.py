"""INT-PATH-001 regression tests: the dagger role wrapper must be path-agnostic.

``ops/dagger-role-tick.sh`` is the canonical copy of the manual role-tick
wrapper (deployed as a byte copy to ``~/.hermes/scripts/dagger-role-tick.sh``;
a RETIRED scheduler driver per SCHED-GAP-150, kept for manual role runs).
The defect this row fixes: the wrapper hardcoded ``/home/kara/hermes-dagger``
as the default DAGGER_BIN/DAGGER_HOME, so its ``[ -x "$DAGGER_BIN" ] || exit 2``
preflight worked only while that host symlink existed. These tests fail if any
wrapper copy reintroduces an absolute legacy path, and prove that an
env-provided DAGGER_HOME wins over the documented $HOME-based default.

Run:  python3 -m pytest tests/test_dagger_role_tick_path_resolution.py -v
"""
from __future__ import annotations

import filecmp
import re
import shutil
import subprocess
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]
CANONICAL = REPO_ROOT / "ops" / "dagger-role-tick.sh"
DEPLOYED = Path.home() / ".hermes" / "scripts" / "dagger-role-tick.sh"

LEGACY_PATH = "/home/kara/hermes-dagger"
ABS_ASSIGN_RE = re.compile(r"^\s*DAGGER_(BIN|HOME)=.*[\"']?/home/[A-Za-z0-9._-]+/")


def _assert_path_agnostic(text: str) -> None:
    assert LEGACY_PATH not in text, "legacy absolute dagger path reintroduced"
    offenders = [ln for ln in text.splitlines() if ABS_ASSIGN_RE.match(ln)]
    assert not offenders, (
        f"DAGGER_BIN/DAGGER_HOME carry an absolute /home default: {offenders}"
    )


def test_canonical_copy_has_no_absolute_legacy_path():
    assert CANONICAL.is_file(), f"missing canonical wrapper copy: {CANONICAL}"
    _assert_path_agnostic(CANONICAL.read_text(encoding="utf-8"))


def test_env_override_wins(tmp_path):
    if shutil.which("bash") is None:
        pytest.skip("bash unavailable on this host")
    fake_home = tmp_path / "nonexistent"
    result = subprocess.run(
        [
            "env", "-u", "GATEWAY_API_KEY",
            f"DAGGER_HOME={fake_home}",
            "bash", str(CANONICAL), "dogfooding",
        ],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert result.returncode == 2, (
        f"expected preflight exit 2, got {result.returncode}; stderr: {result.stderr}"
    )
    assert f"{fake_home}/dagger" in result.stderr, (
        f"preflight did not consult the env-provided DAGGER_HOME: {result.stderr}"
    )
    assert LEGACY_PATH not in result.stderr, (
        f"wrapper still consults the legacy absolute path: {result.stderr}"
    )


def test_deployed_copy_is_path_agnostic():
    if not DEPLOYED.is_file():
        pytest.skip("live wrapper not deployed on this host")
    _assert_path_agnostic(DEPLOYED.read_text(encoding="utf-8"))
    assert filecmp.cmp(CANONICAL, DEPLOYED, shallow=False), (
        f"deployed copy drifted from canonical {CANONICAL}"
    )
