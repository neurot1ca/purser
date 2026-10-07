import json
import subprocess
import sys
import uuid
from pathlib import Path

import pytest

ORCH = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ORCH))
import codex


@pytest.fixture
def p66_workspace(tmp_path):
    repo = tmp_path / f"p66-repo-{uuid.uuid4().hex}"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "config", "user.email", "tests@example.invalid"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "config", "user.name", "Hook Tests"], cwd=repo, check=True, timeout=10)
    for index in range(3):
        (repo / f"file{index}.py").write_text("value = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "initial"], cwd=repo, check=True, timeout=10)
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    yield repo, tmp_path / "state", config


def _event(tool_name, tool_input, call_id="p66-call", cwd=None):
    return {
        "session_id": "p66-session", "cwd": str(cwd or Path.cwd()),
        "hook_event_name": "PreToolUse", "tool_name": tool_name,
        "tool_use_id": call_id, "tool_input": tool_input,
    }


def _routed_input():
    prompt = (
        "ORCH|task=p66-task|phase=implementation|route=normal|reason=new_task|"
        "basis=specific repository boundary|scope=file0.py,file1.py,file2.py\nDo the work."
    )
    return {"label": "p66 parity", "prompt": prompt}


def test_p66_case_identity_match_and_nonmatch(p66_workspace):
    repo, state, config = p66_workspace
    for tool_name in codex.CODEX_TOOL_NAMES | codex.CODEX_REPLY_TOOL_NAMES:
        matched = codex.pre_tool_use(
            _event(tool_name, [], cwd=repo), config=config, state_root=state, repo_root=repo,
        )
        assert matched["hookSpecificOutput"]["hookEventName"] == "PreToolUse"
    assert codex.pre_tool_use(
        _event("mcp__not-purser__codex", _routed_input(), cwd=repo), config=config,
        state_root=state, repo_root=repo,
    ) == {}


def test_p66_case_deny_shape(p66_workspace):
    repo, state, config = p66_workspace
    result = codex.pre_tool_use(
        _event(codex.CODEX_TOOL, [], cwd=repo), config=config, state_root=state, repo_root=repo,
    )
    assert result == {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "deny",
        "permissionDecisionReason": "MALFORMED_TOOL_INPUT: tool_input must be an object",
    }}


def test_p66_case_rewrite_and_display_shape(p66_workspace):
    repo, state, config = p66_workspace
    result = codex.pre_tool_use(
        _event(codex.CODEX_TOOL, _routed_input(), cwd=repo), config=config,
        state_root=state, repo_root=repo,
    )
    assert set(result) == {"hookSpecificOutput", "systemMessage"}
    specific = result["hookSpecificOutput"]
    assert specific["hookEventName"] == "PreToolUse"
    assert set(specific) == {"hookEventName", "updatedInput"}
    assert specific["updatedInput"]["model"] == config["routes"]["normal"]["model"]
    assert "Do the work." in specific["updatedInput"]["prompt"]
    assert "CODE_MAP=" in specific["updatedInput"]["prompt"]
    assert result["systemMessage"] == (
        f"Purser: route=normal -> {config['routes']['normal']['model']} / "
        f"{config['routes']['normal']['effort']}"
    )


def test_p66_case_additional_context_shape(p66_workspace, monkeypatch):
    repo, state, config = p66_workspace
    monkeypatch.setattr(codex, "_load_config", lambda _config: (_ for _ in ()).throw(RuntimeError("fixture failure")))
    result = codex.pre_tool_use(
        _event(codex.CODEX_TOOL, _routed_input(), cwd=repo), config=config,
        state_root=state, repo_root=repo,
    )
    specific = result["hookSpecificOutput"]
    assert specific["hookEventName"] == "PreToolUse"
    assert "additionalContext" in specific
    assert "permissionDecision" not in specific
    assert "NOT enforced" in specific["additionalContext"]
    assert "The bypass was recorded for PostToolUse." in specific["additionalContext"]
