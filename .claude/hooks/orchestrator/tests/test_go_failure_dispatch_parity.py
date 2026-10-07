"""Process parity for the unified Go PostToolUseFailure dispatcher."""
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURES = ORCH / "tests" / "fixtures" / "go_policy"
sys.path.insert(0, str(ORCH))
import ledger


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _key(event):
    return hashlib.sha256((event["session_id"] + "\0" + event["tool_use_id"]).encode()).hexdigest()


def _snapshot(root):
    result = {}
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        # The OS lock file can remain on one platform after release. It is
        # coordination machinery, not durable state to compare for parity.
        if path.name == ".append.lock":
            continue
        raw = path.read_text(encoding="utf-8")
        raw = raw.replace(str(root), "__STATE__").replace(str(ROOT), "__REPO__")
        try:
            value = json.loads(raw)
        except json.JSONDecodeError:
            result[str(path.relative_to(root))] = raw
            continue

        def normalize(item, key=""):
            if isinstance(item, dict):
                return {k: ("__TIME__" if k == "at" or k.endswith(("_at", "_time", "timestamp")) else
                            "__OFFSET__" if k == "index_offset" else
                            "__DERIVED__" if k in {"checksum", "policy_fingerprint", "policy_hashes", "ts_utc"} else
                            "__ID__" if k in {"event_id", "observation_id"} else normalize(v, k))
                        for k, v in item.items()}
            if isinstance(item, list):
                return [normalize(value) for value in item]
            if isinstance(item, str):
                item = item.replace(str(root), "__STATE__").replace(str(ROOT), "__REPO__")
                return re.sub(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}",
                              "__UUID__", item)
            return item

        result[str(path.relative_to(root))] = normalize(value)
    return result


def _state_differences(left, right):
    differences = {}
    for path in left.keys() | right.keys():
        def leaves(first, second, key=path):
            if isinstance(first, dict) and isinstance(second, dict):
                for child in first.keys() | second.keys():
                    leaves(first.get(child), second.get(child), key + "/" + child)
            elif first != second:
                differences[key] = (first, second)
        leaves(left.get(path), right.get(path))
    return differences


def _cases():
    process = json.loads((FIXTURES / "failure_process.json").read_text(encoding="utf-8"))
    shared = json.loads((FIXTURES / "failure.json").read_text(encoding="utf-8"))
    by_name = {case["name"]: case for case in shared["cases"]}
    cases = [("process-" + case["name"], case, by_name.get(case["name"]))
             for case in process["cases"]]
    cases += [("shared-" + case["name"], None, case) for case in shared["cases"]]
    return cases


