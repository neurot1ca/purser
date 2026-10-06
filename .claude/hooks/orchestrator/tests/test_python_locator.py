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
    assert mcp["args"][1:] == ["go-mcp"]


def test_go_mcp_starts_binary_and_propagates_exit_without_resolving_python(scratch):
    locator = isolated_locator(scratch)
    code = f'''const fs=require('node:fs'), locator=require({json.dumps(str(locator))});
const binary=locator.goBinaryPath(); if(!binary)process.exit(77); fs.writeFileSync(binary,'stub');
const calls=[]; let resolves=0;
const status=locator.main(['go-mcp','--codex-bin','custom'],()=>{{resolves++;return {{path:'python'}}}},(command,args,options)=>{{calls.push({{command,args,stdio:options.stdio}});return {{pid:123,status:17}}}});
    fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls,resolves}}));'''
    result = node(code)
    if result.returncode == 77:
        pytest.skip("host platform has no packaged Go target")
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 17
    assert len(observed["calls"]) == 1
    assert observed["calls"][0]["command"].startswith(str(locator.parent / "purser-hook-"))
    assert observed["calls"][0]["args"] == ["mcp", "--codex-bin", "custom"]
    assert observed["calls"][0]["stdio"] == "inherit"
    assert observed["resolves"] == 0
    assert result.stderr == ""


def test_go_mcp_missing_binary_fails_closed_with_actionable_marker(scratch):
    locator = isolated_locator(scratch)
    code = f'''const locator=require({json.dumps(str(locator))}); let resolves=0; const writes=[], out=[];
const write=process.stderr.write; process.stderr.write=(value)=>{{writes.push(value);return true}};
const stdout=process.stdout.write; process.stdout.write=(value)=>{{out.push(value);return true}};
const status=locator.main(['go-mcp'],()=>{{resolves++;return {{path:'python'}}}},()=>{{throw Error('unexpected spawn')}});
process.stderr.write=write; process.stdout.write=stdout; process.stdout.write(JSON.stringify({{status,resolves,stderr:writes.join(''),out:out.join(''),platform:process.platform,arch:process.arch,expected:locator.goBinaryPath()}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 1
    assert observed["resolves"] == 0
    assert observed["out"] == ""
    assert observed["stderr"] == f"Purser: Go MCP unavailable on {observed['platform']}/{observed['arch']}; expected binary {observed['expected']} (Go binary missing)\n"


def test_go_mcp_unsupported_platform_fails_closed_with_actionable_marker(scratch):
    locator = isolated_locator(scratch)
    code = f'''Object.defineProperty(process,'platform',{{value:'plan9'}}); Object.defineProperty(process,'arch',{{value:'oddarch'}});
const locator=require({json.dumps(str(locator))}); let resolves=0; const writes=[], out=[];
const write=process.stderr.write, stdout=process.stdout.write; process.stderr.write=(value)=>{{writes.push(value);return true}}; process.stdout.write=(value)=>{{out.push(value);return true}};
const status=locator.main(['go-mcp'],()=>{{resolves++;return {{path:'python'}}}},()=>{{throw Error('unexpected spawn')}});
process.stderr.write=write; process.stdout.write=stdout; process.stdout.write(JSON.stringify({{status,resolves,stderr:writes.join(''),out:out.join(''),expected:require('node:path').join({json.dumps(str(locator.parent))},'..','bin','purser-hook-plan9-oddarch')}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 1
    assert observed["resolves"] == 0
    assert observed["out"] == ""
    assert observed["stderr"] == f"Purser: Go MCP unavailable on plan9/oddarch; expected binary {observed['expected']} (unsupported platform)\n"


def test_go_mcp_creation_failure_fails_closed_with_actionable_marker_and_eacces_retry(scratch):
    locator = isolated_locator(scratch)
    code = f'''Object.defineProperty(process,'platform',{{value:'linux'}});
const fs=require('node:fs'), locator=require({json.dumps(str(locator))}); const binary=locator.goBinaryPath();
fs.writeFileSync(binary,'stub'); let attempt=0, resolves=0, chmods=0; const writes=[], out=[];
fs.chmodSync=()=>{{chmods++}};
const write=process.stderr.write, stdout=process.stdout.write; process.stderr.write=(value)=>{{writes.push(value);return true}}; process.stdout.write=(value)=>{{out.push(value);return true}};
const status=locator.main(['go-mcp'],()=>{{resolves++;return {{path:'python'}}}},()=>{{attempt++;return attempt===1?{{pid:0,status:null,error:{{code:'EACCES',message:'permission denied'}}}}:{{pid:0,status:null,error:{{code:'ENOENT',message:'spawn failed'}}}}}});
process.stderr.write=write; process.stdout.write=stdout; fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,attempt,resolves,chmods,stderr:writes.join(''),out:out.join(''),platform:process.platform,arch:process.arch,expected:binary}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 1
    assert observed["attempt"] == 2
    assert observed["resolves"] == 0
    assert observed["chmods"] == 1
    assert observed["out"] == ""
    assert observed["stderr"] == f"Purser: Go MCP unavailable on {observed['platform']}/{observed['arch']}; expected binary {observed['expected']} (Go process creation failed: spawn failed)\n"


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
