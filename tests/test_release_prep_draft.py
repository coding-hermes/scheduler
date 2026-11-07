"""RELEASE-041 fixtures for ``scripts/release-prep.sh --draft-from-range``.

The row's problem: nothing populated CHANGELOG's ``[Unreleased]`` section, so a
release cut over un-noted commits needed a hand-written entry COMMIT (plus its
CI run) purely to get past ``EMPTY_UNRELEASED`` before the rollover could even
run. Draft mode derives the entries from the commit range and prints them; the
operator pastes them and the rollover lands in ONE commit.

Pinned here (each arm is one test):

  * ``--help`` documents the draft-mode usage line and exits 0;
  * the draft prints feat/fix/perf entries ordered Features -> Fixes ->
    Performance, derived from ``git log --no-merges <last-tag>..HEAD`` (a merge
    commit whose subject LOOKS like an entry is excluded — the negative
    control), with non-conventional subjects dropped;
  * the draft writes nothing: ``git status --porcelain`` and the CHANGELOG bytes
    are unchanged after the run;
  * draft mode takes no version and refuses misuse by name (``BAD_USAGE``);
  * a repository with no reachable tag refuses ``NO_PRIOR_TAG`` and accepts an
    explicit ``--since``;
  * promotion still refuses an empty ``[Unreleased]`` body
    (``EMPTY_UNRELEASED`` — the refusal this mode must not weaken);
  * a pasted draft plus the rollover is ONE working-tree change: dirty paths
    other than ``CHANGELOG.md`` still refuse ``DIRTY_TREE``, while a dirty
    ``CHANGELOG.md`` is promoted into the new release heading.

Environment-free by construction: fixtures are built in ``tmp_path`` with
``HOME`` pointed at a temp dir and the host's global/system git config disabled,
so no ``~/.hermes`` state, hooks, or network is involved.

Run:  python3 -m pytest tests/test_release_prep_draft.py -v
"""
from __future__ import annotations

import os
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / "scripts" / "release-prep.sh"

EMPTY_UNRELEASED_CHANGELOG = """\
# Changelog

All notable changes.

## [Unreleased]

## [1.0.0] — 2026-01-01

- Initial release.
"""


def _env(home: Path) -> dict:
    """Host git state must not leak into the fixture (global config, hooks,
    or an enclosing repo's GIT_DIR)."""
    env = dict(os.environ)
    for var in (
        "GIT_DIR",
        "GIT_WORK_TREE",
        "GIT_INDEX_FILE",
        "GIT_OBJECT_DIRECTORY",
        "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    ):
        env.pop(var, None)
    env["HOME"] = str(home)
    env["GIT_CONFIG_GLOBAL"] = os.devnull
    env["GIT_CONFIG_SYSTEM"] = os.devnull
    env["GIT_AUTHOR_NAME"] = "release-prep-fixture"
    env["GIT_AUTHOR_EMAIL"] = "noreply@example.invalid"
    env["GIT_COMMITTER_NAME"] = "release-prep-fixture"
    env["GIT_COMMITTER_EMAIL"] = "noreply@example.invalid"
    return env


def _git(repo: Path, home: Path, *args: str) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), "-c", "core.hooksPath=/dev/null",
         "-c", "commit.gpgsign=false", *args],
        capture_output=True, text=True, env=_env(home),
    )
    assert proc.returncode == 0, f"git {' '.join(args)} failed: {proc.stderr}"
    return proc.stdout.strip()


def _commit(repo: Path, home: Path, subject: str) -> str:
    marker = repo / "touch.txt"
    marker.write_text(subject + "\n", encoding="utf-8")
    _git(repo, home, "add", "--", "touch.txt")
    _git(repo, home, "commit", "-q", "-m", subject)
    return _git(repo, home, "rev-parse", "--short", "HEAD")


