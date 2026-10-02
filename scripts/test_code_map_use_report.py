import importlib.util
import json
from pathlib import Path
import shutil
import uuid


spec = importlib.util.spec_from_file_location("code_map_use_report", Path(__file__).with_name("code_map_use_report.py"))
reporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reporter)


def write(path, values):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(json.dumps(value) for value in values) + "\n", encoding="utf-8")


def worker_trace(thread, map_path, command=None, output="", status="FRESH"):
    values = [
        {"type": "session_meta", "payload": {"id": thread, "originator": "codex_exec"}},
        {"type": "response_item", "payload": {"type": "message", "role": "user",
         "content": [{"type": "input_text", "text": f"Use map\nCODE_MAP={map_path} STATUS={status}"}]}},
    ]
    if command is not None:
        values.extend([
            {"type": "response_item", "payload": {"type": "custom_tool_call", "call_id": "read", "input": command}},
            {"type": "response_item", "payload": {"type": "custom_tool_call_output", "call_id": "read",
             "output": [{"type": "input_text", "text": output}]}},
        ])
    return values


def start(session, task, provider, ts="2026-09-25T01:00:00Z"):
    return {"event_type": "IMPLEMENTER_STARTED", "ts_utc": ts, "session_id": session,
            "task_id": task, "agent_type": "codex-main",
            "payload": {"phase": "implementation", "route": "normal",
                        "implementer": provider}}


def finish(session, task, thread, offered=None):
    return {"event_type": "IMPLEMENTER_FINISHED", "session_id": session, "task_id": task,
            "payload": {"phase": "implementation", "thread_id": thread, "implementer": "codex",
                        "has_code_map": offered}}


def test_code_map_use_rows_keep_missing_evidence_unknown():
    root = Path(__file__).parents[1] / ".tmp" / ("code-map-use-" + uuid.uuid4().hex)
    sessions, events = root / "sessions", root / "events"
    map_path = "C:/workspace/.claude/hooks/orchestrator/code_map.py"
    write(sessions / "rollout-read.jsonl",
          worker_trace("read", map_path, f'rg -n "symbol" "{map_path}"', "line", status="STALE"))
    write(sessions / "rollout-no-read.jsonl", worker_trace("no-read", map_path, status="FRESH"))
    write(events / "session_jobs.jsonl", [
        start("s", "read", "codex"), finish("s", "read", "read", offered=True),
        start("s", "no-read", "codex"), finish("s", "no-read", "no-read", offered=True),
        start("s", "missing", "codex"), finish("s", "missing", "missing", offered=True),
    ])
    try:
        result = reporter.report(sessions, events, reporter.timestamp("2026-09-25T00:00:00Z"))
    finally:
        shutil.rmtree(root)

    rows = {row["task_id"]: row for row in result["code_map_use"]}
    assert rows["read"] == {"session_id": "s", "task_id": "read", "provider": "codex", "offered": True,
                            "observed_open_count": 1, "measured_output_bytes": 4, "exploration_read_count": 1}
    assert rows["no-read"]["offered"] is True
    assert rows["no-read"]["observed_open_count"] == 0
    assert rows["no-read"]["measured_output_bytes"] is None
    assert rows["no-read"]["exploration_read_count"] == 0
    assert rows["missing"]["offered"] is True
    assert rows["missing"]["observed_open_count"] is None
    assert rows["missing"]["measured_output_bytes"] is None
    assert rows["missing"]["exploration_read_count"] is None
    assert "not token savings" in reporter.markdown(result)


