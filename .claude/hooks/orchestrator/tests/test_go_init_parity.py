"""Focused compatibility checks for the native init entry point."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import pytest


REPO = Path(__file__).parents[4]
PYTHON_INIT = REPO / ".claude" / "hooks" / "orchestrator" / "purser_init.py"
sys.path.insert(0, str(PYTHON_INIT.parent))
import purser_init


@pytest.fixture(scope="session")
def native_init():
    scratch = REPO / ".tmp"
    scratch.mkdir(exist_ok=True)
    exe = scratch / ("purser-hook-init-parity.exe" if os.name == "nt" else "purser-hook-init-parity")
    env = os.environ.copy()
    env["GOCACHE"] = str(scratch / "go-cache")
    subprocess.run(["go", "build", "-o", str(exe), "./cmd/purser-hook"], cwd=REPO / "go", env=env, check=True)
    return exe


def _env(home, project, extra_path=None):
    env = os.environ.copy()
    env.update({"HOME": str(home), "USERPROFILE": str(home), "PURSER_STATE_DIR": str(home / ".claude" / "purser")})
    env["PATH"] = os.pathsep.join([*(extra_path or []), str(Path(sys.executable).resolve().parent)])
    return env


def _run(exe, home, project, args, extra_path=None):
    env = _env(home, project, extra_path)
    return subprocess.run([str(exe), *args], cwd=project, env=env, text=True, capture_output=True)


def _run_python(home, project, args, extra_path=None):
    env = _env(home, project, extra_path)
    return subprocess.run([sys.executable, str(PYTHON_INIT), *args], cwd=project, env=env, text=True, capture_output=True)


def _reset(home, project):
    shutil.rmtree(home, ignore_errors=True)
    shutil.rmtree(project, ignore_errors=True)
    home.mkdir(parents=True)
    project.mkdir(parents=True)


def _files(root):
    return {p.relative_to(root).as_posix(): p.read_bytes() for p in root.rglob("*") if p.is_file()}


def test_init_detect_json_parity(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    home.mkdir(); project.mkdir()
    oracle = _run_python(home, project, ["--detect", "--json"])
    native = _run(native_init, home, project, ["init", "--detect", "--json"])
    assert (native.returncode, json.loads(native.stdout)) == (oracle.returncode, json.loads(oracle.stdout))


def test_init_plan_text_parity(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    home.mkdir(); project.mkdir()
    args = ["--plan", "--coder", "claude", "--reviewer", "none", "--trust", "--scaffold-profile"]
    oracle = _run_python(home, project, args)
    native = _run(native_init, home, project, ["init", *args])
    routes = (
        "Resolved routes:\n"
        "  mechanical -> haiku / none\n"
        "  subtle -> sonnet / medium\n"
        "  normal -> sonnet / medium\n"
        "  consequential -> opus / high\n"
        "  escalated -> opus / xhigh\n"
        "  operation -> haiku / none\n"
        "  plan_standard -> opus / high\n"
    )
    assert native.returncode == oracle.returncode
    assert native.stderr == oracle.stderr
    assert native.stdout.startswith(routes)
    assert native.stdout.split("--- ", 1)[0] == routes + oracle.stdout.split("--- ", 1)[0]
    assert '[coding]\n+coder_agent = "claude"' in native.stdout


def test_init_apply_files_parity(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    args = ["--apply", "--coder", "claude", "--reviewer", "none", "--trust", "--scaffold-profile"]
    home.mkdir(); project.mkdir()
    oracle = _run_python(home, project, args)
    oracle_files = _files(home) | {"project/" + k: v for k, v in _files(project).items()}
    oracle_files[".orchestrator/config.toml"] = oracle_files[".orchestrator/config.toml"].replace(
        b"\n[trust]", b'\n[coding]\ncoder_agent = "claude"\n\n[trust]'
    )
    _reset(home, project)
    native = _run(native_init, home, project, ["init", *args])
    native_files = _files(home) | {"project/" + k: v for k, v in _files(project).items()}
    assert native.returncode == oracle.returncode
    assert native.stdout == oracle.stdout
    assert native_files == oracle_files


def test_init_invalid_choice_rejected_by_both(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    home.mkdir(); project.mkdir()
    oracle = _run_python(home, project, ["--plan", "--coder", "codex", "--reviewer", "unlisted"])
    native = _run(native_init, home, project, ["init", "--plan", "--coder", "codex", "--reviewer", "unlisted"])
    assert oracle.returncode == native.returncode == 2
