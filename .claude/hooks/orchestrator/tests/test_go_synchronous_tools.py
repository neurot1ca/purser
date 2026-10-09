"""Capture the Python synchronous Codex tool contract for Go private-op replay."""
import json
import os
from pathlib import Path
import sys
import threading
import time

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "cmd" / "purser-hook" / "testdata" / "synchronous_python.json"
sys.path.insert(0, str(ORCH))
import codex_exec_mcp

FAKE_CODEX = r'''import json, os, pathlib, sys, time
mode = os.environ.get("PURSER_SYNC_ORACLE_MODE", "success")
thread = os.environ.get("PURSER_SYNC_ORACLE_THREAD", "thread-new")
args = sys.argv
output = pathlib.Path(args[args.index("-o") + 1])
if os.environ.get("PURSER_SYNC_ORACLE_STARTED"):
    pathlib.Path(os.environ["PURSER_SYNC_ORACLE_STARTED"]).write_text("started", encoding="utf-8")
if mode == "cancel":
    time.sleep(60)
print(json.dumps({"type": "thread.started", "thread_id": thread}), flush=True)
if mode == "failure":
    print(json.dumps({"type": "turn.failed", "message": "oracle worker failure"}), flush=True)
    sys.exit(1)
content = "oracle final text"
if mode == "unicode":
    content = "Türkçe İstanbul: \u2028 separator \u2029 separator \b backspace \f formfeed literal \\u2028"
output.write_text(content, encoding="utf-8")
print(json.dumps({"type": "item.completed", "item": {"type": "agent_message", "text": content}}), flush=True)
print(json.dumps({"type": "turn.completed", "usage": {"input_tokens": 3, "output_tokens": 2}}), flush=True)
'''

CASES = (
    ("new_success", "codex", {"prompt": "new prompt"}, "success", "thread-new", None),
    ("reply_success", "codex-reply", {"threadId": "thread-recorded", "prompt": "reply prompt"},
     "success", "thread-recorded", {"thread_id": "thread-recorded", "cwd": None, "model": None,
                                    "config_args": ["model_reasoning_effort=\"high\"", "tool_output_token_limit=123"],
                                    "sandbox": None, "agent": "codex"}),
    ("worker_failure", "codex", {"prompt": "fail"}, "failure", "thread-failed", None),
    ("unicode_content", "codex", {"prompt": "unicode"}, "unicode", "thread-unicode", None),
    ("unknown_thread", "codex-reply", {"threadId": "missing-thread", "prompt": "reply"},
     "success", "thread-recorded", None),
    ("cancelled", "codex", {"prompt": "cancel"}, "cancel", "thread-cancelled", None),
)


def _capture(tmp_path):
    captured = []
    for name, tool, arguments, mode, thread, record in CASES:
        state = tmp_path / name
        state.mkdir()
        repo = tmp_path / (name + "-repo")
        repo.mkdir()
        if record is not None:
            saved = dict(record, cwd=str(repo))
            codex_exec_mcp._save_record(state / "codex_exec_mcp" / "threads", saved)
        marker = state / "child-started"
        old = {key: os.environ.get(key) for key in ("PURSER_STATE_DIR", "PURSER_SYNC_ORACLE_MODE",
                                                      "PURSER_SYNC_ORACLE_THREAD", "PURSER_SYNC_ORACLE_STARTED")}
        os.environ.update({"PURSER_STATE_DIR": str(state), "PURSER_SYNC_ORACLE_MODE": mode,
                           "PURSER_SYNC_ORACLE_THREAD": thread, "PURSER_SYNC_ORACLE_STARTED": str(marker)})
        try:
            server = codex_exec_mcp.Server(codex_bin=json.dumps([sys.executable, "-c", FAKE_CODEX]), agent="codex")
            sent = []
            server.send = sent.append
            request_id = "sync-" + name
            args = dict(arguments)
            if tool == "codex":
                args["cwd"] = str(repo)
            worker = threading.Thread(target=server._worker,
                                      args=(request_id, {"name": tool, "arguments": args}), daemon=True)
            worker.start()
            if name == "cancelled":
                deadline = time.monotonic() + 5
                process = None
                while time.monotonic() < deadline:
                    with server.jobs_lock:
                        process = server.jobs.get(request_id)
                    if process is not None:
                        break
                    time.sleep(0.01)
                assert process is not None, "sync child did not start"
                with server.jobs_lock:
                    server.cancelled.add(request_id)
                codex_exec_mcp._stop_process(process)
            worker.join(10)
            assert not worker.is_alive(), name
            if name == "cancelled":
                assert sent == [], sent
                result = None
            else:
                assert len(sent) == 1, (name, sent)
                result = sent[0]["result"]
            captured.append({"name": name, "tool": tool, "arguments": args, "mode": mode,
                             "threadId": thread, "threadRecord": (dict(record, cwd=str(repo)) if record else None),
                             "result": result})
        finally:
            for key, value in old.items():
                if value is None:
                    os.environ.pop(key, None)
                else:
                    os.environ[key] = value
    def normalize(value):
        if isinstance(value, dict):
            return {key: normalize(item) for key, item in value.items()}
        if isinstance(value, list):
            return [normalize(item) for item in value]
        if isinstance(value, str):
            value = value.replace(str(sys.executable), "__PYTHON__").replace(
                str(tmp_path).replace("\\", "\\\\"), "__ROOT__").replace(str(tmp_path), "__ROOT__")
            return value.replace("\\", "/") if "__ROOT__" in value else value
        return value

    return normalize({"oracle": "codex_exec_mcp.Server._worker->_run_tool_once", "python": sys.executable,
                      "fakeCodex": FAKE_CODEX, "cases": captured})


def test_go_synchronous_python_fixture(tmp_path):
    fixture = _capture(tmp_path)
    encoded = json.dumps(fixture, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n"
    assert len(encoded.encode("utf-8")) < 30_000
    if os.environ.get("PURSER_REGEN_FIXTURES") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(encoded, encoding="utf-8", newline="\n")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set PURSER_REGEN_FIXTURES=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
