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
    root.mkdir(parents=True, exist_ok=True)
    yield root
    shutil.rmtree(root, ignore_errors=True)


def node(code, *, env=None):
    return subprocess.run(["node", "-e", code], capture_output=True, text=True, env=env, check=False)


def isolated_locator(scratch):
    bindir = scratch / "bin"
    bindir.mkdir()
    locator = bindir / "python-locator.js"
    shutil.copy2(LOCATOR, locator)
    return locator


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
    assert mcp["args"][1:] == ["go-mcp", "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/codex_exec_mcp.py"]


def test_go_mcp_prefers_supported_binary_and_forwards_arguments(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''const fs=require('node:fs'), locator=require({json.dumps(str(locator))});
const binary=locator.goBinaryPath(); if(!binary)process.exit(77); fs.writeFileSync(binary,'stub');
const calls=[]; let resolves=0;
const status=locator.main(['go-mcp',{json.dumps(script)},'--codex-bin','custom'],()=>{{resolves++;return {{path:'python'}}}},(command,args,options)=>{{calls.push({{command,args,stdio:options.stdio}});return {{pid:123,status:0}}}});
    fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls,resolves}}));'''
    result = node(code)
    if result.returncode == 77:
        pytest.skip("host platform has no packaged Go target")
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 0
    assert len(observed["calls"]) == 1
    assert observed["calls"][0]["command"].startswith(str(locator.parent / "purser-hook-"))
    assert observed["calls"][0]["args"] == ["mcp", "--codex-bin", "custom"]
    assert observed["calls"][0]["stdio"] == "inherit"
    assert observed["resolves"] == 0
    assert result.stderr == ""


def test_go_mcp_missing_binary_falls_back_with_marker(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''const locator=require({json.dumps(str(locator))}); let resolves=0; const calls=[];
const status=locator.main(['go-mcp',{json.dumps(script)}],()=>{{resolves++;return {{path:'python'}}}},(command,args,options)=>{{calls.push([command,args,options.stdio,options.env.PYTHONUTF8]);return {{status:0}}}});
process.stdout.write(JSON.stringify({{status,calls,resolves}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 0
    assert observed["calls"] == [["python", [script], "inherit", "1"]]
    assert observed["resolves"] == 1
    assert result.stderr == "Purser: Go MCP unavailable (Go binary missing); falling back to Python\n"


def test_go_mcp_unsupported_platform_falls_back_with_marker(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''Object.defineProperty(process,'platform',{{value:'plan9'}});
const locator=require({json.dumps(str(locator))}); let resolves=0; const calls=[];
const status=locator.main(['go-mcp',{json.dumps(script)}],()=>{{resolves++;return {{path:'python'}}}},(command,args)=>{{calls.push([command,args]);return {{status:0}}}});
process.stdout.write(JSON.stringify({{status,calls,resolves}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    assert json.loads(result.stdout) == {"status": 0, "calls": [["python", [script]]], "resolves": 1}
    assert result.stderr == "Purser: Go MCP unavailable (unsupported platform); falling back to Python\n"


def test_go_mcp_spawn_failure_falls_back_with_marker(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''const fs=require('node:fs'), locator=require({json.dumps(str(locator))}); const binary=locator.goBinaryPath();
fs.writeFileSync(binary,'stub'); let attempt=0; const calls=[];
const status=locator.main(['go-mcp',{json.dumps(script)}],()=>({{path:'python'}}),(command,args)=>{{calls.push([command,args]);return ++attempt===1?{{pid:0,status:null,error:{{code:'ENOENT',message:'spawn failed'}}}}:{{status:0}}}});
fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 0
    assert len(observed["calls"]) == 2
    assert observed["calls"][0][1] == ["mcp"]
    assert observed["calls"][1] == ["python", [script]]
    assert observed["calls"][0][0].startswith(str(locator.parent / "purser-hook-"))
    assert result.stderr == "Purser: Go MCP unavailable (Go process creation failed: spawn failed); falling back to Python\n"


def test_go_mcp_real_spawn_failure_falls_back_with_marker(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''const fs=require('node:fs'), cp=require('node:child_process'), locator=require({json.dumps(str(locator))});
const binary=locator.goBinaryPath(); fs.writeFileSync(binary,'stub'); let failedSpawn; const calls=[];
const status=locator.main(['go-mcp',{json.dumps(script)}],()=>({{path:'python'}}),(command,args,options)=>{{
  calls.push([command,args]); if(command==='python')return {{status:0}};
  fs.unlinkSync(binary); failedSpawn=cp.spawnSync(command,args,options); return failedSpawn;
}});
process.stdout.write(JSON.stringify({{status,calls,pid:failedSpawn.pid,childStatus:failedSpawn.status,error:failedSpawn.error?.code}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 0
    assert observed["pid"] == 0
    assert observed["childStatus"] is None
    assert observed["error"] == "ENOENT"
    assert len(observed["calls"]) == 2
    assert observed["calls"][0][1] == ["mcp"]
    assert observed["calls"][1] == ["python", [script]]
    assert result.stderr.startswith("Purser: Go MCP unavailable (Go process creation failed: spawnSync ")
    assert result.stderr.endswith(" ENOENT); falling back to Python\n")


def test_go_mcp_does_not_fallback_after_child_starts(scratch):
    locator = isolated_locator(scratch)
    script = str(scratch / "codex_exec_mcp.py")
    code = f'''const fs=require('node:fs'), locator=require({json.dumps(str(locator))}); const binary=locator.goBinaryPath();
fs.writeFileSync(binary,'stub'); let resolves=0; const calls=[];
const status=locator.main(['go-mcp',{json.dumps(script)}],()=>{{resolves++;return {{path:'python'}}}},(command,args)=>{{calls.push([command,args]);return {{pid:123,status:17}}}});
fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls,resolves}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 17
    assert len(observed["calls"]) == 1
    assert observed["calls"][0][1] == ["mcp"]
    assert observed["resolves"] == 0
    assert result.stderr == ""


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
