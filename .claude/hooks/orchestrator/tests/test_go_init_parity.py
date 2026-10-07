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
    assert (native.returncode, native.stdout, native.stderr) == (oracle.returncode, oracle.stdout, oracle.stderr)


def test_init_apply_files_parity(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    args = ["--apply", "--coder", "claude", "--reviewer", "none", "--trust", "--scaffold-profile"]
    home.mkdir(); project.mkdir()
    oracle = _run_python(home, project, args)
    oracle_files = _files(home) | {"project/" + k: v for k, v in _files(project).items()}
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


def test_init_adopt_model_version_parity(native_init, tmp_path):
    home, project = tmp_path / "home", tmp_path / "project"
    home.mkdir(); project.mkdir()
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    source = tmp_path / "codex.go"
    source.write_text('package main\nimport ("fmt";"os";"strings")\nfunc main(){a:=strings.Join(os.Args[1:]," ");if a=="--version"{fmt.Print("codex parity");return};if a=="debug models"{fmt.Print(`{"models":[{"slug":"gpt-6.1-luna","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`);return}}\n', encoding="utf-8")
    fake_codex = fake_bin / ("codex.exe" if os.name == "nt" else "codex")
    build_env = os.environ.copy()
    build_env["GOCACHE"] = str(REPO / ".tmp" / "go-cache")
    subprocess.run(["go", "build", "-o", str(fake_codex), str(source)], cwd=REPO / "go", env=build_env, check=True)

    def seed():
        state = home / ".claude" / "purser" / "model-catalog" / "catalog.json"
        state.parent.mkdir(parents=True, exist_ok=True)
        state.write_text(json.dumps({"fetched_at": "2026-10-01T00:00:00Z", "dropped_count": 0,
                                     "providers": {"openai": {"models": {"gpt-6.1-luna": {
                                         "id": "gpt-6.1-luna", "name": "GPT Luna", "reasoning": True,
                                         "family": "openai", "limits": {}}}}}}), encoding="utf-8")
        config = home / ".orchestrator" / "config.toml"
        config.parent.mkdir(parents=True, exist_ok=True)
        route_names = ["mechanical", "subtle", "normal", "consequential", "escalated", "operation", "plan_standard"]
        route_values = ", ".join(f'{name} = {{ model = "gpt-6-luna", effort = "medium" }}' for name in route_names)
        config.write_text(f'schema_version = 1\n[roles]\ncoder = "codex"\n[coding]\ncoder_agent = "codex"\nroutes = {{{route_values}}}\n', encoding="utf-8")

    seed()
    oracle = _run_python(home, project, ["--adopt-model-version", "gpt-6.1-luna"], [str(fake_bin)])
    oracle_config = (home / ".orchestrator" / "config.toml").read_bytes()
    oracle_state = json.loads((home / ".claude" / "purser" / "model-version-notice.json").read_text(encoding="utf-8"))
    _reset(home, project)
    seed()
    native = _run(native_init, home, project, ["init", "--adopt-model-version", "gpt-6.1-luna"], [str(fake_bin)])
    native_config = (home / ".orchestrator" / "config.toml").read_bytes()
    native_state = json.loads((home / ".claude" / "purser" / "model-version-notice.json").read_text(encoding="utf-8"))
    assert (native.returncode, native.stdout) == (oracle.returncode, oracle.stdout)
    assert native_config == oracle_config
    assert native_state == oracle_state
