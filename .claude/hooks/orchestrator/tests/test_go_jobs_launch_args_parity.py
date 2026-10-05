"""Parity fixtures for the Go Codex launch argv builder."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_launch_args" / "launch_args.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge


def _ptr(value):
    return value


def _cases():
    config = {
        "model_reasoning_effort": "high",
        "nested": {
            "quoted": 'a "quote" and \\ path\n雪',
            "enabled": True,
            "count": 7,
            "ratio": 1.25,
            "values": ["x", False, 2],
        },
    }
    config_args = bridge._flatten_config(config)
    config_args.extend((
        f"developer_instructions={bridge._toml('follow these instructions')}",
        f"base_instructions={bridge._toml('base rules')}",
        f"compact_prompt={bridge._toml('compact this')}",
        f"approval_policy={bridge._toml('never')}",
    ))
    cases = [
        {
            "name": "new_all_options",
            "input": {
                "binary_override": "codex", "name": "codex", "cwd": "C:/work",
                "model": "gpt-test", "config": config, "sandbox": "workspace-write",
                "output_path": "C:/tmp/last-message.txt",
                "developer_instructions": _ptr("follow these instructions"),
                "base_instructions": _ptr("base rules"), "compact_prompt": _ptr("compact this"),
                "approval_policy": _ptr("never"),
            },
            "expected": bridge._agent_command("codex", ["codex"], "codex", "C:/work", "gpt-test",
                                               config_args, "workspace-write", None,
                                               Path("C:/tmp/last-message.txt"), "prompt text stays on stdin"),
        },
        {
            "name": "resume_all_options",
            "input": {
                "binary_override": "codex", "name": "codex-reply", "cwd": "C:/ignored",
                "model": "gpt-test", "config": config, "thread_id": "thread-123",
                "output_path": "C:/tmp/resume.txt",
                "developer_instructions": _ptr("follow these instructions"),
                "base_instructions": _ptr("base rules"), "compact_prompt": _ptr("compact this"),
                "approval_policy": _ptr("never"),
            },
            "expected": bridge._agent_command("codex", ["codex"], "codex-reply", "C:/ignored", "gpt-test",
                                               config_args, None, "thread-123",
                                               Path("C:/tmp/resume.txt"), "prompt text stays on stdin"),
        },
        {
            "name": "new_optional_fields_absent",
            "input": {"binary_override": "codex", "name": "codex", "cwd": "C:/work",
                      "output_path": "C:/tmp/empty.txt"},
            "expected": bridge._agent_command("codex", ["codex"], "codex", "C:/work", None,
                                               [], None, None, Path("C:/tmp/empty.txt"), "stdin"),
        },
        {
            "name": "resume_optional_fields_absent",
            "input": {"binary_override": "codex", "name": "codex-reply", "thread_id": "thread-empty",
                      "output_path": "C:/tmp/empty-resume.txt"},
            "expected": bridge._agent_command("codex", ["codex"], "codex-reply", "", None,
                                               [], None, "thread-empty", Path("C:/tmp/empty-resume.txt"), "stdin"),
        },
    ]
    return cases


def test_launch_args_python_oracle_and_go_parity():
    cases = _cases()
    expected = {"cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_LAUNCH_ARGS") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    if not FIXTURE.is_file():
        pytest.fail("launch argv fixture is missing; set PURSER_REGEN_GO_JOBS_LAUNCH_ARGS=1")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture == expected
    go = shutil.which("go")
    if go is None:
        pytest.skip("Go toolchain unavailable")
    env = os.environ.copy()
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env.setdefault("GOCACHE", str(cache))
    result = subprocess.run([go, "test", "./internal/jobs/", "-run", "Launch"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True, timeout=120)
    assert result.returncode == 0, result.stdout + result.stderr


def test_codex_binary_missing_error(monkeypatch):
    monkeypatch.setattr(bridge.shutil, "which", lambda _name: None)
    with pytest.raises(ValueError, match="Codex executable was not found"):
        bridge._binary_prefix(None)


def test_windows_cmd_and_exe_prefix_resolution(tmp_path, monkeypatch):
    if os.name != "nt":
        pytest.skip("Windows launcher resolution")
    bin_dir = tmp_path / "bin"
    launcher = bin_dir / "codex.cmd"
    entry = bin_dir / "node_modules" / "@openai" / "codex" / "bin" / "codex.js"
    node = tmp_path / "node.exe"
    (entry.parent).mkdir(parents=True)
    bin_dir.mkdir(exist_ok=True)
    launcher.touch()
    entry.touch()
    node.touch()
    (bin_dir / "codex.exe").touch()
    monkeypatch.setenv("PATH", str(tmp_path) + os.pathsep + str(bin_dir))
    assert bridge._binary_prefix(str(bin_dir / "codex")) == [shutil.which("node"), str(entry)]
    assert bridge._binary_prefix(str(bin_dir / "codex.exe")) == [str(bin_dir / "codex.exe")]
