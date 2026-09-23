import os
from pathlib import Path
import subprocess
import sys
import time
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from project_profile import Check
from checks import run_checks, select_checks


@pytest.fixture
def repo():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / uuid.uuid4().hex
    root.mkdir(parents=True)
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    return root


def _python(name, script, timeout=60):
    return Check(name, (sys.executable, "-c", script), None, timeout)


def test_pass_and_empty_selection(repo):
    result = run_checks(repo, (_python("pass", "print('fine')"),), deadline_seconds=5)
    assert result["status"] == "PASS"
    assert result["results"][0]["tail"] == ""
    assert run_checks(repo, (), deadline_seconds=5) == {"status": "NOT_RUN", "results": [], "side_effect_paths": []}


def test_fail_reports_tail(repo):
    result = run_checks(repo, (_python("fail", "print('failed'); raise SystemExit(7)"),), deadline_seconds=5)
    assert result["status"] == "FAIL"
    assert result["results"][0]["exit_code"] == 7
    assert "failed" in result["results"][0]["tail"]


def test_timeout_kills_child_and_deadline_skips_later_check(repo):
    marker = repo / "child.txt"
    child_script = f"import time, pathlib; time.sleep(1.5); pathlib.Path({str(marker)!r}).write_text('alive')"
    script = f"import subprocess, sys, time; subprocess.Popen([sys.executable, '-c', {child_script!r}]); time.sleep(10)"
    result = run_checks(repo, (_python("slow", script, 1), _python("later", "print('later')")), deadline_seconds=1)
    assert result["status"] == "UNKNOWN"
    assert result["results"][0]["status"] == "UNKNOWN"
    assert result["results"][1]["tail"] == "not run: deadline"
    time.sleep(1.7)
    assert not marker.exists()


def test_unresolvable_executable_is_unknown(repo):
    result = run_checks(repo, (Check("missing", ("nonexistent-purser-check-command-xyz",), None, 60),), deadline_seconds=5)
    assert result["status"] == "UNKNOWN"
    assert "unresolvable" in result["results"][0]["tail"]


def test_untracked_side_effect(repo):
    result = run_checks(repo, (_python("write", "from pathlib import Path; Path('generated.txt').write_text('x')"),), deadline_seconds=5)
    assert result["status"] == "PASS"
    assert result["side_effect_paths"] == ["generated.txt"]


def test_deadline_exhaustion_still_reports_side_effect(repo):
    script = "from pathlib import Path; import time; Path('generated.txt').write_text('x'); time.sleep(10)"
    result = run_checks(repo, (_python("write_then_wait", script),), deadline_seconds=1)
    assert result["status"] == "UNKNOWN"
    assert result["side_effect_paths"] == ["generated.txt"]


def test_select_checks_uses_protected_globs():
    always = Check("always", (sys.executable,), None, 60)
    scoped = Check("scoped", (sys.executable,), ("app/**", "**/*.ts"), 60)
    assert select_checks((always, scoped), ("other/file.py",)) == (always,)
    assert select_checks((always, scoped), ("./app/deep/file.py",)) == (always, scoped)
