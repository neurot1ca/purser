import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

PACKAGE_ROOT = Path(__file__).parents[1]
sys.path.insert(0, str(PACKAGE_ROOT))

import codex
import telemetry_schema


@pytest.fixture
def pre_context(monkeypatch):
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "scope-count" / uuid.uuid4().hex
    repo, state = root / "repo", root / "state"
    repo.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10)
    (repo / "first.py").write_text("value = 1\n", encoding="utf-8")
    (repo / "second.py").write_text("value = 2\n", encoding="utf-8")
    config = json.loads((PACKAGE_ROOT / "config.json").read_text(encoding="utf-8"))
    observations = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observations.append((args, kwargs)))
    try:
        yield repo, state, config, observations
    finally:
        shutil.rmtree(root, ignore_errors=True)


@pytest.mark.parametrize(
    ("phase", "route", "scope", "expected"),
    [("implementation", "normal", "first.py,second.py", 2), ("operation", "operation", None, 0)],
)
def test_pre_telemetry_records_scope_count_only(pre_context, phase, route, scope, expected):
    repo, state, config, observations = pre_context
    fields = ["task=scope-count", f"phase={phase}", f"route={route}"]
    if route in {"normal", "consequential", "escalated"}:
        fields.extend(["reason=new_task", "basis=specific repository boundary"])
    if scope is not None:
        fields.append(f"scope={scope}")
    prompt = "ORCH|" + "|".join(fields) + "\nDo the work."
    event = {
        "session_id": "scope-count-session", "hook_event_name": "PreToolUse",
        "tool_name": codex.CODEX_TOOL, "tool_use_id": "scope-count-call",
        "tool_input": {"prompt": prompt, "label": "scope count"},
    }

    codex.pre_tool_use(event, config=config, state_root=state, repo_root=repo)

    event_payloads = []
    for args, payload in observations:
        if args[1] not in {"TASK_ROUTE_SELECTED", "IMPLEMENTER_STARTED"}:
            continue
        assert payload["scope_path_count"] == expected
        assert "scope" not in payload
        event_payloads.append(args[1])
        built = telemetry_schema.build_event(
            args[1], task_id="scope-count", payload={"scope_path_count": expected}
        )
        assert telemetry_schema.valid_event(built)
    assert event_payloads == ["TASK_ROUTE_SELECTED", "IMPLEMENTER_STARTED"]
