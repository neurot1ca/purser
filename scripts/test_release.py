import json
import os
import shutil
import subprocess
import sys
import uuid
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
RELEASE = ROOT / "scripts" / "release.py"


@pytest.fixture
def tmp_path():
    scratch = ROOT / ".tmp" / f"release-test-{uuid.uuid4().hex}"
    scratch.mkdir()
    try:
        yield scratch
    finally:
        shutil.rmtree(scratch, ignore_errors=True)


def run(cmd, cwd, *, env=None, check=True):
    result = subprocess.run(
        cmd, cwd=cwd, env=env, text=True, capture_output=True, check=False
    )
    if check:
        assert result.returncode == 0, result.stdout + result.stderr
    return result


def repo(tmp_path):
    path = tmp_path / "repo"
    path.mkdir()
    run(["git", "init", "-q"], path)
    env = os.environ.copy()
    env.update(
        GIT_AUTHOR_NAME="Test Author",
        GIT_AUTHOR_EMAIL="author@example.test",
        GIT_COMMITTER_NAME="Test Committer",
        GIT_COMMITTER_EMAIL="committer@example.test",
        GIT_CONFIG_NOSYSTEM="1",
    )
    (path / ".claude-plugin").mkdir()
    (path / ".claude-plugin" / "plugin.json").write_text(
        '{\n  "name": "purser",\n  "version": "0.1.12"\n}\n', encoding="utf-8"
    )
    (path / "CHANGELOG.md").write_text(
        "# Changelog\n\nPreamble.\n\n## 0.1.12 — 2026-09-24\n\n- Existing entry (`aaaaaaa`).\n",
        encoding="utf-8",
    )
    (path / "base.txt").write_text("base\n", encoding="utf-8")
    (path / "scripts").mkdir()
    shutil.copy2(RELEASE, path / "scripts" / "release.py")
    run(["git", "add", "-f", "--", "."], path, env=env)
    run(["git", "commit", "-qm", "initial"], path, env=env)
    return path, env


def fake_claude(tmp_path, code=0):
    executable = tmp_path / "fake_claude.py"
    executable.write_text(
        "import os, sys\n"
        "print('fake claude:', ' '.join(sys.argv[1:]))\n"
        "fail = int(os.environ.get('FAKE_CLAUDE_EXIT', '0')) if 'purser@purser-local' in sys.argv else 0\n"
        "if fail:\n"
        "    print('controlled updater failure', file=sys.stderr)\n"
        "sys.exit(fail)\n",
        encoding="utf-8",
    )
    return executable


def release(path, env, claude, *args):
    return run(
        [sys.executable, str(path / "scripts" / "release.py"), "--claude-bin", str(claude), *args],
        path,
        env=env,
        check=False,
    )


def test_unrelated_staged_file_is_refused_without_repo_changes(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    (path / "unrelated.txt").write_text("staged elsewhere\n", encoding="utf-8")
    run(["git", "add", "--", "unrelated.txt"], path, env=env)
    before_head = run(["git", "rev-parse", "HEAD"], path).stdout
    before_index = run(["git", "diff", "--cached", "--binary"], path).stdout
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 2
    assert run(["git", "rev-parse", "HEAD"], path).stdout == before_head
    assert run(["git", "diff", "--cached", "--binary"], path).stdout == before_index
    assert (path / "CHANGELOG.md").read_text(encoding="utf-8").count("## 0.1.13") == 0


def test_only_listed_paths_land_in_change_commit(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    (path / "unlisted.txt").write_text("leave for later\n", encoding="utf-8")
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    names = run(["git", "show", "--pretty=format:", "--name-only", "HEAD~1"], path).stdout.splitlines()
    assert names == ["change.txt"]
    assert run(["git", "status", "--porcelain"], path).stdout.strip() == "?? unlisted.txt"


def test_changelog_uses_change_hash_and_inserts_before_newest(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    change_hash = run(["git", "rev-parse", "HEAD~1"], path).stdout.strip()[:7]
    changelog = (path / "CHANGELOG.md").read_text(encoding="utf-8")
    assert changelog.index("## 0.1.13 — ") < changelog.index("## 0.1.12 — ")
    assert f"- **Change:** added change (`{change_hash}`)." in changelog
    assert f"added change. (`{change_hash}`)" not in changelog


def test_failed_plugin_update_resumes_without_new_commits(tmp_path):
    path, env = repo(tmp_path)
    env["FAKE_CLAUDE_EXIT"] = "7"
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    claude = fake_claude(tmp_path)
    args = (
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change",
        "--title",
        "add change",
        "--version",
        "0.1.13",
    )
    failed = release(path, env, claude, *args)
    assert failed.returncode == 1
    assert "plugin update" in failed.stdout + failed.stderr
    assert "controlled updater failure" in failed.stdout + failed.stderr
    after_failure = run(["git", "rev-list", "--count", "HEAD"], path).stdout
    env["FAKE_CLAUDE_EXIT"] = "0"
    resumed = release(path, env, claude, *args)
    assert resumed.returncode == 0, resumed.stdout + resumed.stderr
    assert run(["git", "rev-list", "--count", "HEAD"], path).stdout == after_failure
    assert (path / "CHANGELOG.md").read_text(encoding="utf-8").count("## 0.1.13 — ") == 1


def test_dry_run_changes_nothing(tmp_path):
    path, env = repo(tmp_path)
    (path / "change.txt").write_text("the change\n", encoding="utf-8")
    before_head = run(["git", "rev-parse", "HEAD"], path).stdout
    result = release(
        path,
        env,
        fake_claude(tmp_path),
        "--paths",
        "change.txt",
        "--message",
        "feature: add change",
        "--entry",
        "- **Change:** added change.",
        "--title",
        "add change",
        "--version",
        "0.1.13",
        "--dry-run",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert run(["git", "rev-parse", "HEAD"], path).stdout == before_head
    assert run(["git", "status", "--porcelain"], path).stdout == "?? change.txt\n"
    assert json.loads((path / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8"))["version"] == "0.1.12"
    assert "dry-run" in result.stdout
