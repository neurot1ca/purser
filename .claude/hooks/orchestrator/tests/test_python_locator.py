import json
import os
from pathlib import Path
import subprocess
import sys
import shutil
import uuid

import pytest


ROOT = Path(__file__).parents[4]
LOCATOR = ROOT / "bin" / "python-locator.js"


@pytest.fixture
def scratch():
    root = ROOT / ".tmp" / f"python-locator-{uuid.uuid4().hex}"
    root.mkdir(parents=True)
    yield root
    shutil.rmtree(root, ignore_errors=True)


def node(code, *, env=None):
    return subprocess.run(["node", "-e", code], capture_output=True, text=True, env=env, check=False)


def cannot_spawn_from_node():
    result = node("const r=require('node:child_process').spawnSync(process.execPath,['--version']); process.stdout.write(r.error?.code || 'OK')")
    return result.stdout == "EPERM"


def test_manifests_use_locator():
    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8"))["hooks"]
    for entries in hooks.values():
        for entry in entries:
            command = entry["hooks"][0]
            assert command["command"] == "node"
            assert command["args"][0] == "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js"
    mcp = json.loads((ROOT / "plugin/mcp.json").read_text(encoding="utf-8"))["mcpServers"]["codex-main"]
    assert mcp["command"] == "node"
    assert mcp["args"][0] == "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js"


def test_env_override_wins_for_real(scratch):
    stored = scratch / ".orchestrator" / "python"
    stored.parent.mkdir()
    stored.write_text("missing-python\n", encoding="utf-8")
    env = {**os.environ, "PURSER_PYTHON": sys.executable, "USERPROFILE": str(scratch), "HOME": str(scratch)}
    code = f'''const locator=require({json.dumps(str(LOCATOR))}); const selected=locator.resolvePython(locator.candidates(), command => ({{status:0,stdout:JSON.stringify({{path:command,version:[3,12,0]}})}})); process.stdout.write(JSON.stringify(selected));'''
    stubbed = node(code, env=env)
    assert stubbed.returncode == 0
    assert json.loads(stubbed.stdout)["source"] == "PURSER_PYTHON"
    if cannot_spawn_from_node():
        return
    result = subprocess.run(["node", str(LOCATOR), "--resolve-json"], capture_output=True, text=True, env=env)
    assert result.returncode == 0
    assert json.loads(result.stdout)["source"] == "PURSER_PYTHON"


def test_stored_path_wins_over_candidates(scratch):
    stored = scratch / ".orchestrator" / "python"
    stored.parent.mkdir()
    stored.write_text(sys.executable + "\n", encoding="utf-8")
    env = {**os.environ, "USERPROFILE": str(scratch), "HOME": str(scratch)}
    env.pop("PURSER_PYTHON", None)
    code = f'''const locator=require({json.dumps(str(LOCATOR))}); const selected=locator.resolvePython(locator.candidates(), command => ({{status:0,stdout:JSON.stringify({{path:command,version:[3,12,0]}})}})); process.stdout.write(JSON.stringify(selected));'''
    stubbed = node(code, env=env)
    assert stubbed.returncode == 0
    assert json.loads(stubbed.stdout)["source"] == "~/.orchestrator/python"
    if cannot_spawn_from_node():
        return
    result = subprocess.run(["node", str(LOCATOR), "--resolve-json"], capture_output=True, text=True, env=env)
    assert result.returncode == 0
    assert json.loads(result.stdout)["source"] == "~/.orchestrator/python"


def test_old_candidate_is_skipped():
    code = f'''const locator = require({json.dumps(str(LOCATOR))});
const list = [{{command:'old',prefix:[],source:'old'}},{{command:'new',prefix:[],source:'new'}}];
const result = locator.resolvePython(list, command => ({{status:0,stdout:JSON.stringify({{path:command,version:command==='old'?[3,10,9]:[3,11,0]}})}}));
process.stdout.write(JSON.stringify(result));'''
    result = node(code)
    assert result.returncode == 0
    assert json.loads(result.stdout)["source"] == "new"


def test_no_interpreter_has_event_specific_exit():
    for mode, expected in (("pre-codex", 2), ("hook", 0), ("mcp", 1)):
        code = f'''const locator = require({json.dumps(str(LOCATOR))}); process.exit(locator.main([{json.dumps(mode)},'target.py'], () => null));'''
        result = node(code)
        assert result.returncode == expected
        assert result.stderr == "Purser: no Python 3.11+ found; run /purser:check\n"


def test_stdin_stdout_and_exit_code_pass_through(scratch):
    if cannot_spawn_from_node():
        pytest.skip("sandbox denies Node child processes (EPERM)")
    target = scratch / "target.py"
    target.write_text("import sys\nsys.stdout.buffer.write(sys.stdin.buffer.read())\nsys.exit(7)\n", encoding="utf-8")
    env = {**os.environ, "PURSER_PYTHON": sys.executable}
    result = subprocess.run(["node", str(LOCATOR), "hook", str(target)], input=b"hello\x00world", capture_output=True, env=env)
    assert result.returncode == 7
    assert result.stdout == b"hello\x00world"


def test_spawned_python_always_receives_utf8_mode(scratch):
    code = f'''const cp=require('node:child_process');
cp.spawnSync=(command,args,options)=>{{process.stdout.write(options.env.PYTHONUTF8);return {{status:0}}}};
const locator=require({json.dumps(str(LOCATOR))});
process.exit(locator.main(['hook','target.py'],()=>({{path:'stub-python'}})));'''
    result = node(code, env={**os.environ, "PYTHONUTF8": "0"})

    assert result.returncode == 0
    assert result.stdout == "1"
