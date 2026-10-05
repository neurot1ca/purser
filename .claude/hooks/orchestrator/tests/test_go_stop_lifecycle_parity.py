"""Capture registered Python Stop lifecycle behavior for the Go parity test."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from contract_hook_runner import run_contract_hook

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "contextio" / "testdata" / "stop_lifecycle_python_fixture.json"
sys.path.insert(0, str(ORCH))
import claude_transcript_usage as usage


def _records(*rows):
    return "".join(json.dumps(row, separators=(",", ":")) + "\n" for row in rows)


def _snapshot(state, repo, session):
    key = hashlib.sha256(os.path.normcase(str(repo.resolve())).encode()).hexdigest()[:16]
    advisor_path = state / "projects" / key / "telemetry" / "compaction_advisor.json"
    advisor = json.loads(advisor_path.read_text()) if advisor_path.exists() else {}
    active_path = state / "active_tasks" / f"{session}.json"
    active = json.loads(active_path.read_text()) if active_path.exists() else None
    active_snapshot = dict(active) if active else None
    if active_snapshot:
        active_snapshot["canonical_transcript_path"] = "<TRANSCRIPT>"
        active_snapshot["created_at"] = "<CREATED_AT>"
        active_snapshot["project_key"] = "<PROJECT_KEY>"
        for field in ("transcript_dev", "transcript_ino", "transcript_ctime_ns"):
            active_snapshot.pop(field, None)
    event_path = state / "telemetry" / "events" / "by_task" / f"{active.get('task_id')}.jsonl" if active else None
    task_event_count = 0
    if event_path and event_path.exists():
        task_event_count = sum(json.loads(line).get("event_type") == "CLAUDE_TASK_USAGE_RECORDED"
                               for line in event_path.read_text(encoding="utf-8").splitlines())
    return {"retry_count": advisor.get("codex_retry_count", 0),
            "retry_pending": advisor.get("codex_retry_pending", False),
            "active_completed": active.get("completed") if active else None,
            "active_invalidated": active.get("invalidated") if active else None,
            "active_reason": active.get("invalidated_reason") if active else None,
            "advisor_state": advisor, "active_state": active_snapshot,
            "task_event_count": task_event_count}


def test_p132_python_stop_lifecycle_fixture(tmp_path, monkeypatch):
    writing = os.environ.get("STOP_LIFECYCLE_WRITE_FIXTURE") == "1"
    fixture = {"oracle": "registered Python codex.stop + compaction_advisor.stop", "cases": []}
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True, timeout=10)
    (repo / "README.md").write_text("p132 fixture baseline\n", encoding="utf-8")
    subprocess.run(["git", "add", "README.md"], cwd=repo, check=True, timeout=10)
    git_env = {**os.environ, "GIT_AUTHOR_NAME": "Purser Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
               "GIT_COMMITTER_NAME": "Purser Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
               "GIT_AUTHOR_DATE": "1700000000 +0000", "GIT_COMMITTER_DATE": "1700000000 +0000"}
    subprocess.run(["git", "commit", "-qm", "fixture baseline"], cwd=repo, env=git_env, check=True, timeout=10)
    user = tmp_path / "user"
    projects = user / ".claude" / "projects"
    projects.mkdir(parents=True)
    monkeypatch.setenv("USERPROFILE", str(user))
    monkeypatch.setenv("HOME", str(user))
    monkeypatch.setenv("CLAUDE_PLUGIN_ROOT", str(ROOT))
    denial = _records(
        {"type": "user", "message": {"content": "Try Codex."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "call-1",
          "name": "mcp__plugin_purser_codex-main__codex_start", "input": {}}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "call-1",
          "is_error": True, "content": "classifier denied permission"}]}})

    def exercise(name, events, transcript, *, consume_retry=False):
        state = tmp_path / "state" / name
        session = "p132-" + name
        transcript_path = projects / f"{session}.jsonl"
        if transcript is not None:
            transcript_path.write_text(transcript, encoding="utf-8")
        payloads = []
        outputs = []
        active_seed = None
        if name == "active-stop":
            state.mkdir(parents=True)
            transcript_path.write_text(_records({"type": "user", "message": {"content": "Continue."}}), encoding="utf-8")
            event = {"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex_start",
                     "tool_use_id": "p132-task-tool", "session_id": session, "cwd": str(repo),
                     "transcript_path": str(transcript_path), "tool_input": {"mode": "new", "label": "P132 task",
                     "timeoutSeconds": 60, "prompt": "ORCH|task=p132-task|phase=implementation|route=subtle|scope=README.md\nImplement the scoped change."}}
            for key, value in {"PURSER_STATE_DIR": str(state), "CODEX_HOME": str(tmp_path / "codex")}.items():
                monkeypatch.setenv(key, value)
            pre = run_contract_hook("PreToolUse", event, state, entrypoint="enforcement")
            assert pre.returncode == 0, pre.stderr.decode("utf-8", errors="replace")
            active_path = state / "active_tasks" / f"{session}.json"
            active_seed = json.loads(active_path.read_text(encoding="utf-8"))
            transcript_path.write_text(transcript or "", encoding="utf-8")
            active_seed["canonical_transcript_path"] = "<TRANSCRIPT>"
            active_seed["created_at"] = "<CREATED_AT>"
            active_seed["project_key"] = "<PROJECT_KEY>"
            for field in ("transcript_dev", "transcript_ino", "transcript_ctime_ns"):
                active_seed.pop(field, None)
        for event in events:
            payload = {"hook_event_name": "Stop", "cwd": str(repo), "last_assistant_message": "Done.", **event}
            if event.get("with_transcript", True) and transcript is not None:
                payload["transcript_path"] = str(transcript_path)
            else:
                payload.pop("with_transcript", None)
            # Persist exact hook input without the test-only field.
            payload.pop("with_transcript", None)
            saved_payload = dict(payload)
            saved_payload["cwd"] = "<REPO>"
            if "transcript_path" in saved_payload:
                saved_payload["transcript_path"] = "<TRANSCRIPT>"
            payloads.append(saved_payload)
            enforcement = run_contract_hook("Stop", payload, state, entrypoint="enforcement")
            context = run_contract_hook("Stop", payload, state, entrypoint="context")
            assert enforcement.returncode == context.returncode == 0
            outputs.append(json.loads(context.stdout) if context.stdout else {})
        before_prompt = _snapshot(state, repo, session)
        prompt_output = None
        after_prompt = None
        if consume_retry:
            prompt = {"hook_event_name": "UserPromptSubmit", "cwd": str(repo), "session_id": session,
                      "prompt": "Continue."}
            result = run_contract_hook("UserPromptSubmit", prompt, state, entrypoint="context")
            assert result.returncode == 0
            prompt_output = json.loads(result.stdout) if result.stdout else {}
            after_prompt = _snapshot(state, repo, session)
        actual_transcript = transcript_path.read_bytes().decode("utf-8") if transcript is not None else None
        fixture["cases"].append({"name": name, "session": session, "transcript": actual_transcript,
                                 "events": payloads, "outputs": outputs, "snapshot": before_prompt,
                                 "prompt_output": prompt_output, "after_prompt": after_prompt,
                                 "active_seed": active_seed})

    exercise("event-mapping", [{"session_id": "p132-event-mapping", "stop_hook_active": False},
                                {"session_id": "p132-event-mapping", "stop_hook_active": True},
                                {"stop_hook_active": False}], None)
    exercise("missing-transcript", [{"session_id": "p132-missing-transcript", "stop_hook_active": False}], None)
    exercise("active-stop", [{"session_id": "p132-active-stop", "stop_hook_active": False},
                             {"session_id": "p132-active-stop", "stop_hook_active": True}],
             _records({"type": "user", "message": {"content": "Continue."}},
                      {"type": "assistant", "requestId": "p132-task-request", "message": {
                          "id": "p132-task-message", "content": [{"type": "tool_use", "id": "p132-task-tool"}],
                          "usage": {"input_tokens": 7, "output_tokens": 3,
                                    "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2}}}))
    exercise("retry-three-stops", [{"session_id": "p132-retry-three-stops", "stop_hook_active": False}] * 3,
             denial, consume_retry=True)

    if writing:
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
    saved = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture == saved
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-build-cache")
    result = subprocess.run(["go", "test", "./internal/contextio/", "-run", "^TestP132StopLifecyclePythonParity$", "-count=1"],
                            cwd=ROOT / "go", text=True, capture_output=True, timeout=180, env=env)
    assert result.returncode == 0, result.stdout + result.stderr