def _seed(state, name, process_case, shared_case):
    async_initial = (process_case or {}).get("async_initial") or {}
    event = copy.deepcopy(async_initial.get("event") or (shared_case or {}).get("event") or {})
    if not event:
        suffix = "codex"
        if "start" in name:
            suffix = "codex_start"
        elif "cancel" in name:
            suffix = "codex_cancel"
        elif "result" in name or "claimed" in name:
            suffix = "codex_result"
        elif "reply" in name:
            suffix = "codex-reply"
        event = {"session_id": "dispatch-parity", "tool_use_id": name,
                 "tool_name": "mcp__codex-main__" + suffix}
    event.update(hook_event_name="PostToolUseFailure", session_id=event.get("session_id", "dispatch-parity"),
                 tool_use_id=event.get("tool_use_id", name),
                 cwd=str(ROOT), is_interrupt="interrupt" in name)
    event.setdefault("error", "fixture failure")
    event.setdefault("duration_ms", 1234)
    pending = copy.deepcopy((shared_case or {}).get("pending") or
                            ((process_case or {}).get("evidence") or {}).get("pending_call") or
                            async_initial.get("pending"))
    tool = event["tool_name"]
    if tool.endswith(("__codex_result", "__codex_cancel")):
        if async_initial:
            def expand(value):
                if isinstance(value, dict):
                    return {key: expand(item) for key, item in value.items()}
                if isinstance(value, list):
                    return [expand(item) for item in value]
                if isinstance(value, str):
                    return (value.replace("__PROJECT_KEY__", "0123456789abcdef")
                            .replace("__REPO__", str(ROOT)).replace("__ROOT__", str(state)))
                return value

            index = expand(async_initial["index"])
            pending = expand(async_initial["pending"])
            job = expand(async_initial.get("job"))
            _write(state / "async_jobs" / (index["jobId"] + ".json"), index)
            _write(state / "pending" / (index["call_key"] + ".json"), pending)
            ledger.ScopeLeases(state / "projects" / pending["project_key"]).acquire(
                pending["lease_owner"], pending["task_id"], ["src"])
            if job is not None:
                _write(state / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"), job)
            if index.get("stage") == "FINALIZING":
                evidence = expand((process_case or {}).get("evidence"))
                if evidence:
                    _write(state / "raw_tool_outputs" /
                           (index["call_key"] + "-failure.json"), evidence)
        return event
    if pending is not None and name not in {"missing_pending", "malformed_pending"}:
        pending["tool_use_id"] = event["tool_use_id"]
        pending.setdefault("task_id", "task-" + name)
        pending.setdefault("project_key", "0123456789abcdef")
        pending.setdefault("route", "normal")
        pending.setdefault("model", "gpt-fixture")
        pending.setdefault("effort", "medium")
        pending.setdefault("repo_root", str(ROOT))
        pending["lease_owner"] = _key(event)
        pending["scope"] = pending.get("scope") or ["src"]
        path = state / "pending" / (_key(event) + ".json")
        _write(path, pending)
        ledger.ScopeLeases(state / "projects" / pending["project_key"]).acquire(
            pending["lease_owner"], pending["task_id"], pending["scope"])
    elif name == "malformed_pending":
        path = state / "pending" / (_key(event) + ".json")
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("{", encoding="utf-8")
    if name == "undelivered_marker_cleanup":
        _write(state / "pending" / ("undelivered-" + _key(event) + ".json"),
               {"reason": "adapter delivery failed"})
    if "live_job_rejected" in name and pending:
        _write(state / "codex_exec_mcp" / "jobs" / (pending["job_id"] + ".json"),
               {"jobId": pending["job_id"], "call": pending.get("async_call"), "state": "RUNNING"})
    return event


def test_go_failure_dispatch_process_parity(tmp_path):
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-build-cache")
    for index, (name, process_case, shared_case) in enumerate(_cases()):
        source = tmp_path / f"seed-{index}"
        source.mkdir()
        event = _seed(source, name.removeprefix("process-").removeprefix("shared-"), process_case, shared_case)
        python_state, go_state = tmp_path / f"python-{index}", tmp_path / f"go-{index}"
        shutil.copytree(source, python_state)
        shutil.copytree(source, go_state)
        payload = json.dumps(event).encode("utf-8")
        python_env = env | {"PURSER_STATE_DIR": str(python_state)}
        python = subprocess.run([sys.executable, str(ORCH / "hook.py"), "enforce"], cwd=ROOT,
                                env=python_env, input=payload, capture_output=True, timeout=45, check=False)
        go_env = env | {"PURSER_STATE_DIR": str(go_state)}
        go = subprocess.run(["go", "run", "./cmd/purser-hook", "enforce"], cwd=ROOT / "go",
                            env=go_env, input=payload, capture_output=True, timeout=90, check=False)
        assert python.returncode == go.returncode == 0, (name, python.stdout, python.stderr, go.stdout, go.stderr)
        def normalize_output(value, state):
            if isinstance(value, dict):
                return {key: normalize_output(item, state) for key, item in value.items()}
            if isinstance(value, list):
                return [normalize_output(item, state) for item in value]
            if isinstance(value, str):
                return value.replace(str(state), "__STATE__")
            return value

        python_output = normalize_output(json.loads(python.stdout), python_state)
        go_output = normalize_output(json.loads(go.stdout), go_state)
        assert python_output == go_output, (name, python_output, go_output,
                                            python.stderr.decode(errors="replace"), go.stderr.decode(errors="replace"))
        python_files, go_files = _snapshot(python_state), _snapshot(go_state)
        differences = _state_differences(python_files, go_files)
        assert not differences, f"{name}: {differences}"
