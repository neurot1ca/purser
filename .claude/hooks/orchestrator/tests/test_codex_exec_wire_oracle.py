"""Byte-exact Python oracle for the Codex MCP bridge wire contract."""

import json
import os
from pathlib import Path

import codex_exec_mcp


FIXTURES = Path(__file__).parent / "fixtures" / "codex_exec_wire"

CASES = [
    {"name": "schema_codex", "input": {"function": "schema", "tool": "codex"}},
    {"name": "schema_codex_reply_alias", "input": {"function": "schema", "tool": "codex-reply"}},
    {"name": "schema_codex_start", "input": {"function": "schema", "tool": "codex_start"}},
    {"name": "schema_codex_result", "input": {"function": "schema", "tool": "codex_result"}},
    {"name": "schema_codex_cancel", "input": {"function": "schema", "tool": "codex_cancel"}},
    {"name": "schema_codex_apply", "input": {"function": "schema", "tool": "codex_apply"}},
    {"name": "tool_result_success", "input": {"function": "_tool_result", "thread_id": "thread-123",
                                                    "content": "completed"}},
    {"name": "tool_result_error", "input": {"function": "_tool_result", "thread_id": None,
                                                 "error": "request failed"}},
    {"name": "tool_result_alias_unicode", "input": {"function": "_tool_result", "thread_id": "iş parçacığı 🧵",
                                                         "content": "Merhaba, dünya! 🚀"}},
    {"name": "tool_result_usage_absent", "input": {"function": "_tool_result", "thread_id": "thread-usage",
                                                       "content": "done", "usage": {}}},
    {"name": "tool_result_usage_present", "input": {"function": "_tool_result", "thread_id": "thread-usage",
                                                        "content": "done", "usage": {"input_tokens": 12,
                                                                                      "output_tokens": 3}}},
    {"name": "job_result_success", "input": {"function": "_job_result", "value": {"jobId": "job-123",
                                                                                       "state": "SUCCEEDED",
                                                                                       "content": "done"}}},
    {"name": "job_result_error", "input": {"function": "_job_result", "value": {"state": "FAILED",
                                                                                     "errorCode": "INVALID_ARGUMENT",
                                                                                     "message": "bad request"}}},
    {"name": "job_result_alias_unicode", "input": {"function": "_job_result", "value": {"conversationId": "oturum-ıd 🪁",
                                                                                             "threadId": "işlem-çubuğu",
                                                                                             "message": "Başarılı ✅"}}},
]


def _expected_text(case):
    value = case["input"]
    if value["function"] == "schema":
        schema = next(item for item in codex_exec_mcp.TOOL_SCHEMAS
                      if item["name"] == value["tool"])
        return json.dumps(schema, ensure_ascii=False)
    if value["function"] == "_tool_result":
        result = codex_exec_mcp._tool_result(
            value.get("thread_id"), content=value.get("content"), error=value.get("error"),
            usage=value.get("usage"))
    elif value["function"] == "_job_result":
        result = codex_exec_mcp._job_result(value["value"])
    else:
        raise AssertionError(f"unknown fixture function: {value['function']}")
    return result["content"][0]["text"]


def test_codex_exec_wire_oracle():
    expected = [{**case, "expected_text": _expected_text(case)} for case in CASES]
    if os.environ.get("PURSER_REGEN_WIRE_FIXTURES") == "1":
        FIXTURES.mkdir(parents=True, exist_ok=True)
        for case in expected:
            path = FIXTURES / f"{case['name']}.json"
            path.write_text(json.dumps(case, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    assert FIXTURES.is_dir(), (
        f"missing fixtures: {FIXTURES}; set PURSER_REGEN_WIRE_FIXTURES=1 to capture"
    )
    fixture_files = sorted(FIXTURES.glob("*.json"))
    assert [path.name for path in fixture_files] == sorted(f"{case['name']}.json" for case in expected)
    for case in expected:
        path = FIXTURES / f"{case['name']}.json"
        actual = json.loads(path.read_text(encoding="utf-8"))
        assert actual == case, f"wire fixture differs from Python oracle: {path.name}"

    assert {item["name"] for item in codex_exec_mcp.TOOL_SCHEMAS} == {
        "codex", "codex-reply", "codex_start", "codex_result", "codex_cancel", "codex_apply",
    }
