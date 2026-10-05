"""Registered Python Stop composition oracle for the Go Stop entry point."""
import json
import hashlib
import os
from pathlib import Path
import subprocess

from contract_hook_runner import run_contract_hook
import claude_transcript_usage as usage

PACKAGE = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).parents[4]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "stop_compose.json"


def _records(*items):
    return "".join(json.dumps(item, ensure_ascii=False) + "\n" for item in items)


def test_p131_registered_python_stop_composition(tmp_path, monkeypatch):
    writing = os.environ.get("STOP_COMPOSE_WRITE_FIXTURE") == "1"
    fixture = {"oracle": "registered Python codex.stop + compaction_advisor.stop", "cases": []}
    expected_cases = None if writing else json.loads(FIXTURE.read_text(encoding="utf-8"))["cases"]
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True, timeout=10)
    userprofile = tmp_path / "user"
    projects = userprofile / ".claude" / "projects"
    projects.mkdir(parents=True)
    monkeypatch.setenv("USERPROFILE", str(userprofile))

    job = "a" * 64
    denial = _records(
        {"type": "user", "message": {"content": "Try Codex."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "call-1",
            "name": "mcp__plugin_purser_codex-main__codex_start", "input": {}}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "call-1",
            "is_error": True, "content": "classifier denied permission"}]}})
    flow = _records(
        {"type": "user", "message": {"content": "Continue."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "result-1",
            "name": "mcp__plugin_purser_codex-main__codex_result", "input": {}}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "result-1",
            "content": "Finished"}]}},
        {"type": "assistant", "message": {"content": "Should I continue?"}})
    plain = _records(
        {"type": "user", "message": {"content": "Continue."}},
        {"type": "assistant", "message": {"content": "Done."}})

    def run_case(name, transcript_text, *, active=None, contention=False):
        state = tmp_path / "state" / name
        session = "p131-" + name
        transcript = projects / (session + ".jsonl")
        if active:
            transcript_text += _records({"type": "assistant", "requestId": "task-request",
                "message": {"id": "task-message", "content": [{"type": "tool_use", "id": "task-tool"}],
                "usage": {"input_tokens": 7, "output_tokens": 3,
                          "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2}}})
        transcript.write_text(transcript_text, encoding="utf-8")
        active_path = state / "active_tasks" / (session + ".json")
        if active:
            active_path.parent.mkdir(parents=True, exist_ok=True)
            started = usage.start_active_task(state, session_id=session, task_id="task-" + name,
                project_key="0123456789abcdef", tool_use_id="task-tool", transcript_path=transcript,
                allowed_projects_root=projects)
            assert started["status"] == "STARTED", started
            record = json.loads(active_path.read_text(encoding="utf-8"))
            if active == "identity-failure":
                record["canonical_transcript_path"] = str(projects / "wrong.jsonl")
                active_path.write_text(json.dumps(record), encoding="utf-8")

        payload = {"hook_event_name": "Stop", "cwd": str(repo), "session_id": session,
                   "stop_hook_active": False, "last_assistant_message": "Done.",
                   "transcript_path": str(transcript)}
        if contention:
            lock_state = state / "active_tasks" / ".locks"
            lock_state.mkdir(parents=True, exist_ok=True)
            with usage.session_lock(state, session, timeout_seconds=1) as acquired:
                assert acquired
                # Execute the actual registered handlers while the finalizer lock is held.
                enforcement = run_contract_hook("Stop", payload, state, entrypoint="enforcement")
                context = run_contract_hook("Stop", payload, state, entrypoint="context")
        else:
            enforcement = run_contract_hook("Stop", payload, state, entrypoint="enforcement")
            context = run_contract_hook("Stop", payload, state, entrypoint="context")
        assert enforcement.returncode == context.returncode == 0
        decision = json.loads(context.stdout) if context.stdout else {}
        project_key = hashlib.sha256(os.path.normcase(str(repo.resolve())).encode("utf-8")).hexdigest()[:16]
        advisor_path = state / "projects" / project_key / "telemetry" / "compaction_advisor.json"
        advisor = json.loads(advisor_path.read_text(encoding="utf-8")) if advisor_path.exists() else {}
        final_active = json.loads(active_path.read_text(encoding="utf-8")) if active_path.exists() else None
        observed = {
            "decision": decision.get("decision", "allow"),
            "reason": decision.get("reason", ""),
            "retry_count": advisor.get("codex_retry_count", 0),
            "active_completed": final_active.get("completed") if final_active else None,
            "active_invalidated": final_active.get("invalidated") if final_active else None,
            "active_reason": final_active.get("invalidated_reason") if final_active else None,
            "contention": (state / "active_tasks" / (session + ".contention")).exists(),
        }
        if writing:
            fixture["cases"].append({"name": name, "expected": observed})
        else:
            expected = next(item["expected"] for item in expected_cases if item["name"] == name)
            assert observed == expected, name

    run_case("plain-allow", plain)
    run_case("retry-block", denial)
    run_case("flow-question-block", flow)
    run_case("finalization-failure-isolated", plain, active="identity-failure")
    run_case("finalization-success-retry-block", denial, active="success")
    run_case("concurrent-finalization-contention", plain, active="success", contention=True)
    if writing:
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
    else:
        assert fixture["oracle"] == json.loads(FIXTURE.read_text(encoding="utf-8"))["oracle"]
