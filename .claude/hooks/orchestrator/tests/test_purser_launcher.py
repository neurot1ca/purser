import json
from pathlib import Path
import shutil
import subprocess
import uuid

import pytest


ROOT = Path(__file__).parents[4]
LAUNCHER = ROOT / "bin" / "purser-launcher.js"


@pytest.fixture
def scratch():
    root = ROOT / ".tmp" / f"purser-launcher-{uuid.uuid4().hex}"
    root.mkdir(parents=True, exist_ok=True)
    yield root
    shutil.rmtree(root, ignore_errors=True)


def node(code):
    return subprocess.run(["node", "-e", code], capture_output=True, text=True, check=False)


def isolated_launcher(scratch):
    bindir = scratch / "bin"
    bindir.mkdir()
    launcher = bindir / "purser-launcher.js"
    shutil.copy2(LAUNCHER, launcher)
    return launcher


def test_go_mcp_starts_binary_and_propagates_exit(scratch):
    launcher = isolated_launcher(scratch)
    code = f'''const fs=require('node:fs'), launcher=require({json.dumps(str(launcher))});
const binary=launcher.goBinaryPath(); if(!binary)process.exit(77); fs.writeFileSync(binary,'stub');
const calls=[]; const status=launcher.main(['go-mcp','--codex-bin','custom'],(command,args,options)=>{{calls.push({{command,args,stdio:options.stdio}});return {{pid:123,status:17}}}});
fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls}}));'''
    result = node(code)
    if result.returncode == 77:
        pytest.skip("host platform has no packaged Go target")
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 17
    assert len(observed["calls"]) == 1
    assert observed["calls"][0]["command"].startswith(str(launcher.parent / "purser-hook-"))
    assert observed["calls"][0]["args"] == ["mcp", "--codex-bin", "custom"]
    assert observed["calls"][0]["stdio"] == "inherit"
    assert result.stderr == ""


def test_go_mcp_missing_binary_reports_expected_path(scratch):
    launcher = isolated_launcher(scratch)
    code = f'''const launcher=require({json.dumps(str(launcher))}); const writes=[];
const write=process.stderr.write; process.stderr.write=(value)=>{{writes.push(value);return true}};
const status=launcher.main(['go-mcp'],()=>{{throw Error('unexpected spawn')}});
process.stderr.write=write; process.stdout.write(JSON.stringify({{status,stderr:writes.join(''),platform:process.platform,arch:process.arch,expected:launcher.goBinaryPath()}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    observed = json.loads(result.stdout)
    assert observed["status"] == 1
    assert observed["stderr"] == f"Purser: Go MCP unavailable on {observed['platform']}/{observed['arch']}; expected binary {observed['expected']} (Go binary missing)\n"


def test_go_mcp_retries_after_eacces(scratch):
    launcher = isolated_launcher(scratch)
    code = f'''Object.defineProperty(process,'platform',{{value:'linux'}});
const fs=require('node:fs'), launcher=require({json.dumps(str(launcher))}); const binary=launcher.goBinaryPath();
fs.writeFileSync(binary,'stub'); let calls=0, chmods=0; fs.chmodSync=()=>{{chmods++}};
const status=launcher.main(['go-mcp'],()=>{{calls++;return calls===1?{{pid:0,status:null,error:{{code:'EACCES',message:'denied'}}}}:{{pid:123,status:0}}}});
fs.unlinkSync(binary); process.stdout.write(JSON.stringify({{status,calls,chmods}}));'''
    result = node(code)
    assert result.returncode == 0, result.stdout + result.stderr
    assert json.loads(result.stdout) == {"status": 0, "calls": 2, "chmods": 1}


def test_invalid_mode_reports_launcher_error():
    result = subprocess.run(["node", str(LAUNCHER), "hook"], capture_output=True, text=True, check=False)
    assert result.returncode == 1
    assert result.stderr == "Purser: invalid launcher invocation\n"