def test_report_distinguishes_whole_scoped_absent_and_unobservable_traces():
    root = Path(__file__).parents[1] / ".tmp" / ("code-map-report-" + uuid.uuid4().hex)
    sessions, events = root / "sessions", root / "events"
    map_path = "C:/workspace/.claude/hooks/orchestrator/code_map.py"
    write(sessions / "2026/09/25/rollout-whole.jsonl",
          worker_trace("whole", map_path, f'Get-Content "{map_path}"', "whole map"))
    write(sessions / "2026/09/25/rollout-scoped.jsonl",
          worker_trace("scoped", map_path, f'rg -n "symbol" "{map_path}"', "line 1"))
    write(sessions / "2026/09/25/rollout-absent.jsonl", worker_trace("absent", map_path))
    (sessions / "2026/09/25").mkdir(parents=True, exist_ok=True)
    (sessions / "2026/09/25/rollout-bad.jsonl").write_text("not json\n", encoding="utf-8")
    write(events / "2026-09/session_jobs.jsonl", [
        start("s", "whole", "codex"), finish("s", "whole", "whole"),
        start("s", "scoped", "codex"), finish("s", "scoped", "scoped"),
        start("s", "absent", "codex"), finish("s", "absent", "absent"),
        start("s", "missing", "codex"), finish("s", "missing", "missing"),
        start("s", "bad", "codex"), finish("s", "bad", "bad"),
        start("s", "opencode", "opencode"), start("s", "agy", "agy"),
    ])

    try:
        result = reporter.report(sessions, events, reporter.timestamp("2026-09-25T00:00:00Z"))
    finally:
        shutil.rmtree(root)

    codex = result["providers"]["codex"]
    assert codex["jobs"] == 5
    assert codex["eligible_jobs"] == 3
    assert codex["map_open_jobs"] == 2
    assert codex["whole_map_reads"] == 1
    assert codex["targeted_map_reads"] == 1
    assert codex["bytes_read"] == len("whole map".encode()) + len("line 1".encode())
    assert codex["eligible_no_open_jobs"] == 1
    assert codex["unobservable_jobs"] == 2
    assert codex["ineligible_jobs"] == 0
    assert result["providers"]["opencode"]["jobs"] == 1
    assert result["providers"]["opencode"]["unobservable_jobs"] == 1
    assert result["providers"]["opencode"]["map_open_jobs"] is None
    assert result["providers"]["agy"]["jobs"] == 1
    assert result["providers"]["agy"]["unobservable_jobs"] == 1
    assert result["providers"]["agy"]["eligible_jobs"] is None
    assert "unobservable" in reporter.markdown(result).lower()


def test_nested_functions_exec_read_counts_map_open():
    map_path = r"C:\workspace\.claude\hooks\orchestrator\code_map.py"
    values = worker_trace("nested", map_path)
    values.extend([
        {"type": "response_item", "payload": {"type": "function_call", "call_id": "nested-read",
         "arguments": {"script": f'const result = await tools.exec_command({{cmd: {json.dumps("Get-Content " + map_path)}}});'}}},
        {"type": "response_item", "payload": {"type": "function_call_output", "call_id": "nested-read",
         "output": [{"type": "input_text", "text": "map contents"}]}},
    ])

    root = Path(__file__).parents[1] / ".tmp" / ("nested-read-" + uuid.uuid4().hex)
    path = root / "rollout-nested.jsonl"
    try:
        write(path, values)
        observation = reporter.trace(path)
    finally:
        shutil.rmtree(root)

    assert observation["operations"] == [{"kind": "whole", "bytes": len("map contents".encode())}]


def test_nested_functions_exec_without_read_verb_does_not_count_map_open():
    map_path = r"C:\workspace\.claude\hooks\orchestrator\code_map.py"
    values = worker_trace("nested-no-read", map_path)
    values.extend([
        {"type": "response_item", "payload": {"type": "function_call", "call_id": "nested-no-read",
         "arguments": {"script": f'await tools.exec_command({{cmd: {json.dumps("echo " + map_path)}}});'}}},
        {"type": "response_item", "payload": {"type": "function_call_output", "call_id": "nested-no-read",
         "output": [{"type": "input_text", "text": "printed path"}]}},
    ])

    root = Path(__file__).parents[1] / ".tmp" / ("nested-no-read-" + uuid.uuid4().hex)
    path = root / "rollout-nested-no-read.jsonl"
    try:
        write(path, values)
        observation = reporter.trace(path)
    finally:
        shutil.rmtree(root)

    assert observation["operations"] == []