def _make_repo(tmp_path: Path) -> tuple[Path, Path]:
    """A repo whose only tag (v1.0.0) is the initial commit, so every commit
    added afterwards falls inside the draft range."""
    home = tmp_path / "home"
    home.mkdir()
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, home, "init", "-q", "-b", "main")
    (repo / "CHANGELOG.md").write_text(EMPTY_UNRELEASED_CHANGELOG, encoding="utf-8")
    _git(repo, home, "add", "--", "CHANGELOG.md")
    _git(repo, home, "commit", "-q", "-m", "chore: initial import")
    _git(repo, home, "tag", "v1.0.0")
    return repo, home


def _run(repo: Path, home: Path, *args: str) -> tuple[int, str, str]:
    proc = subprocess.run(
        ["bash", str(SCRIPT), "--repo", str(repo), *args],
        capture_output=True, text=True, env=_env(home),
    )
    return proc.returncode, proc.stdout, proc.stderr


def _reason(stderr: str) -> str:
    for line in stderr.splitlines():
        if line.startswith("REASON="):
            return line.split("=", 1)[1]
    return ""


def _draft_fixture(tmp_path: Path) -> tuple[Path, Path, dict[str, str]]:
    """Tagged repo plus one commit of every shape the draft must classify."""
    repo, home = _make_repo(tmp_path)
    shas = {
        "feat": _commit(repo, home, "feat(api): add /api/v1/widgets"),
        "fix": _commit(repo, home, "fix(scheduler): stop the double spawn"),
        "perf": _commit(repo, home, "perf(lanehook): scan the tree once"),
        "chore": _commit(repo, home, "chore(board): bookkeeping"),
        "docs": _commit(repo, home, "docs: explain the widgets endpoint"),
        "noscope": _commit(repo, home, "fix: repair the unscoped defect"),
    }
    # A merge commit whose OWN subject looks like an entry: --no-merges must
    # drop it (it summarises no work of its own).
    _git(repo, home, "switch", "-q", "-c", "topic")
    shas["topic"] = _commit(repo, home, "feat(topic): real work on a branch")
    _git(repo, home, "switch", "-q", "main")
    _git(repo, home, "merge", "-q", "--no-ff", "-m", "feat(merge): merge branch topic", "topic")
    shas["merge"] = _git(repo, home, "rev-parse", "--short", "HEAD")
    return repo, home, shas


def test_help_documents_the_draft_mode(tmp_path: Path) -> None:
    repo, home = _make_repo(tmp_path)
    rc, out, _ = _run(repo, home, "--help")
    assert rc == 0
    assert "--draft-from-range" in out
    assert "scripts/release-prep.sh --draft-from-range [--repo DIR] [--since TAG]" in out
    assert "git log --no-merges" in out


def test_draft_orders_entries_and_drops_non_entries(tmp_path: Path) -> None:
    repo, home, shas = _draft_fixture(tmp_path)
    rc, out, err = _run(repo, home, "--draft-from-range")
    assert rc == 0, err

    assert "v1.0.0..HEAD" in out
    # Ordered feat -> fix -> perf.
    pos = [out.index(heading) for heading in ("### Features", "### Fixes", "### Performance")]
    assert pos == sorted(pos), out

    # Entries carry the derived subject text and the commit's short sha.
    assert f"api: add /api/v1/widgets — `{shas['feat']}`" in out
    assert f"scheduler: stop the double spawn — `{shas['fix']}`" in out
    assert f"lanehook: scan the tree once — `{shas['perf']}`" in out
    # An unscoped subject keeps its text without a fabricated scope prefix.
    assert f"- repair the unscoped defect — `{shas['noscope']}`" in out

    # Negative control: non-conventional subjects and the merge commit are not
    # entries, even though the merge subject parses as one.
    assert "bookkeeping" not in out
    assert "explain the widgets endpoint" not in out
    assert "merge branch topic" not in out
    assert f"`{shas['merge']}`" not in out
    assert f"`{shas['topic']}`" in out  # the branch's real work IS in range


