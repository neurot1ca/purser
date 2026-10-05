import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
if str(ORCH) not in sys.path:
    sys.path.insert(0, str(ORCH))

import codex_exec_mcp


FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_agents" / "agents.json"


def _command_case(agent, tool):
    model = "google/gemini-test" if agent == "opencode" else "gemini-test"
    thread_id = "thread-123" if tool == "codex-reply" else None
    args = codex_exec_mcp._agent_command(
        agent, ["agent-bin", "--prefix"], tool, "C:/repo", model,
        ['model_reasoning_effort="high"'], None, thread_id,
        Path("C:/state/output"), "prompt text",
    )
    return {"agent": agent, "kind": tool, "argv": args}


def _unsupported_case(agent, kind):
    model = "google/gemini-test" if agent == "opencode" else "gemini-test"
    sandbox = "read-only" if kind == "sandbox" else None
    config = ["unsupported_option=true"] if kind == "config" else ['model_reasoning_effort="high"']
    try:
        codex_exec_mcp._agent_command(
            agent, ["agent-bin"], "codex", "C:/repo", model, config, sandbox,
            None, Path("C:/state/output"), "prompt text",
        )
    except ValueError as exc:
        error = str(exc)
    else:
        raise AssertionError(f"{agent} {kind} unexpectedly accepted")
    return {"agent": agent, "kind": "unsupported_" + kind, "error": error}


def _identity_mismatch(agent):
    other_agent = "agy" if agent == "opencode" else "opencode"
    server = codex_exec_mcp.Server.__new__(codex_exec_mcp.Server)
    server.agent = agent
    server.state_dir = Path("unused")
    # The actual record loader is exercised using a temporary state directory by
    # the fixture builder below; this helper receives that directory separately.
    return server, other_agent


def _oracle(tmp_path):
    cases = []
    for agent in ("opencode", "agy"):
        cases.extend((_command_case(agent, "codex"), _command_case(agent, "codex-reply")))
        server, other_agent = _identity_mismatch(agent)
        server.state_dir = tmp_path / agent
        record = {"thread_id": "thread-123", "cwd": "C:/repo", "config_args": [], "agent": other_agent}
        codex_exec_mcp._save_record(server.state_dir, record)
        mismatch = server._run_tool_once(
            "codex-reply", {"agent": agent, "threadId": "thread-123", "prompt": "continue"}, "fixture"
        )
        cases.append({"agent": agent, "kind": "resume_identity_mismatch", "error": json.loads(mismatch["content"][0]["text"])["error"]})
        cases.append(_unsupported_case(agent, "sandbox"))
        cases.append(_unsupported_case(agent, "config"))
    return {"oracle": "codex_exec_mcp._agent_command and Server._run_tool_once resume identity", "cases": cases}


def test_go_jobs_agents_python_oracle(tmp_path):
    fixture = _oracle(tmp_path)
    if os.environ.get("PURSER_REGEN_GO_JOBS_AGENTS") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    recorded = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert recorded == fixture
