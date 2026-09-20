"""RELEASE-006 regression fixtures for the version-authority gate.

Drives ``scripts/check-version-authority.sh`` end-to-end (subprocess — the
shell script IS the unit under test) against fixture repositories built in
``tmp_path``, pinning both arms of the gate:

  * tag mode (release.yaml): a CHANGELOG that names ``vX.Y.Z`` exits 0; one
    that still says ``[Unreleased]`` (the defect class this gate exists for)
    exits 2 with ``REASON=CHANGELOG_DOES_NOT_NAME_VERSION``;
  * structure mode (ci.yaml): exactly-one-``[Unreleased]`` + newest release
    heading parsing as ``## [X.Y.Z]`` exits 0; each seeded violation exits 2
    with its named reason.

The gate delegates the tag-mode rule to ``scripts/release-prep.sh --check``
via a throwaway fixture git repo, so the battery also pins that the wrapper
is immune to the operator-checkout refusal classes the brief warned about:
DIRTY_TREE (a dirty TARGET tree must not matter — the assertion never runs
against the target's git state) and TAG_ALREADY_EXISTS (a target that already
carries the tag is exactly the CI shape).

Environment-free by construction and pinned so it stays that way: every
invocation runs with ``HOME`` pointed at a temp directory — no ~/.hermes
state, no network, no database.

Run:  python3 -m pytest tests/test_check_version_authority.py -v
"""
from __future__ import annotations

import os
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / "scripts" / "check-version-authority.sh"

CONFORMING_CHANGELOG = """\
# Changelog

All notable changes.

## [Unreleased]

Nothing yet.

## [1.4.0] — 2026-09-20

- First entry for 1.4.0.

## [1.3.0] — 2026-09-16

- Older entry.
"""


def _write_changelog(target: Path, body: str) -> Path:
    changelog = target / "CHANGELOG.md"
    changelog.write_text(body, encoding="utf-8")
    return changelog


def _make_target_repo(tmp_path: Path, name: str, changelog_body: str) -> Path:
    """A plain directory with a CHANGELOG.md — good enough, because the
    tag-mode assertion runs on the wrapper's own throwaway fixture repo and
    only reads the target's CHANGELOG.md."""
    target = tmp_path / name
    target.mkdir()
    _write_changelog(target, changelog_body)
    return target


def _run(*args: str, cwd: Path | None = None) -> tuple[int, str, str]:
    """Run the gate with HOME pointed at a temp dir (the environment-free
    pin: the gate must not depend on any ~/.hermes state)."""
    scratch = subprocess.run(
        ["mktemp", "-d"], capture_output=True, text=True, check=True
    ).stdout.strip()
    env = dict(os.environ)
    env["HOME"] = scratch
    proc = subprocess.run(
        ["bash", str(SCRIPT), *args],
        capture_output=True,
        text=True,
        env=env,
        cwd=str(cwd) if cwd else None,
        timeout=120,
    )
    return proc.returncode, proc.stdout, proc.stderr


# ---------------------------------------------------------------- tag mode --


def test_tag_mode_conforming_fixture_passes(tmp_path):
    """A CHANGELOG that names the version exits 0 with a PASS line."""
    target = _make_target_repo(tmp_path, "conforming", CONFORMING_CHANGELOG)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "PASS" in out, out


def test_tag_mode_unnamed_version_fails_with_named_reason(tmp_path):
    """The defect class: a tag pushed over a tree whose CHANGELOG still says
    [Unreleased] exits 2 naming CHANGELOG_DOES_NOT_NAME_VERSION."""
    unnamed = CONFORMING_CHANGELOG.replace("## [1.4.0] — 2026-09-20\n\n- First entry for 1.4.0.\n\n", "")
    target = _make_target_repo(tmp_path, "unnamed", unnamed)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=CHANGELOG_DOES_NOT_NAME_VERSION" in err, err
    assert out.strip() == "", out  # no PASS line on a refusal


def test_tag_mode_missing_unreleased_fails_with_named_reason(tmp_path):
    """No [Unreleased] heading at all -> CHANGELOG_UNRELEASED_INVALID."""
    no_unreleased = "\n".join(
        line for line in CONFORMING_CHANGELOG.splitlines() if "[Unreleased]" not in line
    )
    target = _make_target_repo(tmp_path, "no-unreleased", no_unreleased)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=CHANGELOG_UNRELEASED_INVALID" in err, err


