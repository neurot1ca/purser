import os
import shutil
import stat
import subprocess
from pathlib import Path
import sys
import uuid

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import project_root


@pytest.fixture()
def tmp_path():
    root = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "project-root"
        / uuid.uuid4().hex
    )
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)

        shutil.rmtree(root, onerror=make_writable)


def _git_repo(path: Path) -> Path:
    path.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=path, check=True, timeout=10)
    return path


def test_tool_input_cwd_precedes_event_cwd_and_resolves_top_level(tmp_path):
    tool_repo = _git_repo(tmp_path / "tool-repo")
    event_repo = _git_repo(tmp_path / "event-repo")
    nested = tool_repo / "nested" / "work"
    nested.mkdir(parents=True)

    resolved, reason = project_root.resolve_project_root(
        {"cwd": str(event_repo), "tool_input": {"cwd": str(nested)}}
    )

    assert resolved == tool_repo.resolve()
    assert reason == ""


def test_event_cwd_is_used_when_tool_input_has_no_cwd(tmp_path):
    repo = _git_repo(tmp_path / "event-repo")

    resolved, reason = project_root.resolve_project_root(
        {"cwd": str(repo), "tool_input": {}}
    )

    assert resolved == repo.resolve()
    assert reason == ""


def test_explicit_root_precedes_hook_candidates_without_git_lookup(tmp_path):
    explicit = tmp_path / "explicit"
    explicit.mkdir()

    resolved, reason = project_root.resolve_project_root(
        {"cwd": str(tmp_path / "missing")}, explicit
    )

    assert resolved == explicit.resolve()
    assert reason == ""
