import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[4]
FIXTURE = ROOT / ".claude/hooks/orchestrator/tests/fixtures/go_policy/probe.json"
SOURCE = ROOT / ".claude/hooks/orchestrator/codex_exec_mcp.py"
ORACLE = r"""
import json, sys
from pathlib import Path
from codex_exec_mcp import _agent_command, _agent_event, _launch_failure_evidence
case = json.load(sys.stdin)
if case['kind'] == 'command':
    c = case['input']
    result = _agent_command('codex', c['prefix'], 'codex', c['cwd'], c['model'] or None, [], 'read-only', None, Path(c['output']), 'Reply with OK.')
else:
    c = case['input']
    events = []
    for line in c['stdout'].splitlines():
        try: event = json.loads(line)
        except (ValueError, UnicodeError): continue
        if isinstance(event, dict): events.append(event)
    if c['timed_out']:
        result = {'ok': False, 'failure': {'type': 'NO_RESPONSE', 'retryAfterSeconds': None, 'noWorkProof': False}}
    else:
        parsed = [_agent_event('codex', e) for e in events]
        completed = any(e.get('type') == 'turn.completed' for e in events)
        failures = [failure for _, _, failure, _ in parsed if failure]
        if c['returncode'] == 0 and completed and not failures: result = {'ok': True}
        else:
            evidence = _launch_failure_evidence('codex', events, stderr=c['stderr'], stdout_had_output=bool(c['stdout'].strip()), output_had_data=c['output_data'], process_started=c['started'])
            result = {'ok': False, 'failure': evidence}
print(json.dumps(result, ensure_ascii=False))
"""


def _oracle(kind, case):
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        [sys.executable, "-B", "-c", ORACLE], input=json.dumps({"kind": kind, "input": case}),
        text=True, capture_output=True, cwd=SOURCE.parent, env=env, check=True,
    )
    return json.loads(result.stdout)


def test_p38_python_oracle_golden_fixture():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    for command in fixture["commands"]:
        case = {key: command[key] for key in ("prefix", "cwd", "model", "output")}
        assert _oracle("command", case) == command["expected"]
    for item in fixture["cases"]:
        case = {"stdout": item["stdout"], "stderr": item["stderr"], "output_data": item["output_data"], "started": item["started"], "returncode": item["returncode"], "timed_out": item["timed_out"]}
        assert _oracle("result", case) == item["expected"], item["name"]