def test_tag_mode_duplicate_unreleased_fails_with_named_reason(tmp_path):
    """Two [Unreleased] headings -> CHANGELOG_UNRELEASED_INVALID (release-prep
    requires exactly one)."""
    duplicated = CONFORMING_CHANGELOG.replace(
        "## [1.4.0] — 2026-09-20",
        "## [Unreleased] — 2026-09-21\n\n## [1.4.0] — 2026-09-20",
    )
    target = _make_target_repo(tmp_path, "dup-unreleased", duplicated)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=CHANGELOG_UNRELEASED_INVALID" in err, err


def test_tag_mode_empty_unreleased_passes(tmp_path):
    """The healthy post-rollover shape: [Unreleased] present but empty, the
    version heading above nothing yet. release-prep's EMPTY_UNRELEASED guards
    its WRITE path only; the gate must not false-fail this tree."""
    rolled_over = CONFORMING_CHANGELOG.replace(
        "## [Unreleased]\n\nNothing yet.\n",
        "## [Unreleased]\n\n## [1.4.0] — 2026-09-20\n\n- First entry for 1.4.0.\n",
    )
    assert "Nothing yet" not in rolled_over  # premise
    target = _make_target_repo(tmp_path, "rolled-over", rolled_over)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "PASS" in out, out


def test_tag_mode_bad_version_shape_refused(tmp_path):
    """Non-vX.Y.Z input refuses with BAD_VERSION_SHAPE before any assertion."""
    target = _make_target_repo(tmp_path, "shape", CONFORMING_CHANGELOG)

    rc, out, err = _run("--repo", str(target), "v1.4.0-rc1")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=BAD_VERSION_SHAPE" in err, err


def test_tag_mode_dirty_target_tree_does_not_matter(tmp_path):
    """DIRTY_TREE immunity pin: the operator-checkout precondition must not
    leak through. The TARGET here is deliberately a dirty git repo (modified
    CHANGELOG + untracked file); the wrapper's assertion runs on its own
    fixture copy, so the gate still passes."""
    import subprocess as sp

    target = _make_target_repo(tmp_path, "dirty-target", CONFORMING_CHANGELOG)
    env = dict(os.environ)
    env.update({
        "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
        "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
    })
    sp.run(["git", "init", "-q"], cwd=target, env=env, check=True)
    sp.run(["git", "add", "CHANGELOG.md"], cwd=target, env=env, check=True)
    sp.run(["git", "commit", "-q", "-m", "init"], cwd=target, env=env, check=True)
    # dirty it: modify the tracked CHANGELOG and add an untracked file
    _write_changelog(target, CONFORMING_CHANGELOG + "\n<!-- local edit, uncommitted -->\n")
    (target / "untracked.txt").write_text("dirty\n", encoding="utf-8")
    status = sp.run(["git", "status", "--porcelain"], cwd=target,
                    capture_output=True, text=True, check=True).stdout
    assert status.strip() != ""  # premise: the target repo IS dirty

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "PASS" in out, out


def test_tag_mode_existing_tag_on_target_does_not_matter(tmp_path):
    """TAG_ALREADY_EXISTS immunity pin: a CI tag checkout has the tag being
    released already present in the target repo. The wrapper must still pass
    a conforming CHANGELOG (its fixture repo carries no tags)."""
    import subprocess as sp

    target = _make_target_repo(tmp_path, "tagged-target", CONFORMING_CHANGELOG)
    env = dict(os.environ)
    env.update({
        "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
        "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
    })
    sp.run(["git", "init", "-q"], cwd=target, env=env, check=True)
    sp.run(["git", "add", "CHANGELOG.md"], cwd=target, env=env, check=True)
    sp.run(["git", "commit", "-q", "-m", "init"], cwd=target, env=env, check=True)
    sp.run(["git", "tag", "-a", "v1.4.0", "-m", "x"], cwd=target, env=env, check=True)

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "PASS" in out, out