def test_draft_writes_nothing(tmp_path: Path) -> None:
    repo, home, _ = _draft_fixture(tmp_path)
    changelog = repo / "CHANGELOG.md"
    before = changelog.read_bytes()
    rc, _, err = _run(repo, home, "--draft-from-range")
    assert rc == 0, err
    assert _git(repo, home, "status", "--porcelain") == ""
    assert changelog.read_bytes() == before


def test_draft_accepts_an_explicit_since_and_refuses_a_version(tmp_path: Path) -> None:
    repo, home, shas = _draft_fixture(tmp_path)
    rc, out, err = _run(repo, home, "--draft-from-range", "--since", "v1.0.0")
    assert rc == 0, err
    assert f"`{shas['feat']}`" in out

    rc, _, err = _run(repo, home, "--draft-from-range", "v1.1.0")
    assert rc == 2
    assert _reason(err) == "BAD_USAGE"
    assert _git(repo, home, "status", "--porcelain") == ""


def test_draft_without_a_reachable_tag_refuses_by_name(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    repo = tmp_path / "untagged"
    repo.mkdir()
    _git(repo, home, "init", "-q", "-b", "main")
    (repo / "CHANGELOG.md").write_text(EMPTY_UNRELEASED_CHANGELOG, encoding="utf-8")
    _git(repo, home, "add", "--", "CHANGELOG.md")
    _git(repo, home, "commit", "-q", "-m", "chore: initial import")
    first = _git(repo, home, "rev-parse", "--short", "HEAD")
    _commit(repo, home, "fix(core): first real fix")

    rc, _, err = _run(repo, home, "--draft-from-range")
    assert rc == 2
    assert _reason(err) == "NO_PRIOR_TAG"

    rc, out, err = _run(repo, home, "--draft-from-range", "--since", first)
    assert rc == 0, err
    assert "- core: first real fix —" in out


def test_promotion_still_refuses_an_empty_unreleased_body(tmp_path: Path) -> None:
    repo, home = _make_repo(tmp_path)
    _git(repo, home, "update-ref", "refs/remotes/origin/main", "HEAD")
    rc, _, err = _run(repo, home, "v1.0.1")
    assert rc == 2
    assert _reason(err) == "EMPTY_UNRELEASED"


def test_pasted_draft_promotes_in_one_commit(tmp_path: Path) -> None:
    repo, home = _make_repo(tmp_path)
    _commit(repo, home, "feat(api): add /api/v1/widgets")
    _git(repo, home, "update-ref", "refs/remotes/origin/main", "HEAD")

    # The operator pastes the draft into the working tree — no commit, no CI.
    changelog = repo / "CHANGELOG.md"
    changelog.write_text(
        EMPTY_UNRELEASED_CHANGELOG.replace(
            "## [Unreleased]\n",
            "## [Unreleased]\n\n### Features\n- api: add /api/v1/widgets — `deadbee`\n",
        ),
        encoding="utf-8",
    )
    # Any OTHER dirty path still refuses: the release commit must carry only
    # the rollover.
    stray = repo / "scratch-notes.txt"
    stray.write_text("not part of the release\n", encoding="utf-8")
    rc, _, err = _run(repo, home, "v1.1.0")
    assert rc == 2
    assert _reason(err) == "DIRTY_TREE"
    stray.unlink()

    rc, out, err = _run(repo, home, "v1.1.0")
    assert rc == 0, err
    assert "uncommitted edits" in out

    rolled = changelog.read_text(encoding="utf-8")
    # The pasted entry was promoted into the release heading...
    assert "## [1.1.0] — " in rolled
    assert "api: add /api/v1/widgets" in rolled.split("## [1.1.0]", 1)[1]
    # ...and a fresh, empty [Unreleased] sits above it.
    assert rolled.count("## [Unreleased]") == 1
    # ONE commit's worth of change: CHANGELOG.md is the only dirty path.
    assert _git(repo, home, "status", "--porcelain") == "M CHANGELOG.md"
