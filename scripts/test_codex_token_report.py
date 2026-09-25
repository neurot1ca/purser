import importlib.util
import json
from pathlib import Path
import shutil
import uuid

import pytest


spec = importlib.util.spec_from_file_location("codex_token_report", Path(__file__).with_name("codex_token_report.py"))
reporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reporter)


def write(path, items):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(json.dumps(item) for item in items) + "\n", encoding="utf-8")


@pytest.fixture
def area():
    path = Path(__file__).parents[1] / ".tmp" / ("codex-token-report-" + uuid.uuid4().hex)
    path.mkdir(parents=True)
    try:
        yield path
    finally:
        shutil.rmtree(path)


def rollout(thread, date):
    def call(call_id, command, output):
        return [
            {"type": "response_item", "payload": {"type": "custom_tool_call", "call_id": call_id, "input": command}},
            {"type": "response_item", "payload": {"type": "custom_tool_call_output", "call_id": call_id,
                                                    "output": [{"type": "input_text", "text": output}]}},
        ]
    return ([{"type": "session_meta", "payload": {"id": thread, "originator": "codex_exec", "timestamp": date}},
             {"type": "event_msg", "payload": {"type": "token_count", "info": {"total_token_usage": {
                 "input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1, "reasoning_output_tokens": 0}}}}]
            + call("r", 'tools.exec_command({cmd:"rg -n x files"})', "read")
            + call("t", 'tools.exec_command({cmd:"pytest -q"})', "test")
            + call("o", 'tools.exec_command({cmd:"git status"})', "other")
            + [{"type": "event_msg", "payload": {"type": "token_count", "info": {"total_token_usage": {
                "input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 10, "reasoning_output_tokens": 4}}}}])


def test_join_period_and_command_classes(area):
    sessions = area / "sessions"
    events = area / "events"
    for thread, date in (("a", "2026-09-24T23:00:00Z"), ("b", "2026-09-25T01:00:00Z")):
        write(sessions / "2026/09/25" / f"rollout-{thread}.jsonl", rollout(thread, date))
    write(events / "2026-09/session_1.jsonl", [
        {"event_type": "TASK_ROUTE_SELECTED", "session_id": "s", "task_id": "task-a", "payload": {"route": "normal"}},
        {"event_type": "IMPLEMENTER_FINISHED", "session_id": "s", "task_id": "task-a", "payload": {"thread_id": "a", "duration_ms": 1000}},
        {"event_type": "IMPLEMENTER_FINISHED", "session_id": "s", "task_id": "task-b", "payload": {"thread_id": "b", "route": "subtle", "duration_ms": 2000}},
        {"event_type": "IMPLEMENTER_FINISHED", "session_id": "s", "task_id": "task-b", "payload": {"thread_id": "b", "route": "subtle", "duration_ms": 500}},
    ])
    result = reporter.report(sessions, events, reporter.timestamp("2026-09-25T00:00:00Z"))
    assert [(job["task"], job["route"], job["period"]) for job in result["jobs"]] == [
        ("task-a", "normal", "before"), ("task-b", "subtle", "after")]
    assert result["jobs"][0]["uncached"] == 20
    assert (result["jobs"][0]["read_search_chars"], result["jobs"][0]["test_chars"],
            result["jobs"][0]["other_chars"]) == (4, 4, 5)
    assert result["summary"]["before"]["jobs"] == result["summary"]["after"]["jobs"] == 1
    assert result["summary"]["after"]["duration_ms"] == 2500