def test_tag_mode_missing_changelog_refused(tmp_path):
    target = tmp_path / "no-changelog"
    target.mkdir()

    rc, out, err = _run("--repo", str(target), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=MISSING_CHANGELOG" in err, err


def test_tag_mode_missing_repo_dir_refused(tmp_path):
    rc, out, err = _run("--repo", str(tmp_path / "does-not-exist"), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=NOT_A_REPO" in err, err


# ----------------------------------------------------------- structure mode --


def test_structure_mode_conforming_fixture_passes(tmp_path):
    target = _make_target_repo(tmp_path, "structure-ok", CONFORMING_CHANGELOG)

    rc, out, err = _run("--structure-only", "--repo", str(target))

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "PASS" in out, out


def test_structure_mode_no_unreleased_fails_with_named_reason(tmp_path):
    no_unreleased = "\n".join(
        line for line in CONFORMING_CHANGELOG.splitlines() if "[Unreleased]" not in line
    )
    target = _make_target_repo(tmp_path, "structure-no-unrel", no_unreleased)

    rc, out, err = _run("--structure-only", "--repo", str(target))

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=UNRELEASED_HEADING_COUNT" in err, err


def test_structure_mode_duplicate_unreleased_fails_with_named_reason(tmp_path):
    duplicated = CONFORMING_CHANGELOG.replace(
        "## [1.3.0] — 2026-09-16",
        "## [Unreleased] — 2026-09-21\n\n## [1.3.0] — 2026-09-16",
    )
    target = _make_target_repo(tmp_path, "structure-dup-unrel", duplicated)

    rc, out, err = _run("--structure-only", "--repo", str(target))

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=UNRELEASED_HEADING_COUNT" in err, err


def test_structure_mode_malformed_newest_heading_fails_with_named_reason(tmp_path):
    """A CHANGELOG whose newest release heading is not `## [X.Y.Z]` — e.g. a
    free-text section someone pasted above the real releases — fails the
    structure arm. Exactly one [Unreleased] is kept so the heading-count
    check passes and the malformed-newest-heading reason is the one that
    fires."""
    malformed = CONFORMING_CHANGELOG.replace("## [1.4.0] — 2026-09-20", "## WIP stuff")
    assert malformed.count("## [Unreleased]") == 1  # premise: count check passes
    target = _make_target_repo(tmp_path, "structure-malformed", malformed)

    rc, out, err = _run("--structure-only", "--repo", str(target))

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=NEWEST_RELEASE_HEADING_MALFORMED" in err, err


# ------------------------------------------------------------------ surface --


def test_help_arm_prints_usage_and_exits_zero():
    rc, out, err = _run("--help")

    assert rc == 0, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "usage:" in out, out
    assert "REASON=" not in err, err


def test_no_version_refused_with_usage_reason(tmp_path):
    rc, out, err = _run()

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=BAD_USAGE" in err, err


def test_structure_mode_rejects_version_argument(tmp_path):
    """--structure-only takes no version: passing one is a wrapper misuse,
    not a gate failure."""
    target = _make_target_repo(tmp_path, "structure-extra", CONFORMING_CHANGELOG)

    rc, out, err = _run("--structure-only", "--repo", str(target), "v1.4.0")

    assert rc == 2, f"exit {rc}\nstdout:\n{out}\nstderr:\n{err}"
    assert "REASON=BAD_USAGE" in err, err


def test_gate_runs_green_against_the_real_repo_head():
    """The real repo pins both arms as they must hold at every push/tag:
    structure mode passes (CI arm), and the tag naming the current newest
    release heading (1.1.0) passes, while an unreleased tag refuses."""
    rc_struct, out, err = _run("--structure-only")
    assert rc_struct == 0, f"structure arm: exit {rc_struct}\nstdout:\n{out}\nstderr:\n{err}"

    rc_named, out_named, err_named = _run("v1.1.0")
    assert rc_named == 0, f"v1.1.0 arm: exit {rc_named}\nstdout:\n{out_named}\nstderr:\n{err_named}"

    rc_unnamed, out_unnamed, err_unnamed = _run("v0.0.1")
    assert rc_unnamed == 2, f"v0.0.1 arm: exit {rc_unnamed}\nstdout:\n{out_unnamed}\nstderr:\n{err_unnamed}"
    assert "REASON=CHANGELOG_DOES_NOT_NAME_VERSION" in err_unnamed, err_unnamed
